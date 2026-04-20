package server

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ustclug/rsync-proxy/pkg/queue"
	"github.com/ustclug/rsync-proxy/test/fake/rsync"
)

func startServer(t *testing.T) *Server {
	srv := New()
	const (
		addr    = "127.0.0.1:0"
		timeout = time.Second
	)
	srv.HTTPListenAddr = addr
	srv.ListenAddr = addr
	srv.ReadTimeout = timeout
	srv.WriteTimeout = timeout
	err := srv.Listen()
	require.NoErrorf(t, err, "Fail to listen")

	go func() {
		t.Logf("rsync-proxy is running on: %s", srv.TCPListener.Addr())
		err := srv.Run()
		assert.NoErrorf(t, err, "Fail to run server")
	}()
	return srv
}

func doClientHandshake(conn *rsync.Conn, version []byte, module string) (svrVersion string, err error) {
	_, err = conn.Write(version)
	if err != nil {
		return
	}

	svrVersion, err = conn.ReadLine()
	if err != nil {
		return
	}

	_, err = conn.Write([]byte(module + "\n"))
	return
}

func doServerHandshake(conn *rsync.Conn, data []byte) (cliVersion, module string, err error) {
	// read protocol version from client
	cliVersion, err = conn.ReadLine()
	if err != nil {
		return
	}

	_, err = conn.Write(data)
	if err != nil {
		return
	}

	// read module name from client
	module, err = conn.ReadLine()
	return
}

// See also: https://github.com/ustclug/rsync-proxy/issues/11
func TestMotdFromServer(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()
	proxyMotd := "Hello\n"
	srv.Motd = proxyMotd

	l := strings.Repeat("a", TCPBufferSize)
	serverMotd := fmt.Sprintf("%s\n%s\n\n", l, l)

	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()

		_, _, err := doServerHandshake(conn, append(RsyncdServerVersion, []byte(serverMotd)...))
		assert.NoError(t, err)
	})
	fakeRsync.Start()
	defer fakeRsync.Close()

	srv.modules = map[string][]Target{
		"fake": {{Upstream: "u1", Addr: fakeRsync.Listener.Addr().String()}},
	}
	srv.upstreamQueues = map[string]*queue.Queue{"u1": queue.New(0, 0)}

	r := require.New(t)

	rawConn, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	r.NoError(err)
	conn := rsync.NewConn(rawConn)
	defer conn.Close()

	_, err = doClientHandshake(conn, RsyncdServerVersion, "fake")
	r.NoError(err)

	allData, err := io.ReadAll(conn)
	r.NoError(err)

	r.Equal(proxyMotd+"\n"+serverMotd, string(allData))
}

// See also: https://github.com/ustclug/rsync-proxy/commit/d581c18dab8008c5bc9c1a5d667b49d67a4edfed
func TestClientReadTimeout(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	r := require.New(t)

	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()

		_, _, err := doServerHandshake(conn, RsyncdServerVersion)
		r.NoError(err, "server handshake")

		for i := 0; i < 3; i++ {
			_, err = conn.Write([]byte("data\n"))
			r.NoError(err, "write data")
			time.Sleep(srv.ReadTimeout)
		}
	})
	fakeRsync.Start()
	defer fakeRsync.Close()

	srv.modules = map[string][]Target{
		"fake": {{Upstream: "u1", Addr: fakeRsync.Listener.Addr().String()}},
	}
	srv.upstreamQueues = map[string]*queue.Queue{"u1": queue.New(0, 0)}

	rawConn, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	r.NoError(err)
	conn := rsync.NewConn(rawConn)
	defer conn.Close()

	_, err = doClientHandshake(conn, RsyncdServerVersion, "fake")
	r.NoError(err)

	allData, err := io.ReadAll(conn)
	r.NoError(err)

	expected := strings.Repeat("data\n", 3)
	r.Equal(expected, string(allData))
}

