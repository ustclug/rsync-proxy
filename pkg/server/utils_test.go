package server

import (
	"bufio"
	"bytes"
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

	got, err := readLine(bufio.NewReaderSize(c, ReadBufferSize))
	require.NoError(t, err)
	expected := []byte("@RSYNCD: 31.0\n")
	assert.Equal(t, expected, got, "unexpected data")
}

// A line is exactly one line: pipelined data stays in the reader.
func TestReadLineDoesNotReturnPipelinedData(t *testing.T) {
	c := &fakeConn{fragments: [][]byte{
		[]byte("@RSYNCD: 31.0\n#early_input=7\nrest"),
	}}
	br := bufio.NewReaderSize(c, ReadBufferSize)

	got, err := readLine(br)
	require.NoError(t, err)
	assert.Equal(t, "@RSYNCD: 31.0\n", string(got))

	got, err = readLine(br)
	require.NoError(t, err)
	assert.Equal(t, "#early_input=7\n", string(got))
	assert.Equal(t, 4, br.Buffered())
}

// A "line" that fills the buffer without a terminator is an error, it must not
// be returned (and later replayed to the upstream) as if it were complete.
func TestReadLineRejectsOverlongLine(t *testing.T) {
	c := &fakeConn{fragments: [][]byte{
		append([]byte("@RSYNCD: 31.0 "), bytes.Repeat([]byte("X"), ReadBufferSize)...),
	}}
	_, err := readLine(bufio.NewReaderSize(c, ReadBufferSize))
	require.ErrorIs(t, err, errLineTooLong)
}

func TestHandshakeValidation(t *testing.T) {
	assert.True(t, isValidGreeting([]byte("@RSYNCD: 31.0\n")))
	assert.True(t, isValidGreeting([]byte("@RSYNCD: 32.0 sha512 sha256 sha1 md5 md4\n")))
	assert.True(t, isValidGreeting([]byte("@RSYNCD: 29\n")))
	assert.False(t, isValidGreeting([]byte("@RSYNCD: 31.0\nfoo\n")))
	assert.False(t, isValidGreeting([]byte("@RSYNCD: 31.0 \x1b[2J\n")))
	assert.False(t, isValidGreeting([]byte("@RSYNCD: 31.0")))
	assert.False(t, isValidGreeting([]byte("@RSYNCD:\n")))
	assert.False(t, isValidGreeting([]byte("GET / HTTP/1.1\r\n")))

	assert.True(t, isValidModuleName("debian-cd"))
	assert.True(t, isValidModuleName("模块"))
	assert.True(t, isValidModuleName("with space"))
	assert.True(t, isValidModuleName("gbk-\xc4\xa3\xbf\xe9"), "module names are byte strings, not necessarily UTF-8")
	assert.False(t, isValidModuleName(""))
	assert.False(t, isValidModuleName("a\nb"))
	assert.False(t, isValidModuleName("a\rb"))
	assert.False(t, isValidModuleName("a\x1b[31m"))
	assert.False(t, isValidModuleName("a\x00b"))
	assert.False(t, isValidModuleName("a\x7f"))
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
