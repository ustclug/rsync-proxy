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

	srv.modules = map[string]string{
		"fake": fakeRsync.Listener.Addr().String(),
	}

	rawConn, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	conn := rsync.NewConn(rawConn)
	defer conn.Close()

	_, err = doClientHandshake(conn, RsyncdServerVersion, "fake")
	require.NoError(t, err)

	allData, err := io.ReadAll(conn)
	require.NoError(t, err)

	assert.Equal(t, proxyMotd+"\n"+serverMotd, string(allData))
}

// See also: https://github.com/ustclug/rsync-proxy/commit/d581c18dab8008c5bc9c1a5d667b49d67a4edfed
func TestClientReadTimeout(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()

		_, _, err := doServerHandshake(conn, RsyncdServerVersion)
		if err != nil {
			return
		}

		for i := 0; i < 3; i++ {
			_, err = conn.Write([]byte("data\n"))
			if err != nil {
				return
			}
			time.Sleep(srv.ReadTimeout)
		}
	})
	fakeRsync.Start()
	defer fakeRsync.Close()

	srv.modules = map[string]string{
		"fake": fakeRsync.Listener.Addr().String(),
	}

	rawConn, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	conn := rsync.NewConn(rawConn)
	defer conn.Close()

	_, err = doClientHandshake(conn, RsyncdServerVersion, "fake")
	require.NoError(t, err)

	allData, err := io.ReadAll(conn)
	require.NoError(t, err)

	expected := strings.Repeat("data\n", 3)
	assert.Equal(t, expected, string(allData))
}

func TestTLSRsyncListener(t *testing.T) {
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
	require.NoError(t, err)
	srv.tlsCertificate = &cert
	srv.tlsConfig.GetCertificate = srv.getTLSCertificate
	err = srv.Listen()
	require.NoError(t, err)
	defer srv.Close()

	go func() {
		err := srv.Run()
		assert.NoErrorf(t, err, "Fail to run server")
	}()

	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()

		_, module, err := doServerHandshake(conn, RsyncdServerVersion)
		if err != nil {
			return
		}
		assert.Equal(t, "fake\n", module)
	})
	fakeRsync.Start()
	defer fakeRsync.Close()

	srv.modules = map[string]string{
		"fake": fakeRsync.Listener.Addr().String(),
	}

	pool := x509.NewCertPool()
	certPEM, err := os.ReadFile(tlsFiles.certPath)
	require.NoError(t, err)
	pool.AppendCertsFromPEM(certPEM)

	rawConn, err := tls.Dial("tcp", srv.TLSListenAddr, &tls.Config{
		RootCAs:    pool,
		ServerName: "localhost",
	})
	require.NoError(t, err)
	conn := rsync.NewConn(rawConn)
	defer conn.Close()

	_, err = doClientHandshake(conn, RsyncdServerVersion, "fake")
	require.NoError(t, err)
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