func TestTLSRsyncListener(t *testing.T) {
	r := require.New(t)

	tlsFiles := writeTestTLSCert(t, t.TempDir(), "server", "rsync-proxy-tls")

	srv := New()
	const (
		addr    = "127.0.0.1:0"
		timeout = time.Second
	)
	srv.HTTPListenAddr = addr
	srv.ListenAddr = addr
	srv.TLSListenAddr = addr
	srv.ReadTimeout = timeout
	srv.WriteTimeout = timeout
	cert, err := tls.LoadX509KeyPair(tlsFiles.certPath, tlsFiles.keyPath)
	r.NoError(err)
	srv.tlsCertificate = &cert
	srv.tlsConfig.GetCertificate = srv.getTLSCertificate
	err = srv.Listen()
	r.NoError(err)
	defer srv.Close()

	go func() {
		err := srv.Run()
		assert.NoErrorf(t, err, "Fail to run server")
	}()

	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()

		_, module, err := doServerHandshake(conn, RsyncdServerVersion)
		r.NoError(err, "server handshake")
		assert.Equal(t, "fake\n", module)
	})
	fakeRsync.Start()
	defer fakeRsync.Close()

	srv.modules = map[string][]Target{
		"fake": {{Upstream: "u1", Addr: fakeRsync.Listener.Addr().String()}},
	}
	srv.upstreamQueues = map[string]*queue.Queue{"u1": queue.New(0, 0)}

	pool := x509.NewCertPool()
	certPEM, err := os.ReadFile(tlsFiles.certPath)
	r.NoError(err)
	pool.AppendCertsFromPEM(certPEM)

	rawConn, err := tls.Dial("tcp", srv.TLSListenAddr, &tls.Config{
		RootCAs:    pool,
		ServerName: "localhost",
	})
	r.NoError(err)
	conn := rsync.NewConn(rawConn)
	defer conn.Close()

	_, err = doClientHandshake(conn, RsyncdServerVersion, "fake")
	r.NoError(err)
}

func TestReloadTLSCertificate(t *testing.T) {
	dir := t.TempDir()
	firstCert := writeTestTLSCert(t, dir, "first", "first-cert")
	secondCert := writeTestTLSCert(t, dir, "second", "second-cert")

	configPath := filepath.Join(dir, "config.toml")
	writeConfig := func(certFiles tlsCertFiles) {
		configContent := fmt.Sprintf(`
[proxy]
listen = "127.0.0.1:0"
listen_http = "127.0.0.1:0"
listen_tls = "127.0.0.1:0"
tls_cert_file = %q
tls_key_file = %q

[upstreams.u1]
address = "127.0.0.1:1234"
modules = ["foo"]
`, certFiles.certPath, certFiles.keyPath)
		require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0600))
	}

	writeConfig(firstCert)

	srv := New()
	srv.ConfigPath = configPath
	srv.ReadTimeout = time.Second
	srv.WriteTimeout = time.Second
	err := srv.ReadConfigFromFile(true)
	require.NoError(t, err)
	err = srv.Listen()
	require.NoError(t, err)
	defer srv.Close()

	go func() {
		err := srv.Run()
		assert.NoErrorf(t, err, "Fail to run server")
	}()

	getCommonName := func() string {
		conn, err := tls.Dial("tcp", srv.TLSListenAddr, &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "localhost",
		})
		require.NoError(t, err)
		defer conn.Close()

		state := conn.ConnectionState()
		return state.PeerCertificates[0].Subject.CommonName
	}

	assert.Equal(t, firstCert.commonName, getCommonName())

	writeConfig(secondCert)
	require.NoError(t, srv.ReadConfigFromFile(true))
	assert.Equal(t, secondCert.commonName, getCommonName())
}

