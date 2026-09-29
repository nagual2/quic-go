package io_uring

// Benchmarks comparing the three UDP send strategies for the QUIC send path
// on loopback, 64 KiB GSO super-buffers (= ~48 datagrams of 1350 bytes each):
//
//   Sendto          - one syscall per datagram (no batching)
//   WriteBatchGSO   - x/net WriteBatch + UDP_SEGMENT (the current quic-go path)
//   IOURingGSO      - io_uring SENDMSG batch, one submit per ring cycle
//
// The receiver drains at full speed so the kernel's receive buffer never
// throttles the sender.

import (
	"net"
	"testing"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const (
	superBufLen = 64000 // one GSO super-buffer
	gsoSize     = 1350      // nominal datagram size
	batchLen    = 64        // datagrams/messages per batching unit
	rxBuffer    = 16 << 20  // receiver socket buffer: absorb the flood
)

func newBenchUDP(b *testing.B) (sender *net.UDPConn, receiver *net.UDPConn) {
	b.Helper()
	rx, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatal(err)
	}
	_ = rx.SetReadBuffer(rxBuffer)
	tx, err := net.DialUDP("udp", nil, rx.LocalAddr().(*net.UDPAddr))
	if err != nil {
		b.Fatal(err)
	}
	_ = tx.SetWriteBuffer(16 << 20)

	done := make(chan struct{})
	go func() {
		buf := make([]byte, maxPayload)
		for {
			if _, err := rx.Read(buf); err != nil {
				close(done)
				return
			}
		}
	}()
	b.Cleanup(func() {
		rx.Close()
		<-done
	})
	return tx, rx
}

func iovecFor(p []byte) unix.Iovec {
	iov := unix.Iovec{Base: &p[0]}
	iov.SetLen(len(p))
	return iov
}

// one syscall per datagram
func BenchmarkSendto(b *testing.B) {
	tx, _ := newBenchUDP(b)
	f, err := tx.File()
	if err != nil {
		b.Fatal(err)
	}
	fd := int(f.Fd())
	super := make([]byte, superBufLen)
	dgram := make([]byte, gsoSize)
	b.SetBytes(int64(superBufLen))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < batchLen; j++ {
			copy(dgram, super)
			if err := unix.Sendto(fd, dgram, 0, nil); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// one syscall per 64 KiB jumbo datagram (loopback / jumbo-MTU paths only)
func BenchmarkSendtoJumbo(b *testing.B) {
	tx, _ := newBenchUDP(b)
	f, err := tx.File()
	if err != nil {
		b.Fatal(err)
	}
	fd := int(f.Fd())
	super := make([]byte, superBufLen)
	b.SetBytes(int64(superBufLen))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := unix.Sendto(fd, super, 0, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// x/net WriteBatch + UDP_SEGMENT: the current quic-go send path
func BenchmarkWriteBatchGSO(b *testing.B) {
	tx, _ := newBenchUDP(b)
	pc := ipv4.NewPacketConn(tx)
	control := gsoControl(gsoSize)

	super := make([]byte, superBufLen)
	msghdrs := make([]ipv4.Message, batchLen)
	for i := range msghdrs {
		msghdrs[i].Buffers = [][]byte{super}
		msghdrs[i].OOB = control
	}
	b.SetBytes(int64(superBufLen) * int64(batchLen))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := pc.WriteBatch(msghdrs, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// io_uring SENDMSG batch, one enter per ring cycle
func BenchmarkIOURingGSO(b *testing.B) {
	tx, _ := newBenchUDP(b)
	f, err := tx.File()
	if err != nil {
		b.Fatal(err)
	}
	s, err := NewSender(int(f.Fd()), gsoSize)
	if err != nil {
		b.Skipf("io_uring unavailable: %v", err)
	}
	defer s.Close()

	supers := make([][]byte, ringEntries)
	for i := range supers {
		supers[i] = make([]byte, superBufLen)
	}
	b.SetBytes(int64(superBufLen) * int64(ringEntries))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, super := range supers {
			if err := s.Submit(super); err != nil {
				b.Fatal(err)
			}
		}
		if err := s.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}

var _ = net.IPv4len
