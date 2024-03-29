package server

import (
	"io"
	"net"
	"reflect"
	"testing"
	"time"
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

	buf := make([]byte, TCPBufferSize)
	n, err := readLine(c, buf, time.Minute)
	if err != nil {
		t.Error(err)
	}
	got := buf[:n]
	expected := []byte("@RSYNCD: 31.0\n")
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("Unexpected data\nExpected: %s\nGot: %s\n", string(expected), string(got))
	}
}
