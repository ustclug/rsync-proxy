package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ustclug/rsync-proxy/pkg/queue"
)

// Invariant: whatever a client sends, the first two lines that reach the
// upstream rsyncd are (1) ONE greeting line and (2) a module name that the
// proxy is configured to serve.
//
//	go test -run '^$' -fuzz '^FuzzHandshakeForwarding$' -fuzztime 5m ./pkg/server/

type forwardingUpstream struct {
	ln   net.Listener
	mu   sync.Mutex
	seen [][]byte
	wg   sync.WaitGroup
}

func newForwardingUpstream(tb testing.TB) *forwardingUpstream {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	u := &forwardingUpstream{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u.wg.Add(1)
			go func() {
				defer u.wg.Done()
				defer c.Close()
				_, _ = c.Write(RsyncdServerVersion)
				_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
				data, _ := io.ReadAll(c)
				u.mu.Lock()
				u.seen = append(u.seen, data)
				u.mu.Unlock()
			}()
		}
	}()
	return u
}

func (u *forwardingUpstream) take() [][]byte {
	u.wg.Wait()
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.seen
	u.seen = nil
	return out
}

func FuzzHandshakeForwarding(f *testing.F) {
	up := newForwardingUpstream(f)
	defer up.ln.Close()

	srv := New()
	srv.ReadTimeout = 150 * time.Millisecond
	srv.WriteTimeout = 150 * time.Millisecond
	srv.modules = map[string][]Target{
		"allowed": {{Upstream: "u1", Addr: up.ln.Addr().String()}},
	}
	srv.upstreams = []upstreamConfig{{Name: "u1"}}
	srv.upstreamQueues = map[string]*queue.Queue{"u1": queue.New(0, 0)}

	// first chunk, second chunk (each is written with a single Write call)
	f.Add([]byte("@RSYNCD: 31.0\n"), []byte("allowed\n"))
	f.Add([]byte("@RSYNCD: 32.0 sha512 sha256 sha1 md5 md4\n"), []byte("nope\n"))
	f.Add([]byte("@RSYNCD: 29.0\n"), []byte("\n"))
	f.Add([]byte("garbage\n"), []byte("allowed\n"))

	f.Fuzz(func(t *testing.T, first, second []byte) {
		if len(first) > 600 || len(second) > 600 {
			t.Skip()
		}
		cli, svr := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.handleConn(context.Background(), svr)
		}()
		go func() { _, _ = io.Copy(io.Discard, cli) }() // drain whatever the proxy says
		_ = cli.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := cli.Write(first); err == nil {
			time.Sleep(time.Millisecond)
			_, _ = cli.Write(second)
		}
		time.Sleep(2 * time.Millisecond)
		_ = cli.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("handleConn did not return")
		}

		for _, data := range up.take() {
			lines := bytes.SplitN(data, []byte("\n"), 3)
			if len(lines) < 3 {
				continue // upstream did not even get two complete lines
			}
			if !bytes.HasPrefix(lines[0], RsyncdVersionPrefix) {
				t.Fatalf("upstream got a first line that is not a greeting: %q", data)
			}
			if string(lines[1]) != "allowed" {
				t.Fatalf("SMUGGLED: upstream was asked for module %q (proxy only serves \"allowed\"); "+
					"client chunks: %q | %q; upstream saw: %q", lines[1], first, second, data)
			}
		}
	})
}
