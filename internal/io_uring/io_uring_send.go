//go:build linux

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
	"net"
	"unsafe"
)

const (
	ringEntries = 64
)

// Sender submits UDP payloads through an io_uring instance. Every payload is
// handed to the kernel as a single sendmsg with caller-provided destination
// address and control messages (e.g. UDP_SEGMENT for GSO), so one
// io_uring_enter covers a whole batch of sends.
//
// The caller must keep every payload and control buffer passed to Submit
// alive until the next Flush returns.
type Sender struct {
	ring     *rawRing
	fd       int
	msgs     [ringEntries]msghdrRaw
	iovs     [ringEntries]iovecRaw
	addrs    [ringEntries][28]byte // sockaddr_in6-shaped storage, large enough for sockaddr_in
	pending  uint32
}

func NewSender(fd int) (*Sender, error) {
	ring, err := newRawRing(ringEntries)
	if err != nil {
		return nil, err
	}
	return &Sender{ring: ring, fd: fd}, nil
}

func (s *Sender) Close() error {
	return s.ring.Close()
}

// Submit enqueues a single sendmsg on the ring. addr may be nil for connected
// sockets; control carries marshalled cmsgs (e.g. UDP_SEGMENT) and may be
// nil. Call Flush once the batch is complete.
func (s *Sender) Submit(data []byte, addr *net.UDPAddr, control []byte) error {
	if s.pending >= ringEntries {
		return fmt.Errorf("submission queue full (%d entries)", ringEntries)
	}
	if len(data) == 0 {
		return fmt.Errorf("empty payload")
	}
	slot := s.pending
	s.iovs[slot] = iovecRaw{Base: unsafe.Pointer(&data[0]), Len: uint64(len(data))}
	m := &s.msgs[slot]
	*m = msghdrRaw{Iov: &s.iovs[slot], IovLen: 1}
	if addr != nil {
		m.Name = unsafe.Pointer(&s.addrs[slot])
		m.NameLen = packSockaddr(&s.addrs[slot], addr)
	}
	if len(control) > 0 {
		m.Control = unsafe.Pointer(&control[0])
		m.ControlLen = uint64(len(control))
	}
	if _, err := s.ring.prepareSendMsg(s.fd, unsafe.Pointer(m)); err != nil {
		return err
	}
	s.pending++
	return nil
}

// Flush submits all pending SQEs and waits for their completions. It drains
// the completion queue fully and returns the first error result, so a single
// failed send does not stall the ring.
func (s *Sender) Flush() error {
	pending := s.pending
	s.pending = 0
	if err := s.ring.enter(pending, pending); err != nil {
		return err
	}
	_, err := s.ring.reapCQEs(pending)
	return err
}

// packSockaddr writes addr into buf as a sockaddr_in or sockaddr_in6 and
// returns the value for msghdr.msg_namelen.
func packSockaddr(buf *[28]byte, addr *net.UDPAddr) uint32 {
	if ip4 := addr.IP.To4(); ip4 != nil {
		// AF_INET, little-endian sa_family on x86_64
		buf[0], buf[1] = 2, 0
		binary.BigEndian.PutUint16(buf[2:4], uint16(addr.Port))
		copy(buf[4:8], ip4)
		return 16
	}
	// AF_INET6; flowinfo and scope_id are left zero
	buf[0], buf[1] = 10, 0
	binary.BigEndian.PutUint16(buf[2:4], uint16(addr.Port))
	copy(buf[8:24], addr.IP.To16())
	return 28
}
