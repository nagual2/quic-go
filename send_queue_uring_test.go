//go:build linux

package quic

// Integration test for the io_uring send queue: a real UDP socket pair, the
// Run loop pushing a batch of GSO packets through the ring, all of them
// received on the other end.

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"

	"github.com/stretchr/testify/require"
)

// fakeRawConn adapts a *net.UDPConn to the rawConn interface.
type fakeRawConn struct {
	*net.UDPConn
}

func (c *fakeRawConn) ReadPacket() (receivedPacket, error) {
	return receivedPacket{}, net.ErrClosed
}

func (c *fakeRawConn) WritePacket(b []byte, addr net.Addr, _ []byte, _ uint16, _ protocol.ECN) (int, error) {
	n, _, err := c.WriteMsgUDP(b, nil, addr.(*net.UDPAddr))
	return n, err
}

func (c *fakeRawConn) capabilities() connCapabilities {
	return connCapabilities{DF: true, GSO: true, ECN: true}
}

func TestIOUringSendQueueLoopback(t *testing.T) {
	t.Setenv("QUIC_GO_IO_URING_SEND", "1")

	rx, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer rx.Close()
	require.NoError(t, rx.SetReadBuffer(4 << 20))

	tx, err := net.DialUDP("udp", nil, rx.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer tx.Close()

	sc := newSendConn(&fakeRawConn{UDPConn: tx}, rx.LocalAddr(), packetInfo{}, utils.DefaultLogger)
	q := newIOUringSendQueue(sc)
	uq, ok := q.(*uringSendQueue)
	require.True(t, ok, "expected the uring send queue")
	require.NotNil(t, uq.sender, "ring sender should be available on Linux")

	done := make(chan error, 1)
	go func() { done <- q.Run() }()

	const packets = 5
	payload := bytes.Repeat([]byte{0x42}, 1200)
	for range packets {
		q.Send(getPacketWithContents(payload), 1200, protocol.ECNUnsupported)
	}
	q.Close()
	require.NoError(t, <-done)

	require.NoError(t, rx.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 2000)
	for received := 0; received < packets; received++ {
		n, err := rx.Read(buf)
		require.NoError(t, err)
		require.Equal(t, payload, buf[:n])
	}
}
