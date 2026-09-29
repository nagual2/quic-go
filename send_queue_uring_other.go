//go:build !linux

package quic

// The io_uring send queue is Linux-only; on other platforms newSendQueue
// always returns the standard sendQueue.

func newIOUringSendQueue(conn sendConn) sender { return nil }
