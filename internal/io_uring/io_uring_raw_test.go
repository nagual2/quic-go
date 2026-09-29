package io_uring

// Low-level verification of the raw io_uring bindings, bisecting the layers:
// ring setup -> SQE placement -> enter -> completion (IORING_OP_WRITE to stdout).

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// IORING_OP_WRITE. Opcode 5 is WRITE_FIXED, which requires registered
// buffers and fails with EFAULT without them.
const opWrite = 23

func TestRingWriteStdout(t *testing.T) {
	// request via setup so we can inspect the kernel-filled params
	var params ioUringParams
	fd, _, errno := syscall.Syscall(sysIoUringSetup, 8, uintptr(unsafe.Pointer(&params)), 0)
	if errno != 0 {
		t.Fatalf("setup: %v", errno)
	}
	defer syscall.Close(int(fd))
	t.Logf("params: sqEntries=%d cqEntries=%d features=%#x", params.SqEntries, params.CqEntries, params.Features)
	t.Logf("sqOff: %+v", params.SqOff)
	t.Logf("cqOff: %+v", params.CqOff)
	raw := (*[112]byte)(unsafe.Pointer(&params))
	for i := 0; i < 112; i += 16 {
		t.Logf("params[%3d:%3d] % x", i, i+16, raw[i:i+16])
	}

	r, err := newRawRing(8)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer r.Close()

	t.Logf("params check: sqMask=%d cqMask=%d", r.sqMask, r.cqMask)
	var uts syscall.Utsname
	if err := syscall.Uname(&uts); err == nil {
		rel := (*[65]byte)(unsafe.Pointer(&uts.Release[0]))
		n := bytes.IndexByte(rel[:], 0)
		t.Logf("kernel: %s", rel[:n])
	}

	payload := []byte("RING-OK\n")
	sqe := (*ioUringSQE)(unsafe.Pointer(&r.sqes[0]))
	sqe.Opcode = opWrite
	sqe.Flags = 0
	sqe.Fd = int32(os.Stdout.Fd())
	sqe.Off = 0
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&payload[0])))
	sqe.Len = uint32(len(payload))
	sqe.UserData = 0x1234

	tail := atomicLoad(r.sqTail)
	idx := tail & r.sqMask
	*(*uint32)(unsafe.Pointer(&r.sqRing[r.sqArrayOff+4*idx])) = idx
	atomicStore(r.sqTail, tail+1)

	if err := r.enter(1, 1); err != nil {
		t.Fatalf("enter: %v", err)
	}
	wrote := (*ioUringSQE)(unsafe.Pointer(&r.sqes[0]))
	t.Logf("sqe[0] after enter: opcode=%d fd=%d len=%d userdata=%#x",
		wrote.Opcode, wrote.Fd, wrote.Len, wrote.UserData)
	sqHead := atomicLoad(r.sqHead)
	sqTail := atomicLoad(r.sqTail)
	cqHead := atomicLoad(r.cqHead)
	cqTail := atomicLoad(r.cqTail)
	t.Logf("rings: sqHead=%d sqTail=%d cqHead=%d cqTail=%d", sqHead, sqTail, cqHead, cqTail)
	t.Logf("cq cqes area: % x", r.cqRing[r.cqesOff:r.cqesOff+32])
	head := atomicLoad(r.cqHead)
	cqe := (*ioUringCQE)(unsafe.Pointer(&r.cqRing[r.cqesOff+(head&r.cqMask)*16]))
	res := int32(cqe.Res)
	flags := cqe.Flags
	atomicStore(r.cqHead, head+1)
	t.Logf("cqe: userdata=%#x res=%d flags=%d", cqe.UserData, res, flags)
	if res != int32(len(payload)) {
		t.Fatalf("write res = %d, want %d", res, len(payload))
	}
}

func atomicLoad(p *uint32) uint32     { return *p }
func atomicStore(p *uint32, v uint32) { *p = v }

