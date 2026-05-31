package server

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
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

func setupAccessLog(t *testing.T, srv *Server) string {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "access-*.log")
	require.NoError(t, err)
	path := f.Name()
	require.NoError(t, f.Close())
	require.NoError(t, srv.accessLog.SetFile(path))
	srv.accessLog.SetFlags(0)
	t.Cleanup(func() {
		srv.accessLog.Close()
	})
	return path
}

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

func testHTTPClient() *http.Client {
	return &http.Client{Timeout: time.Second}
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

	l := strings.Repeat("a", ReadBufferSize)
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

// TestUnknownModuleSendsErrorPrefix verifies that requesting a module that
// the proxy is not configured to serve makes the proxy reply with an
// "@ERROR:" line, matching real rsyncd's wire format. The rsync client
// treats an "@ERROR:" reply as a fatal protocol error and exits 5,
// while a plain message followed by "@RSYNCD: EXIT" causes the client
// to exit 0, which historically masked configuration breakage in tools
// such as tunasync.
func TestUnknownModuleSendsErrorPrefix(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	srv.modules = map[string][]Target{}
	srv.upstreamQueues = map[string]*queue.Queue{}

	r := require.New(t)

	rawConn, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	r.NoError(err)
	conn := rsync.NewConn(rawConn)
	defer conn.Close()

	_, err = doClientHandshake(conn, RsyncdServerVersion, "does-not-exist")
	r.NoError(err)

	allData, err := io.ReadAll(conn)
	r.NoError(err)

	r.Contains(string(allData), "@ERROR:", "proxy should reply with @ERROR: prefix so client exits non-zero")
	r.Contains(string(allData), "does-not-exist")
	r.NotContains(string(allData), "@RSYNCD: EXIT", "should not send EXIT after @ERROR; rsync client must treat the response as fatal")
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

// TestRelayIdleTimeoutClosesIdleConnection verifies that when
// RelayIdleTimeout is configured and no data flows in either
// direction during the bidirectional relay phase, the proxy tears the
// connection down. This mirrors rsyncd's "timeout" behavior in
// rsyncd.conf(5).
func TestRelayIdleTimeoutClosesIdleConnection(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()
	srv.RelayIdleTimeout = 500 * time.Millisecond
	accessLogPath := setupAccessLog(t, srv)

	r := require.New(t)

	upstreamReady := make(chan struct{})
	upstreamDone := make(chan struct{})

	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
		defer close(upstreamDone)

		_, _, err := doServerHandshake(conn, RsyncdServerVersion)
		r.NoError(err, "upstream handshake")
		close(upstreamReady)

		// Stay quiet so the relay phase has no I/O. The proxy must
		// close us once the idle timeout elapses; ReadAll then
		// returns with an EOF / closed-connection error.
		_, _ = io.ReadAll(conn)
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

	<-upstreamReady

	start := time.Now()
	// ReadAll should return shortly after the proxy closes our
	// connection due to the idle timeout firing on the relay side.
	_, err = io.ReadAll(conn)
	r.NoError(err)
	elapsed := time.Since(start)

	// Allow generous slack: must be at least the configured timeout,
	// and not pathologically long (e.g. waiting forever).
	r.GreaterOrEqual(elapsed, srv.RelayIdleTimeout, "should have waited at least the idle timeout")
	r.Less(elapsed, 5*time.Second, "should not block far beyond the idle timeout")

	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream connection was not closed after idle timeout")
	}

	logData, err := os.ReadFile(accessLogPath)
	r.NoError(err)
	assert.Contains(t, string(logData), "idle for module fake exceeds")
}

// TestRelayIdleTimeoutNotTriggeredWhenActive verifies that the idle
// timeout is reset whenever data flows, so a slow but continuously
// active stream does not get cut. The fake upstream sends data at an
// interval well below the idle timeout for several iterations.
func TestRelayIdleTimeoutNotTriggeredWhenActive(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()
	srv.RelayIdleTimeout = 2 * time.Second

	r := require.New(t)

	const iterations = 5
	const interval = 200 * time.Millisecond

	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
		_, _, err := doServerHandshake(conn, RsyncdServerVersion)
		r.NoError(err, "upstream handshake")

		for i := 0; i < iterations; i++ {
			_, err = conn.Write([]byte("data\n"))
			r.NoError(err, "write data")
			time.Sleep(interval)
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

	expected := strings.Repeat("data\n", iterations)
	r.Equal(expected, string(allData),
		"steadily flowing traffic must not be interrupted by the idle timeout")
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
		if len(infos) != 1 {
			return false
		}
		return infos[0].snapshot().Upstream == "u1"
	}, time.Second, 10*time.Millisecond)

	wg.Done()
}