func TestChooseTargetByClientIP(t *testing.T) {
	first := chooseTargetByClientIP(net.ParseIP("192.0.2.1"), 2)
	second := chooseTargetByClientIP(net.ParseIP("192.0.2.1"), 2)
	third := chooseTargetByClientIP(net.ParseIP("198.51.100.10"), 2)
	single := chooseTargetByClientIP(net.ParseIP("203.0.113.1"), 1)

	assert.Equal(t, first, second)
	assert.Contains(t, []int{0, 1}, first)
	assert.Contains(t, []int{0, 1}, third)
	assert.Equal(t, 0, single)
}

func TestStatusIncludesSelectedUpstream(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	var (
		wg           sync.WaitGroup
		upstreamAddr string
	)
	wg.Add(1)
	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
		_, _, err := doServerHandshake(conn, RsyncdServerVersion)
		require.NoError(t, err)
		wg.Wait()
	})
	fakeRsync.Start()
	defer fakeRsync.Close()

	upstreamAddr = fakeRsync.Listener.Addr().String()
	srv.modules = map[string][]Target{
		"fake": {{Upstream: "u1", Addr: upstreamAddr}},
	}
	srv.upstreamQueues = map[string]*queue.Queue{"u1": queue.New(0, 0)}

	rawConn, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	conn := rsync.NewConn(rawConn)
	defer conn.Close()

	_, err = doClientHandshake(conn, RsyncdServerVersion, "fake")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		infos := srv.ListConnectionInfo()
		return len(infos) == 1 && infos[0].UpstreamAddr == upstreamAddr
	}, time.Second, 10*time.Millisecond)

	wg.Done()
}

func TestPerUpstreamQueueIsolation(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	var (
		firstReady sync.WaitGroup
		release1   sync.WaitGroup
		release2   sync.WaitGroup
		started1   atomic.Int32
		started2   atomic.Int32
	)
	firstReady.Add(1)
	release1.Add(1)
	release2.Add(1)

	upstream1 := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
		_, _, err := doServerHandshake(conn, RsyncdServerVersion)
		require.NoError(t, err)
		if started1.Add(1) == 1 {
			firstReady.Done()
			release1.Wait()
		}
	})
	upstream1.Start()
	defer upstream1.Close()

	upstream2 := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
		_, _, err := doServerHandshake(conn, RsyncdServerVersion)
		require.NoError(t, err)
		started2.Add(1)
		release2.Wait()
	})
	upstream2.Start()
	defer upstream2.Close()

	srv.modules = map[string][]Target{
		"same-a": {{Upstream: "u1", Addr: upstream1.Listener.Addr().String()}},
		"same-b": {{Upstream: "u1", Addr: upstream1.Listener.Addr().String()}},
		"other":  {{Upstream: "u2", Addr: upstream2.Listener.Addr().String()}},
	}
	srv.upstreamQueues = map[string]*queue.Queue{
		"u1": queue.New(1, 1),
		"u2": queue.New(1, 1),
	}

	client1Raw, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	client1 := rsync.NewConn(client1Raw)
	defer client1.Close()
	_, err = doClientHandshake(client1, RsyncdServerVersion, "same-a")
	require.NoError(t, err)

	firstReady.Wait()

	client2Raw, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	client2 := rsync.NewConn(client2Raw)
	defer client2.Close()
	_, err = doClientHandshake(client2, RsyncdServerVersion, "same-b")
	require.NoError(t, err)

	queuedLine, err := client2.ReadLine()
	require.NoError(t, err)
	assert.Contains(t, queuedLine, "Upstream u1 has reached")
	queuedPos, err := client2.ReadLine()
	require.NoError(t, err)
	assert.Contains(t, queuedPos, "Your position: 1")

	client3Raw, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	client3 := rsync.NewConn(client3Raw)
	defer client3.Close()
	_, err = doClientHandshake(client3, RsyncdServerVersion, "other")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return started2.Load() == 1
	}, time.Second, 10*time.Millisecond)

	release2.Done()
	release1.Done()

	require.Eventually(t, func() bool {
		return started1.Load() == 2
	}, time.Second, 10*time.Millisecond)
}

