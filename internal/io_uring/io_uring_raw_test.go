package io_uring

// Low-level verification of the raw io_uring bindings, bisecting the layers:
// ring setup -> SQE placement -> enter -> completion (IORING_OP_WRITE to stdout).

import (
	"os"
	"syscall"
	"testing"
	"unsafe"
)

const opWrite = 5 // IORING_OP_WRITE

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

// TestProbeSQEBase locates the SQE array inside the SQ mmap on this kernel:
// at every candidate base, 8 identical WRITE probes are laid out; the base
// whose probe the kernel executes is reported through the CQE userdata.
func TestProbeSQEBase(t *testing.T) {
	var params ioUringParams
	fd, _, errno := syscall.Syscall(sysIoUringSetup, 8, uintptr(unsafe.Pointer(&params)), 0)
	if errno != 0 {
		t.Fatalf("setup: %v", errno)
	}
	defer syscall.Close(int(fd))

	sqSize := int64(params.SqOff.Array) + int64(params.SqEntries)*4 + 16384
	sqRing := mmapRing(int(fd), offSqRing, sqSize)
	cqRing := mmapRing(int(fd), offCqRing, int64(params.CqOff.Cqes)+int64(params.CqEntries)*16)

	sqMaskV := *(*uint32)(unsafe.Pointer(&sqRing[params.SqOff.RingMask]))
	sqTail := (*uint32)(unsafe.Pointer(&sqRing[params.SqOff.Tail]))
	cqHead := (*uint32)(unsafe.Pointer(&cqRing[params.CqOff.Head]))
	t.Logf("kernel reports: sqMask(offset field)=%d", sqMaskV)

	// identity init of the index array
	for i := uint32(0); i < params.SqEntries; i++ {
		*(*uint32)(unsafe.Pointer(&sqRing[params.SqOff.Array+4*i])) = i
	}

	probe := []byte("PROBE\n")
	sqeBytes := make([]byte, 64)
	sqe := (*ioUringSQE)(unsafe.Pointer(&sqeBytes[0]))
	sqe.Opcode = opWrite
	sqe.Fd = int32(os.Stdout.Fd())
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&probe[0])))
	sqe.Len = uint32(len(probe))

	for _, base := range []int64{352, 512, 1024, 2048, 4096, 8192} {
		for j := 0; j < 8; j++ {
			sqe.UserData = uint64(base) + uint64(j)
			copy(sqRing[base+int64(j)*64:base+int64(j)*64+64], sqeBytes)
		}
		// submit all 8 slots of this candidate
		*sqTail = *sqTail + 8
		if _, _, errno := syscall.Syscall6(sysIoUringEnter, fd, 8, 8, enterGetEvents, 0, 0); errno != 0 {
			t.Fatalf("enter: %v", errno)
		}
		// the first CQE's userdata names the slot the kernel executed
		for k := 0; k < 8; k++ {
			cqe := (*ioUringCQE)(unsafe.Pointer(&cqRing[params.CqOff.Cqes+(uint32(*cqHead)&15)*16]))
			atomicStore(cqHead, *cqHead+1)
			t.Logf("base=%d slot=%d: cqe userdata=%#x res=%d", base, k, cqe.UserData, int32(cqe.Res))
			if cqe.UserData >= uint64(base) && cqe.UserData < uint64(base)+8 {
				t.Logf("FOUND: kernel reads SQEs from offset %d", base)
				return
			}
		}
	}
	t.Fatal("no candidate base executed a probe")
}

