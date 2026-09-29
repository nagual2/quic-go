package io_uring

// IORING_OP_WRITE through the ring into (a) an os.Pipe and (b) a regular
// file - guaranteed-valid targets that do not depend on the stdout
// plumbing of the go test process.

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func ringWrite(t *testing.T, r *rawRing, fd int, data []byte) {
	sqe := (*ioUringSQE)(unsafe.Pointer(&r.sqes[0]))
	sqe.Opcode = opWrite
	sqe.Flags = 0
	sqe.Fd = int32(fd)
	sqe.Off = 0
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&data[0])))
	sqe.Len = uint32(len(data))
	sqe.UserData = 0x1234

	tail := atomicLoad(r.sqTail)
	idx := tail & r.sqMask
	*(*uint32)(unsafe.Pointer(&r.sqRing[r.sqArrayOff+4*idx])) = idx
	atomicStore(r.sqTail, tail+1)

	if err := r.enter(1, 1); err != nil {
		t.Fatalf("enter: %v", err)
	}
	head := atomicLoad(r.cqHead)
	cqe := (*ioUringCQE)(unsafe.Pointer(&r.cqRing[r.cqesOff+(head&r.cqMask)*16]))
	res := int32(cqe.Res)
	atomicStore(r.cqHead, head+1)
	t.Logf("cqe res=%d userdata=%#x", res, cqe.UserData)
	t.Logf("sqes[0:32] after: % x", r.sqes[0:32])
	if res < 0 {
		t.Fatalf("write failed: %v", syscall.Errno(-res))
	}
	if res != int32(len(data)) {
		t.Fatalf("short write: %d of %d", res, len(data))
	}
}

func TestRingWritePipe(t *testing.T) {
	r, err := newRawRing(8)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()

	payload := []byte("PIPE-OK\n")
	ringWrite(t, r, int(pw.Fd()), payload)

	got := make([]byte, len(payload))
	pr.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := pr.Read(got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestRingWriteFile(t *testing.T) {
	r, err := newRawRing(8)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	f, err := os.CreateTemp("", "iouring")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	payload := []byte("FILE-OK\n")
	ringWrite(t, r, int(f.Fd()), payload)
	f.Close()

	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}