func TestMetricsEndpointNoConnections(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	resp, err := testHTTPClient().Get("http://" + srv.HTTPListener.Addr().String() + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain; version=0.0.4")
	assert.Contains(t, text, "# HELP rsync_proxy_active_connections Current active rsync proxy connections.")
	assert.Contains(t, text, "# TYPE rsync_proxy_active_connections gauge")
	assert.Contains(t, text, "rsync_proxy_active_connections 0\n")
}

func TestMetricsEndpointRejectsNonGET(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	resp, err := testHTTPClient().Post("http://"+srv.HTTPListener.Addr().String()+"/metrics", "text/plain", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestMetricsIncludesActiveConnections(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
		_, _, err := doServerHandshake(conn, RsyncdServerVersion)
		require.NoError(t, err)
		wg.Wait()
	})
	fakeRsync.Start()
	defer fakeRsync.Close()

	upstreamAddr := fakeRsync.Listener.Addr().String()
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
		if len(infos) != 1 {
			return false
		}
		return infos[0].snapshot().Upstream == "u1"
	}, time.Second, 10*time.Millisecond)

	resp, err := testHTTPClient().Get("http://" + srv.HTTPListener.Addr().String() + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, text, "rsync_proxy_active_connections 1\n")
	assert.Contains(t, text, "rsync_proxy_active_connections_by_module{module=\"fake\",upstream=\"u1\"} 1\n")
	assert.Contains(t, text, "rsync_proxy_connection_sent_bytes{index=\"")
	assert.Contains(t, text, "module=\"fake\"")
	assert.Contains(t, text, "upstream=\"u1\"")
	assert.Contains(t, text, "rsync_proxy_connection_received_bytes{index=\"")
	assert.Contains(t, text, "rsync_proxy_connection_connected_timestamp_seconds{index=\"")
	assert.Contains(t, text, "rsync_proxy_connection_duration_seconds{index=\"")
	assert.NotContains(t, text, rawConn.LocalAddr().String())
	assert.NotContains(t, text, upstreamAddr)

	wg.Done()
}

