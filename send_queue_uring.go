//go:build linux

package quic

// uringSendQueue is an experimental send queue that batches queued packets
// into io_uring SENDMSG operations: one io_uring_enter per batch of up to 64
// packets instead of one sendmsg syscall per packet. It is enabled with
// QUIC_GO_IO_URING_SEND=1 and degrades to the plain conn.Write path whenever
// the ring is unavailable or fails.

import (
	"errors"
	"net"
	"os"
	"strconv"
	"syscall"

	"github.com/quic-go/quic-go/internal/protocol"

	"github.com/quic-go/quic-go/internal/io_uring"
)

const uringBatchLen = 64 // must not exceed io_uring.ringEntries

func newIOUringSendQueue(conn sendConn) sender {
	if enabled, _ := strconv.ParseBool(os.Getenv("QUIC_GO_IO_URING_SEND")); !enabled {
		return nil
	}
	q := &uringSendQueue{
		conn:        conn,
		runStopped:  make(chan struct{}),
		closeCalled: make(chan struct{}),
		available:   make(chan struct{}, 1),
		queue:       make(chan queueEntry, sendQueueCapacity),
		sender:      newRingSender(conn),
		controls:    make([][]byte, uringBatchLen),
	}
	for i := range q.controls {
		q.controls[i] = make([]byte, 0, 96)
	}
	return q
}

// newRingSender builds the io_uring sender on the connection's socket file
// descriptor, or nil when that is not possible (non-sconn connection, no
// syscall conn, no io_uring support).
func newRingSender(conn sendConn) *io_uring.Sender {
	sc, ok := conn.(*sconn)
	if !ok {
		return nil
	}
	sysc, ok := sc.rawConn.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return nil
	}
	raw, err := sysc.SyscallConn()
	if err != nil {
		return nil
	}
	var fd int
	if err := raw.Control(func(f uintptr) { fd = int(f) }); err != nil {
		return nil
	}
	s, err := io_uring.NewSender(fd)
	if err != nil {
		return nil
	}
	return s
}

type uringSendQueue struct {
	queue       chan queueEntry
	closeCalled chan struct{} // closed when Close() is called
	runStopped  chan struct{} // closed when the run loop returns
	available   chan struct{}
	conn        sendConn
	sender      *io_uring.Sender
	controls    [][]byte // per-slot control message scratch, alive until Flush returns
}

var _ sender = &uringSendQueue{}

func (h *uringSendQueue) Send(p *packetBuffer, gsoSize uint16, ecn protocol.ECN) {
	select {
	case h.queue <- queueEntry{buf: p, gsoSize: gsoSize, ecn: ecn}:
		if len(h.queue) == sendQueueCapacity {
			select {
			case <-h.available:
			default:
			}
		}
	case <-h.runStopped:
	default:
		panic("uringSendQueue.Send would have blocked")
	}
}

func (h *uringSendQueue) SendProbe(p *packetBuffer, addr net.Addr, info packetInfo) {
	h.conn.WriteTo(p.Data, addr, info)
}

func (h *uringSendQueue) WouldBlock() bool {
	return len(h.queue) == sendQueueCapacity
}

func (h *uringSendQueue) Available() <-chan struct{} {
	return h.available
}

func (h *uringSendQueue) Run() error {
	defer close(h.runStopped)
	var shouldClose bool
	var batch [uringBatchLen]queueEntry
	for {
		if shouldClose && len(h.queue) == 0 {
			h.disable()
			return nil
		}
		select {
		case <-h.closeCalled:
			h.closeCalled = nil // prevent this case from being selected again
			// make sure that all queued packets are actually sent out
			shouldClose = true
		case batch[0] = <-h.queue:
			n := 1
		drain:
			for n < uringBatchLen {
				select {
				case batch[n] = <-h.queue:
					n++
				default:
					break drain
				}
			}
			if err := h.sendBatch(batch[:n]); err != nil {
				// This additional check enables:
				// 1. Checking for "datagram too large" message from the kernel, as such,
				// 2. Path MTU discovery, and
				// 3. Eventual detection of loss PingFrame.
				if !isSendMsgSizeErr(err) {
					return err
				}
			}
			for _, e := range batch[:n] {
				e.buf.Release()
				select {
				case h.available <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (h *uringSendQueue) Close() {
	close(h.closeCalled)
	// wait until the run loop returned
	<-h.runStopped
}

func (h *uringSendQueue) sendBatch(batch []queueEntry) error {
	if h.sender == nil {
		return h.writeBatchFallback(batch)
	}
	udpAddr, _ := h.conn.RemoteAddr().(*net.UDPAddr)
	submitted := 0
	var submitErr error
	for i, e := range batch {
		if err := h.sender.Submit(e.buf.Data, udpAddr, h.controlFor(i, e)); err != nil {
			submitErr = err
			break
		}
		submitted++
	}
	// flush whatever was submitted; reapCQEs drains the whole ring, so a
	// single failed send does not stall it
	flushErr := h.sender.Flush()
	if flushErr != nil && !isSendMsgSizeErr(flushErr) {
		// the ring's usefulness is now unknown: degrade permanently
		h.disable()
	}
	if submitErr != nil || (flushErr != nil && !isSendMsgSizeErr(flushErr)) {
		// re-send the whole batch through the fallback path; duplicate
		// datagrams are safe for QUIC (deduplicated by packet number)
		return errors.Join(submitErr, h.writeBatchFallback(batch))
	}
	return flushErr
}

// controlFor builds the control messages for the i-th slot of the current
// batch: the packet info prefix, then UDP_SEGMENT and the ECN marking. The
// scratch buffer is per-slot, since the ring reads it only at Flush time.
func (h *uringSendQueue) controlFor(i int, e queueEntry) []byte {
	sc, ok := h.conn.(*sconn)
	if !ok {
		return nil
	}
	oob := append(h.controls[i][:0], sc.remoteAddrInfo.Load().oob...)
	if e.gsoSize > 0 {
		oob = appendUDPSegmentSizeMsg(oob, e.gsoSize)
	}
	if e.ecn != protocol.ECNUnsupported {
		isV4 := true
		if udpAddr, ok := h.conn.RemoteAddr().(*net.UDPAddr); ok {
			isV4 = udpAddr.IP.To4() != nil
		}
		if isV4 {
			oob = appendIPv4ECNMsg(oob, e.ecn)
		} else {
			oob = appendIPv6ECNMsg(oob, e.ecn)
		}
	}
	return oob
}

func (h *uringSendQueue) writeBatchFallback(batch []queueEntry) error {
	var firstErr error
	for _, e := range batch {
		if err := h.conn.Write(e.buf.Data, e.gsoSize, e.ecn); err != nil && !isSendMsgSizeErr(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (h *uringSendQueue) disable() {
	if h.sender != nil {
		h.sender.Close()
		h.sender = nil
	}
}
