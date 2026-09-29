package io_uring

// Minimal raw io_uring bindings (x86_64, kernel >= 5.11): setup, ring mmap,
// SENDMSG preparation and enter/wait. x/sys/unix does not ship io_uring
// bindings, so the send path carries its own.

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	sysIoUringSetup = 425
	sysIoUringEnter = 426

	enterGetEvents = 0x1

	offSqRing = 0
	offCqRing = 0x8000000
	offSqes   = 0x10000000

	opSendMsg = 9

	sqeSize    = 64
	ringResvU3 = 3 // __u32 resv[3] after wq_fd
)

type ioUringParams struct {
	SqEntries    uint32
	CqEntries    uint32
	Flags        uint32
	SqThreadCpu  uint32
	SqThreadIdle uint32
	Features     uint32
	WqFd         uint32
	Resv1        uint32
	Resv2        uint32
	Resv3        uint32
	SqOff        ioUringSqOff
	CqOff        ioUringCqOff
}

type ioUringSqOff struct {
	Head, Tail, RingMask, RingEntries, Flags, Dropped, Array, Resv1, Resv2 uint32
}

type ioUringCqOff struct {
	Head, Tail, RingMask, RingEntries, Overflow, Cqes, Flags, Resv1 uint32
}

type ioUringSQE struct {
	Opcode   uint8
	Flags    uint8
	Ioprio   uint16
	Fd       int32
	Off      uint64
	Addr     uint64
	Len      uint32
	RwFlags  uint32
	UserData uint64
	_        [16]byte
}

type rawRing struct {
	fd         int
	sqRing     []byte
	cqRing     []byte
	sqes       []byte
	sqMask     uint32 // ring mask VALUE, read from the mapped ring header
	cqMask     uint32
	sqArrayOff uint32 // byte offset of the SQ index array inside sqRing
	cqesOff    uint32 // byte offset of the CQE array inside cqRing
	sqTail     *uint32
	sqHead     *uint32
	cqTail     *uint32
	cqHead     *uint32
}

func mmapRing(fd int, off, size int64) []byte {
	b, err := syscall.Mmap(int(fd), off, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		panic(fmt.Sprintf("io_uring mmap(off=%#x): %v", off, err))
	}
	return b
}

// cqLayout holds the byte offsets of the CQ ring fields inside the CQ mmap.
// The kernel-reported cq_off can diverge from the actual mmap content:
// 6.18.33.2-MS-WSL2 reports the 6.12 padded template {head 0, tail 8,
// mask 12, entries 20, cqes 44} while the mapped region exposes the unified
// io_rings layout {head 8, tail 12, mask 20, entries 28, cqes 64}.
// Cross-check: the reported sq_off.array (64 + cq_entries*16) confirms the
// unified cqes placement. The layout is resolved at setup time by matching
// the entries/mask values the kernel wrote into the mapping.
type cqLayout struct {
	head, tail, mask, entries, cqes uint32
}

func resolveCqLayout(cqRing []byte, cqEntriesWant uint32, reported cqLayout) (uint32, cqLayout, error) {
	u32 := func(off uint32) uint32 { return *(*uint32)(unsafe.Pointer(&cqRing[off])) }
	if u32(reported.entries) == cqEntriesWant {
		return cqEntriesWant, reported, nil
	}
	unified := cqLayout{head: 8, tail: 12, mask: 20, entries: 28, cqes: 64}
	if u32(unified.entries) == cqEntriesWant && u32(unified.mask) == cqEntriesWant-1 {
		return cqEntriesWant, unified, nil
	}
	return 0, cqLayout{}, fmt.Errorf(
		"unknown CQ ring layout: entries@%d=%d entries@28=%d mask@%d=%d mask@20=%d, want %d",
		reported.entries, u32(reported.entries), u32(28),
		reported.mask, u32(reported.mask), u32(20), cqEntriesWant)
}