func TestMetricsIncludesQueueGauges(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	// Configure two upstreams with different queue capacities to verify
	// the gauges are emitted per upstream and reflect configuration.
	srv.reloadLock.Lock()
	srv.upstreams = []upstreamConfig{
		{Name: "u1", MaxActiveConns: 1, MaxQueuedConns: 2},
		{Name: "u2", MaxActiveConns: 0, MaxQueuedConns: 0},
	}
	srv.upstreamQueues = map[string]*queue.Queue{
		"u1": queue.New(1, 2),
		"u2": queue.New(0, 0),
	}
	srv.reloadLock.Unlock()

	resp, err := testHTTPClient().Get("http://" + srv.HTTPListener.Addr().String() + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// queued_connections gauge: nothing queued yet.
	assert.Contains(t, text, "# HELP rsync_proxy_queued_connections")
	assert.Contains(t, text, "# TYPE rsync_proxy_queued_connections gauge")
	assert.Contains(t, text, "rsync_proxy_queued_connections{upstream=\"u1\"} 0\n")
	assert.Contains(t, text, "rsync_proxy_queued_connections{upstream=\"u2\"} 0\n")

	// queue_active_max gauge.
	assert.Contains(t, text, "# HELP rsync_proxy_queue_active_max")
	assert.Contains(t, text, "# TYPE rsync_proxy_queue_active_max gauge")
	assert.Contains(t, text, "rsync_proxy_queue_active_max{upstream=\"u1\"} 1\n")
	assert.Contains(t, text, "rsync_proxy_queue_active_max{upstream=\"u2\"} 0\n")

	// queue_queued_max gauge.
	assert.Contains(t, text, "# HELP rsync_proxy_queue_queued_max")
	assert.Contains(t, text, "# TYPE rsync_proxy_queue_queued_max gauge")
	assert.Contains(t, text, "rsync_proxy_queue_queued_max{upstream=\"u1\"} 2\n")
	assert.Contains(t, text, "rsync_proxy_queue_queued_max{upstream=\"u2\"} 0\n")

	// per-upstream failure counters initialized at zero.
	assert.Contains(t, text, "rsync_proxy_queue_full_rejected_total{upstream=\"u1\"} 0\n")
	assert.Contains(t, text, "rsync_proxy_queue_full_rejected_total{upstream=\"u2\"} 0\n")
	assert.Contains(t, text, "rsync_proxy_upstream_dial_errors_total{upstream=\"u1\"} 0\n")
	assert.Contains(t, text, "rsync_proxy_upstream_dial_errors_total{upstream=\"u2\"} 0\n")

	// unknown module counter (no label).
	assert.Contains(t, text, "rsync_proxy_unknown_module_requests_total 0\n")
}

func TestMetricsCountsQueueFullRejection(t *testing.T) {
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

	srv.reloadLock.Lock()
	srv.upstreams = []upstreamConfig{
		{Name: "u1", MaxActiveConns: 1, MaxQueuedConns: 1},
	}
	srv.modules = map[string][]Target{
		"fake": {{Upstream: "u1", Addr: upstream.Listener.Addr().String()}},
	}
	srv.upstreamQueues = map[string]*queue.Queue{"u1": queue.New(1, 1)}
	srv.reloadLock.Unlock()

	// First connection occupies the active slot.
	c1Raw, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	c1 := rsync.NewConn(c1Raw)
	defer c1.Close()
	_, err = doClientHandshake(c1, RsyncdServerVersion, "fake")
	require.NoError(t, err)

	// Second connection fills the queued slot.
	c2Raw, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	c2 := rsync.NewConn(c2Raw)
	defer c2.Close()
	_, err = doClientHandshake(c2, RsyncdServerVersion, "fake")
	require.NoError(t, err)
	_, err = c2.ReadLine()
	require.NoError(t, err)
	_, err = c2.ReadLine()
	require.NoError(t, err)

	// Third connection should be rejected with queue-full.
	c3Raw, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	c3 := rsync.NewConn(c3Raw)
	defer c3.Close()
	_, err = doClientHandshake(c3, RsyncdServerVersion, "fake")
	require.NoError(t, err)
	line, err := c3.ReadLine()
	require.NoError(t, err)
	require.Contains(t, line, "Server queue is full")

	require.Eventually(t, func() bool {
		return srv.getUpstreamCounters("u1").queueFull.Load() == 1
	}, time.Second, 10*time.Millisecond)

	resp, err := testHTTPClient().Get("http://" + srv.HTTPListener.Addr().String() + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)
	assert.Contains(t, text, "rsync_proxy_queue_full_rejected_total{upstream=\"u1\"} 1\n")

	release.Done()
}

func TestMetricsCountsUnknownModule(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	srv.reloadLock.Lock()
	srv.modules = map[string][]Target{}
	srv.upstreamQueues = map[string]*queue.Queue{}
	srv.upstreams = nil
	srv.reloadLock.Unlock()

	rawConn, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	conn := rsync.NewConn(rawConn)
	defer conn.Close()

	_, err = doClientHandshake(conn, RsyncdServerVersion, "does-not-exist")
	require.NoError(t, err)
	_, err = io.ReadAll(conn)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return srv.unknownModuleCount.Load() == 1
	}, time.Second, 10*time.Millisecond)

	resp, err := testHTTPClient().Get("http://" + srv.HTTPListener.Addr().String() + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)
	assert.Contains(t, text, "rsync_proxy_unknown_module_requests_total 1\n")
}

