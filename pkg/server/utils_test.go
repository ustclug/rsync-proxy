package server

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeConn struct {
	fragments [][]byte

	curIdx int
}

func (c *fakeConn) Read(b []byte) (n int, err error) {
	bound := len(c.fragments)
	if c.curIdx >= bound {
		return 0, io.EOF
	}
	n = copy(b, c.fragments[c.curIdx])
	c.curIdx++
	return
}

func (c *fakeConn) Write(b []byte) (n int, err error) {
	panic("implement me")
}

func (c *fakeConn) Close() error {
	panic("implement me")
}

func (c *fakeConn) LocalAddr() net.Addr {
	panic("implement me")
}

func (c *fakeConn) RemoteAddr() net.Addr {
	panic("implement me")
}

func (c *fakeConn) SetDeadline(t time.Time) error {
	panic("implement me")
}

func (c *fakeConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *fakeConn) SetWriteDeadline(t time.Time) error {
	panic("implement me")
}

func TestReadLine(t *testing.T) {
	c := &fakeConn{fragments: [][]byte{
		RsyncdVersionPrefix,
		[]byte(" 31.0"),
		{'\n'},
	}}

	buf := make([]byte, ReadBufferSize)
	n, err := readLine(c, buf, time.Minute)
	require.NoError(t, err)
	got := buf[:n]
	expected := []byte("@RSYNCD: 31.0\n")
	assert.Equal(t, expected, got, "unexpected data")
}

func TestListenAndDialUnixSocket(t *testing.T) {
	addr := filepath.Join(t.TempDir(), "rsync-proxy.sock")

	listener, err := listenTCPOrUnix(addr)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, listener.Close())
	}()

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	conn, err := dialContextTCPOrUnix(context.Background(), net.Dialer{}, addr)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, conn.Close())
	}()

	select {
	case err := <-acceptErr:
		require.NoError(t, err)
	case acceptedConn := <-accepted:
		defer func() {
			require.NoError(t, acceptedConn.Close())
		}()
		assert.IsType(t, &net.UnixConn{}, acceptedConn)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for unix socket accept")
	}

	assert.IsType(t, &net.UnixConn{}, conn)
	assert.FileExists(t, addr)

	info, err := net.ResolveUnixAddr("unix", addr)
	require.NoError(t, err)
	assert.Equal(t, info.String(), netAddrToString(info))
}
