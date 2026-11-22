package server

import (
	"context"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flakyListener fails the first n Accept calls with EMFILE.
type flakyListener struct {
	net.Listener
	failures atomic.Int32
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return l.Listener.Accept()
}

// A transient accept error (fd exhaustion) must not make the accept loop
// return: Run() would return and the whole proxy would exit.
func TestAcceptLoopSurvivesTransientErrors(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	l := &flakyListener{Listener: inner}
	l.failures.Store(3)

	srv := New()
	srv.ReadTimeout = time.Second
	srv.WriteTimeout = time.Second

	done := make(chan error, 1)
	go func() { done <- srv.runRsyncServer(context.Background(), l, "accept rsync connection") }()

	// The loop is still alive after the failures and serves a client.
	conn, err := net.Dial("tcp", inner.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write(RsyncdServerVersion)
	require.NoError(t, err)
	buf := make([]byte, len(RsyncdServerVersion))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, string(RsyncdServerVersion), string(buf))

	select {
	case err := <-done:
		t.Fatalf("accept loop returned: %v", err)
	default:
	}

	// Closing the listener is still the way to stop it.
	require.NoError(t, inner.Close())
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("accept loop did not stop after the listener was closed")
	}
}