func TestMetricsIncludesGoRuntime(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	resp, err := testHTTPClient().Get("http://" + srv.HTTPListener.Addr().String() + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)

	// promhttp's default gatherer exposes Go runtime and process metrics.
	assert.Contains(t, text, "go_goroutines")
	assert.Contains(t, text, "go_gc_duration_seconds")
	// Our legacy text-format metrics should still be present after the
	// promhttp output (no "# EOF" terminator from OpenMetrics).
	assert.Contains(t, text, "rsync_proxy_active_connections")
}

func TestPrometheusConnectionGroupingUsesStructuredKey(t *testing.T) {
	srv := New()

	first := &ConnInfo{Index: 1, ConnectedAt: time.Unix(100, 0)}
	first.Module = "a\xffb"
	first.Upstream = "c"
	srv.connInfo.Store(first.Index, first)

	second := &ConnInfo{Index: 2, ConnectedAt: time.Unix(100, 0)}
	second.Module = "a"
	second.Upstream = "b\xffc"
	srv.connInfo.Store(second.Index, second)

	var buf bytes.Buffer
	srv.writePrometheusMetrics(&buf, time.Unix(101, 0))
	text := buf.String()

	assert.Contains(t, text, "rsync_proxy_active_connections_by_module{module=\"a\xffb\",upstream=\"c\"} 1\n")
	assert.Contains(t, text, "rsync_proxy_active_connections_by_module{module=\"a\",upstream=\"b\xffc\"} 1\n")
	assert.NotContains(t, text, "rsync_proxy_active_connections_by_module{module=\"a\",upstream=\"b\xffc\"} 2\n")
}

func TestPrometheusDurationIncludesFractionalSeconds(t *testing.T) {
	srv := New()
	conn := &ConnInfo{Index: 1, ConnectedAt: time.Unix(100, 0)}
	conn.Module = "fake"
	conn.Upstream = "127.0.0.1:873"
	srv.connInfo.Store(conn.Index, conn)

	var buf bytes.Buffer
	srv.writePrometheusMetrics(&buf, time.Unix(100, 250_000_000))

	assert.Contains(t, buf.String(), "rsync_proxy_connection_duration_seconds{index=\"1\",module=\"fake\",upstream=\"127.0.0.1:873\"} 0.250\n")
}