func TestQueueFullRejectsConnection(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	var release sync.WaitGroup
	release.Add(1)

	upstream := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
		_, _, err := doServerHandshake(conn, RsyncdServerVersion)
		require.NoError(t, err)
		release.Wait()
	})
	upstream.Start()
	defer upstream.Close()

	srv.modules = map[string][]Target{
		"fake": {{Upstream: "u1", Addr: upstream.Listener.Addr().String()}},
	}
	srv.upstreamQueues = map[string]*queue.Queue{"u1": queue.New(1, 1)}

	client1Raw, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	client1 := rsync.NewConn(client1Raw)
	defer client1.Close()
	_, err = doClientHandshake(client1, RsyncdServerVersion, "fake")
	require.NoError(t, err)

	client2Raw, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	client2 := rsync.NewConn(client2Raw)
	defer client2.Close()
	_, err = doClientHandshake(client2, RsyncdServerVersion, "fake")
	require.NoError(t, err)
	_, err = client2.ReadLine()
	require.NoError(t, err)
	_, err = client2.ReadLine()
	require.NoError(t, err)

	client3Raw, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	client3 := rsync.NewConn(client3Raw)
	defer client3.Close()
	_, err = doClientHandshake(client3, RsyncdServerVersion, "fake")
	require.NoError(t, err)

	line, err := client3.ReadLine()
	require.NoError(t, err)
	assert.Contains(t, line, "Server queue is full")
	exit, err := client3.ReadLine()
	require.NoError(t, err)
	assert.Equal(t, string(RsyncdExit), exit)

	release.Done()
}

func TestStartupFailsWhenModuleDiscoveryFails(t *testing.T) {
	srv := New()
	srv.ReadTimeout = time.Second
	srv.WriteTimeout = time.Second

	upstream := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
	})
	upstream.Start()
	defer upstream.Close()

	configContent := `
[upstreams.u1]
address = "` + upstream.Listener.Addr().String() + `"
discover_modules = true
`
	err := srv.ReadConfig(strings.NewReader(configContent), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover modules from upstream")
}

func TestReloadKeepsPreviousModulesWhenDiscoveryFails(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	firstUpstream := rsync.NewModuleListServer([]string{"foo"})
	firstUpstream.Start()
	defer firstUpstream.Close()

	secondUpstream := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
	})
	secondUpstream.Start()
	defer secondUpstream.Close()

	writeConfig := func(addr string) {
		configContent := fmt.Sprintf(`
[proxy]
listen = "127.0.0.1:0"
listen_http = "127.0.0.1:0"

[upstreams.u1]
address = %q
discover_modules = true
`, addr)
		require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0600))
	}

	writeConfig(firstUpstream.Listener.Addr().String())
	srv := New()
	srv.ConfigPath = configPath
	srv.ReadTimeout = time.Second
	srv.WriteTimeout = time.Second
	require.NoError(t, srv.ReadConfigFromFile(true))
	require.Contains(t, srv.modules, "foo")

	writeConfig(secondUpstream.Listener.Addr().String())
	err := srv.ReadConfigFromFile(true)
	require.Error(t, err)

	srv.reloadLock.RLock()
	defer srv.reloadLock.RUnlock()
	_, hasFoo := srv.modules["foo"]
	_, hasBar := srv.modules["bar"]
	assert.True(t, hasFoo)
	assert.False(t, hasBar)
}

