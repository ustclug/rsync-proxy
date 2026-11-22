package server

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ustclug/rsync-proxy/pkg/queue"
	"github.com/ustclug/rsync-proxy/test/fake/rsync"
)

func TestPerIPLimitKey(t *testing.T) {
	key := func(ip string) string {
		return perIPLimitKey(&net.TCPAddr{IP: net.ParseIP(ip), Port: 1234})
	}
	assert.Equal(t, "192.0.2.1", key("192.0.2.1"))
	assert.Equal(t, "192.0.2.1", key("::ffff:192.0.2.1"))
	assert.NotEqual(t, key("192.0.2.1"), key("192.0.2.2"))
	assert.Equal(t, "2001:db8:1:1::/64", key("2001:db8:1:1::1"))
	assert.Equal(t, key("2001:db8:1:1::1"), key("2001:db8:1:1:ffff:ffff:ffff:ffff"))
	assert.NotEqual(t, key("2001:db8:1:1::1"), key("2001:db8:1:2::1"))
	assert.Equal(t, "/run/x.sock", perIPLimitKey(&net.UnixAddr{Name: "/run/x.sock", Net: "unix"}))
}

func TestPerIPTrackerDropsIdleEntries(t *testing.T) {
	var tr perIPTracker
	for i := 0; i < 1000; i++ {
		key := perIPCountKey{upstream: "u1", ip: fmt.Sprintf("2001:db8:%x::/64", i)}
		require.True(t, tr.acquire(key, 2))
		require.True(t, tr.acquire(key, 2))
		require.False(t, tr.acquire(key, 2))
		tr.release(key)
		tr.release(key)
	}
	assert.Zero(t, tr.size(), "no entry may outlive its last connection")
}

type remoteAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c *remoteAddrConn) RemoteAddr() net.Addr { return c.remote }

// Addresses of the same IPv6 /64 share one allowance.
func TestPerIPCapCoversWholeIPv6Slash64(t *testing.T) {
	release := make(chan struct{})
	upstream := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
		if _, _, err := doServerHandshake(conn, RsyncdServerVersion); err != nil {
			return
		}
		_, _ = conn.Write([]byte("@RSYNCD: OK\n"))
		<-release
	})
	upstream.Start()
	defer upstream.Close()

	srv := New()
	srv.ReadTimeout = 2 * time.Second
	srv.WriteTimeout = 2 * time.Second
	srv.reloadLock.Lock()
	srv.upstreams = []upstreamConfig{{Name: "u1", PerIPMaxActiveConns: 1}}
	srv.modules = map[string][]Target{
		"fake": {{Upstream: "u1", Addr: upstream.Listener.Addr().String()}},
	}
	srv.upstreamQueues = map[string]*queue.Queue{"u1": queue.New(0, 0)}
	srv.reloadLock.Unlock()

	done := make(chan struct{}, 8)
	request := func(ip string) (string, net.Conn) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer ln.Close()
		cli, err := net.Dial("tcp", ln.Addr().String())
		require.NoError(t, err)
		svr, err := ln.Accept()
		require.NoError(t, err)
		go func() {
			srv.handleConn(t.Context(), &remoteAddrConn{Conn: svr, remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000}})
			done <- struct{}{}
		}()
		_, err = cli.Write(RsyncdServerVersion)
		require.NoError(t, err)
		buf := make([]byte, 256)
		_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, err = cli.Read(buf)
		require.NoError(t, err)
		_, err = cli.Write([]byte("fake\n"))
		require.NoError(t, err)
		n, err := cli.Read(buf)
		if err != nil && err != io.EOF {
			require.NoError(t, err)
		}
		return string(buf[:n]), cli
	}

	first, c1 := request("2001:db8:1:1::1")
	defer c1.Close()
	assert.Equal(t, "@RSYNCD: OK\n", first)

	sameSlash64, c2 := request("2001:db8:1:1:dead:beef:0:2")
	defer c2.Close()
	assert.True(t, strings.HasPrefix(sameSlash64, "@ERROR: per-IP connection limit of 1"), sameSlash64)

	otherSlash64, c3 := request("2001:db8:1:2::1")
	defer c3.Close()
	assert.Equal(t, "@RSYNCD: OK\n", otherSlash64)

	close(release)
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("handler did not finish")
		}
	}
	assert.Zero(t, srv.perIPConns.size(), "tracker must be empty once all clients are gone")
}