func newRawRing(entries uint32) (*rawRing, error) {
	var params ioUringParams
	fd, _, errno := syscall.Syscall(sysIoUringSetup, uintptr(entries), uintptr(unsafe.Pointer(&params)), 0)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}
	if fd > 1<<30 {
		return nil, fmt.Errorf("io_uring_setup: bad fd %d", int(fd))
	}
	r := &rawRing{fd: int(fd)}

	sqSize := int64(params.SqOff.Array) + int64(params.SqEntries)*4
	// map enough for both the reported and the unified cqes placement
	cqSize := int64(params.CqOff.Cqes) + int64(params.CqEntries)*16
	if unified := 64 + int64(params.CqEntries)*16; unified > cqSize {
		cqSize = unified
	}
	r.sqRing = mmapRing(r.fd, offSqRing, sqSize)
	r.cqRing = mmapRing(r.fd, offCqRing, cqSize)
	r.sqes = mmapRing(r.fd, offSqes, int64(params.SqEntries)*sqeSize)

	// TRAP: params.SqOff.* are byte OFFSETS into the mmap (on kernels >= 6.12
	// the header is padded for atomics: mask lives at offset 16, entries at
	// 24) - the mask/entries VALUES must be read from the mapped ring header.
	sqEntries := *(*uint32)(unsafe.Pointer(&r.sqRing[params.SqOff.RingEntries]))
	if sqEntries != params.SqEntries {
		return nil, fmt.Errorf("SQ ring layout mismatch: entries@%d=%d, want %d",
			params.SqOff.RingEntries, sqEntries, params.SqEntries)
	}
	cqEntries, cq, err := resolveCqLayout(r.cqRing, params.CqEntries, cqLayout{
		head:    params.CqOff.Head,
		tail:    params.CqOff.Tail,
		mask:    params.CqOff.RingMask,
		entries: params.CqOff.RingEntries,
		cqes:    params.CqOff.Cqes,
	})
	if err != nil {
		return nil, err
	}
	// rings are always sized to a power of two: mask = entries - 1
	r.sqMask = sqEntries - 1
	r.cqMask = cqEntries - 1

	// the application owns the SQ index array: identity mapping
	for i := uint32(0); i < params.SqEntries; i++ {
		*(*uint32)(unsafe.Pointer(&r.sqRing[params.SqOff.Array+4*i])) = i
	}

	r.sqTail = (*uint32)(unsafe.Pointer(&r.sqRing[params.SqOff.Tail]))
	r.sqHead = (*uint32)(unsafe.Pointer(&r.sqRing[params.SqOff.Head]))
	r.cqTail = (*uint32)(unsafe.Pointer(&r.cqRing[cq.tail]))
	r.cqHead = (*uint32)(unsafe.Pointer(&r.cqRing[cq.head]))
	r.sqArrayOff = params.SqOff.Array
	r.cqesOff = cq.cqes
	return r, nil
}

func (r *rawRing) Close() error {
	return syscall.Close(r.fd)
}

// prepareSendMsg places a SENDMSG SQE at the next tail slot; the caller must
// keep *msg alive until the completion is reaped.
func (r *rawRing) prepareSendMsg(fd int, msg unsafe.Pointer) (slot uint32, err error) {
	tail := atomic.LoadUint32(r.sqTail)
	head := atomic.LoadUint32(r.sqHead)
	if tail-head >= uint32(len(r.sqes))/sqeSize {
		return 0, fmt.Errorf("submission queue full")
	}
	slot = tail & r.sqMask
	sqe := (*ioUringSQE)(unsafe.Pointer(&r.sqes[slot*sqeSize]))
	sqe.Opcode = opSendMsg
	sqe.Flags = 0
	sqe.Ioprio = 0
	sqe.Fd = int32(fd)
	sqe.Off = 0
	sqe.Addr = uint64(uintptr(msg))
	sqe.Len = 1
	sqe.UserData = uint64(slot)
	// publish the SQE index in the SQ array before advancing the tail: on
	// x86_64 the atomic store below is a full release for the preceding stores
	*(*uint32)(unsafe.Pointer(&r.sqRing[r.sqArrayOff+4*slot])) = slot
	atomic.StoreUint32(r.sqTail, tail+1)
	return slot, nil
}

// enter submits pending SQEs and waits for pending completions.
func (r *rawRing) enter(toSubmit, toWait uint32) error {
	if toSubmit == 0 {
		return nil
	}
	_, _, errno := syscall.Syscall6(sysIoUringEnter, uintptr(r.fd), uintptr(toSubmit), uintptr(toWait), enterGetEvents, 0, 0)
	if errno != 0 {
		return fmt.Errorf("io_uring_enter: %w", errno)
	}
	return nil
}

// reapCQEs consumes up to max completions, failing on the first error result.
func (r *rawRing) reapCQEs(max uint32) (uint32, error) {
	var reaped uint32
	for reaped < max {
		head := atomic.LoadUint32(r.cqHead)
		tail := atomic.LoadUint32(r.cqTail)
		if head == tail {
			return reaped, nil
		}
		cqe := (*ioUringCQE)(unsafe.Pointer(&r.cqRing[r.cqesOff+(head&r.cqMask)*16]))
		res := int32(atomic.LoadInt32((*int32)(unsafe.Pointer(&cqe.Res))))
		atomic.StoreUint32(r.cqHead, head+1)
		if res < 0 {
			return reaped, fmt.Errorf("io_uring completion: %w", syscall.Errno(-res))
		}
		reaped++
	}
	return reaped, nil
}

type ioUringCQE struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

// msghdr/iov layouts for x86_64, kept local: net.Msghdr is not constructible
// without net package internals.
type msghdrRaw struct {
	Name       unsafe.Pointer
	NameLen    uint32
	_          uint32
	Iov        *iovecRaw
	IovLen     uint64
	Control    unsafe.Pointer
	ControlLen uint64
	Flags      int32
	_          uint32
}

type iovecRaw struct {
	Base unsafe.Pointer
	Len  uint64
}