func TestListUpstreamModules(t *testing.T) {
	srv := New()
	srv.reloadLock.Lock()
	srv.upstreams = []upstreamConfig{
		{Name: "u1", Modules: []string{"foo", "bar"}},
		{Name: "u2", Modules: []string{"baz"}},
	}
	srv.reloadLock.Unlock()

	modules, err := srv.ListUpstreamModules("u1", false)
	require.NoError(t, err)
	assert.Equal(t, []string{"bar", "foo"}, modules)

	_, err = srv.ListUpstreamModules("missing", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown upstream")
}

func TestListUpstreamModulesForceDiscover(t *testing.T) {
	upstream := rsync.NewModuleListServer([]string{"bar", "foo"})
	upstream.Start()
	defer upstream.Close()

	srv := New()
	srv.ReadTimeout = time.Second
	srv.WriteTimeout = time.Second
	srv.reloadLock.Lock()
	srv.upstreams = []upstreamConfig{
		{
			Name:            "u1",
			Target:          Target{Upstream: "u1", Addr: upstream.Listener.Addr().String()},
			Modules:         []string{"stale"},
			DiscoverModules: false,
		},
	}
	srv.reloadLock.Unlock()

	modules, err := srv.ListUpstreamModules("u1", true)
	require.NoError(t, err)
	assert.Equal(t, []string{"bar", "foo"}, modules)
}

func TestDiscoverModules(t *testing.T) {
	upstream := rsync.NewModuleListServer([]string{"bar", "foo"})
	upstream.Start()
	defer upstream.Close()

	srv := New()
	srv.ReadTimeout = time.Second
	srv.WriteTimeout = time.Second

	modules, err := srv.DiscoverModules(upstream.Listener.Addr().String())
	require.NoError(t, err)
	assert.Equal(t, []string{"bar", "foo"}, modules)
}

func TestDiscoverModulesWithProxyProtocol(t *testing.T) {
	upstream := rsync.NewModuleListServerWithProxyProtocol([]string{"bar", "foo"})
	upstream.Start()
	defer upstream.Close()

	srv := New()
	srv.ReadTimeout = time.Second
	srv.WriteTimeout = time.Second

	modules, err := srv.DiscoverModulesWithProxyProtocol(upstream.Listener.Addr().String(), true)
	require.NoError(t, err)
	assert.Equal(t, []string{"bar", "foo"}, modules)
}

func TestDiscoverModulesFromProxyStyleListing(t *testing.T) {
	upstream := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()

		line, err := conn.ReadLine()
		require.NoError(t, err)
		require.Equal(t, string(RsyncdServerVersion), line)

		_, err = conn.Write(RsyncdServerVersion)
		require.NoError(t, err)

		line, err = conn.ReadLine()
		require.NoError(t, err)
		require.Equal(t, "\n", line)

		_, err = conn.Write([]byte("Served by rsync-proxy\n\nfoo\nbar\n@RSYNCD: EXIT\n"))
		require.NoError(t, err)
	})
	upstream.Start()
	defer upstream.Close()

	srv := New()
	srv.ReadTimeout = time.Second
	srv.WriteTimeout = time.Second

	modules, err := srv.DiscoverModules(upstream.Listener.Addr().String())
	require.NoError(t, err)
	assert.Equal(t, []string{"bar", "foo"}, modules)
}

func TestDiscoverModulesFromTrailingModuleBlock(t *testing.T) {
	upstream := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()

		line, err := conn.ReadLine()
		require.NoError(t, err)
		require.Equal(t, string(RsyncdServerVersion), line)

		_, err = conn.Write(RsyncdServerVersion)
		require.NoError(t, err)

		line, err = conn.ReadLine()
		require.NoError(t, err)
		require.Equal(t, "\n", line)

		_, err = conn.Write([]byte("Welcome to upstream\nMirror notice\n\nfoo comment\nbar\n@RSYNCD: EXIT\n"))
		require.NoError(t, err)
	})
	upstream.Start()
	defer upstream.Close()

	srv := New()
	srv.ReadTimeout = time.Second
	srv.WriteTimeout = time.Second

	modules, err := srv.DiscoverModules(upstream.Listener.Addr().String())
	require.NoError(t, err)
	assert.Equal(t, []string{"bar", "foo"}, modules)
}