func TestMetricsIncludesLifetimeCounters(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	payload := []byte("payload from upstream\n")
	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
		_, _, err := doServerHandshake(conn, RsyncdServerVersion)
		require.NoError(t, err)
		_, err = conn.Write(payload)
		require.NoError(t, err)
	})
	fakeRsync.Start()
	defer fakeRsync.Close()

	upstreamAddr := fakeRsync.Listener.Addr().String()
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

	_, err = io.ReadAll(conn)
	require.NoError(t, err)
	conn.Close()

	require.Eventually(t, func() bool {
		return srv.GetActiveConnectionCount() == 0
	}, 3*time.Second, 10*time.Millisecond)

	resp, err := testHTTPClient().Get("http://" + srv.HTTPListener.Addr().String() + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, text, "# HELP rsync_proxy_accepted_connections_total")
	assert.Contains(t, text, "# TYPE rsync_proxy_accepted_connections_total counter")
	assert.Contains(t, text, "rsync_proxy_accepted_connections_total 1\n")
	assert.Contains(t, text, "# HELP rsync_proxy_completed_connections_total")
	assert.Contains(t, text, "# TYPE rsync_proxy_completed_connections_total counter")
	assert.Contains(t, text, "rsync_proxy_completed_connections_total 1\n")
	assert.Contains(t, text, "# HELP rsync_proxy_sent_bytes_total")
	assert.Contains(t, text, "# TYPE rsync_proxy_sent_bytes_total counter")
	assert.Contains(t, text, fmt.Sprintf("rsync_proxy_sent_bytes_total %d\n", len(payload)))
	assert.Contains(t, text, "# HELP rsync_proxy_received_bytes_total")
	assert.Contains(t, text, "# TYPE rsync_proxy_received_bytes_total counter")

	// Per-(module, upstream) lifetime counters.
	assert.Contains(t, text, "# HELP rsync_proxy_module_completed_connections_total")
	assert.Contains(t, text, "# TYPE rsync_proxy_module_completed_connections_total counter")
	assert.Contains(t, text, "rsync_proxy_module_completed_connections_total{module=\"fake\",upstream=\"u1\"} 1\n")
	assert.Contains(t, text, "# HELP rsync_proxy_module_sent_bytes_total")
	assert.Contains(t, text, "# TYPE rsync_proxy_module_sent_bytes_total counter")
	assert.Contains(t, text, fmt.Sprintf("rsync_proxy_module_sent_bytes_total{module=\"fake\",upstream=\"u1\"} %d\n", len(payload)))
	assert.Contains(t, text, "# HELP rsync_proxy_module_received_bytes_total")
	assert.Contains(t, text, "# TYPE rsync_proxy_module_received_bytes_total counter")
	assert.Contains(t, text, "rsync_proxy_module_received_bytes_total{module=\"fake\",upstream=\"u1\"} 0\n")
}

func TestPrometheusLabelValueEscaping(t *testing.T) {
	assert.Equal(t, `plain`, prometheusEscapeLabelValue("plain"))
	assert.Equal(t, `quote\"value`, prometheusEscapeLabelValue(`quote"value`))
	assert.Equal(t, `slash\\value`, prometheusEscapeLabelValue(`slash\value`))
	assert.Equal(t, `line\nbreak`, prometheusEscapeLabelValue("line\nbreak"))
	assert.Equal(t, `unknown`, prometheusLabelValueOrUnknown(""))
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
	accessLogPath := setupAccessLog(t, srv)

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

	logData, err := os.ReadFile(accessLogPath)
	require.NoError(t, err)
	assert.Contains(t, string(logData), "starts requesting module fake")
	assert.Contains(t, string(logData), "starts queueing for module fake")
	assert.Contains(t, string(logData), "queue full for module fake")
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

func TestModuleCountersNormalizeEmptyKeyToUnknown(t *testing.T) {
	srv := New()

	// Both empty and "unknown" inputs must point at the same internal
	// counter, so a scrape cannot emit two lines that share the same
	// rendered Prometheus label set.
	c1 := srv.getModuleCounters("", "")
	c2 := srv.getModuleCounters("unknown", "unknown")
	assert.Same(t, c1, c2)

	c1.completed.Add(7)

	// metrics.go uses prometheusEscapeLabelValue on the stored key only,
	// so the rendered output must show the normalized "unknown" value.
	var buf bytes.Buffer
	srv.writePrometheusMetrics(&buf, time.Now())
	text := buf.String()
	assert.Contains(t, text, "rsync_proxy_module_completed_connections_total{module=\"unknown\",upstream=\"unknown\"} 7\n")

	// And there should be exactly one such line — i.e. no second line with
	// an empty-string label rendered separately.
	assert.Equal(t, 1, strings.Count(text, "rsync_proxy_module_completed_connections_total{"))
}
