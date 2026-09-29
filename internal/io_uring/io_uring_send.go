// Package io_uring contains an experimental io_uring-based UDP send path for
// the QUIC connection. It is the seed of the io_uring send-queue: one
// io_uring_enter covers a whole batch of GSO-segmented packets, replacing
// one syscall per batch with one syscall per ring cycle.
//
// x/sys/unix does not ship io_uring bindings, so the required bindings live
// in io_uring_raw.go (x86_64, kernel >= 5.11).
package io_uring

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	maxPayload  = 64 * 1024
	ringEntries = 64

	udpSegmentCmsgType = 0x67 // UDP_SEGMENT
)

// Sender submits UDP payloads through an io_uring instance. When gsoSize is
// non-zero, each payload is handed to the kernel as a single UDP_SEGMENT-
// tagged sendmsg, which the kernel splits into gsoSize-sized datagrams - the
// io_uring equivalent of the GSO path in quic-go's oobConn.
//
// The caller must keep every payload passed to Submit alive until the next
// Flush returns.
type Sender struct {
	ring     *rawRing
	fd       int
	gsoSize  uint16
	msgs     [ringEntries]msghdrRaw
	iovs     [ringEntries]iovecRaw
	controls [ringEntries][]byte
	pending  uint32
}

func NewSender(fd int, gsoSize uint16) (*Sender, error) {
	ring, err := newRawRing(ringEntries)
	if err != nil {
		return nil, err
	}
	s := &Sender{ring: ring, fd: fd, gsoSize: gsoSize}
	for i := range s.controls {
		s.controls[i] = gsoControl(gsoSize)
	}
	return s, nil
}

func gsoControl(gsoSize uint16) []byte {
	control := make([]byte, unix.CmsgSpace(2))
	cmsg := (*unix.Cmsghdr)(unsafe.Pointer(&control[0]))
	cmsg.Level = unix.IPPROTO_UDP
	cmsg.Type = udpSegmentCmsgType
	cmsg.SetLen(unix.CmsgLen(2))
	binary.LittleEndian.PutUint16(control[unix.CmsgLen(2):], gsoSize)
	return control
}

func (s *Sender) Close() error {
	return s.ring.Close()
}

// Submit enqueues a single GSO payload on the ring. Call Flush once the ring
// is full; payloads must stay alive until that Flush returns.
func (s *Sender) Submit(data []byte) error {
	if s.pending >= ringEntries {
		return fmt.Errorf("submission queue full (%d entries)", ringEntries)
	}
	if len(data) > maxPayload {
		return fmt.Errorf("payload %d exceeds %d", len(data), maxPayload)
	}
	slot := s.pending
	s.iovs[slot] = iovecRaw{Base: unsafe.Pointer(&data[0]), Len: uint64(len(data))}
	s.msgs[slot] = msghdrRaw{Iov: &s.iovs[slot], IovLen: 1}
	if s.gsoSize != 0 {
		s.msgs[slot].Control = unsafe.Pointer(&s.controls[slot][0])
		s.msgs[slot].ControlLen = uint64(len(s.controls[slot]))
	}
	if _, err := s.ring.prepareSendMsg(s.fd, unsafe.Pointer(&s.msgs[slot])); err != nil {
		return err
	}
	s.pending++
	return nil
}

// Flush submits all pending SQEs and waits for their completions.
func (s *Sender) Flush() error {
	pending := s.pending
	s.pending = 0
	if err := s.ring.enter(pending, pending); err != nil {
		return err
	}
	_, err := s.ring.reapCQEs(pending)
	return err
}
