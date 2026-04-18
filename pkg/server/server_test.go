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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		"fake": {{Addr: fakeRsync.Listener.Addr().String()}},
	}

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
		"fake": {{Addr: fakeRsync.Listener.Addr().String()}},
	}

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
		"fake": {{Addr: fakeRsync.Listener.Addr().String()}},
	}

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
		"fake": {{Addr: upstreamAddr}},
	}

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
