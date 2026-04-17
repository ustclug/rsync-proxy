package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ustclug/rsync-proxy/cmd"
	"github.com/ustclug/rsync-proxy/pkg/server"
)

type tlsCertFiles struct {
	certPath string
	keyPath  string
}

func writeTestTLSCert(t *testing.T, dir, name, commonName string) tlsCertFiles {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial number: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: commonName,
		},
		NotBefore: time.Now().Add(-time.Minute),
		NotAfter:  time.Now().Add(time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		DNSNames: []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	certPath := filepath.Join(dir, fmt.Sprintf("%s.crt", name))
	keyPath := filepath.Join(dir, fmt.Sprintf("%s.key", name))
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0600))

	return tlsCertFiles{certPath: certPath, keyPath: keyPath}
}

func writeProxyTLSConfig(t *testing.T, configPath string, certFiles tlsCertFiles) {
	t.Helper()

	configContent := fmt.Sprintf(`
[proxy]
listen = "127.0.0.1:873"
listen_tls = "127.0.0.1:874"
listen_http = "127.0.0.1:9528"
tls_cert_file = %q
tls_key_file = %q

[upstreams.u1]
address = "127.0.0.1:1234"
modules = ["foo"]

[upstreams.u2]
address = "127.0.0.1:1235"
modules = ["bar"]
`, certFiles.certPath, certFiles.keyPath)
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0600))
}

func newRsyncSSLCommand(certPath string, args ...string) *exec.Cmd {
	cmd := exec.Command("rsync-ssl", args...)
	cmd.Env = append(os.Environ(),
		"RSYNC_SSL_TYPE=openssl",
		"RSYNC_SSL_CA_CERT="+certPath,
	)
	return cmd
}

func normalizeRsyncSSLOutput(output []byte) string {
	lines := strings.Split(string(output), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "verify depth is "):
			continue
		case strings.HasPrefix(line, "Connecting to "):
			continue
		default:
			filtered = append(filtered, line)
		}
	}
	return strings.TrimSuffix(strings.Join(filtered, "\n"), "\n") + "\n"
}

func getRsyncTLSPath(s *server.Server, path string) string {
	_, port, err := net.SplitHostPort(s.TLSListenAddr)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("rsync://localhost:%s%s", port, path)
}

func ensureTLSPortIsReady(t *testing.T, addr string) {
	t.Helper()

	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	require.NoError(t, ensureTCPPortIsReady(ctx, port))
}

func TestTLSListModules(t *testing.T) {
	r := require.New(t)
	dir := t.TempDir()
	tlsFiles := writeTestTLSCert(t, dir, "server", "rsync-proxy-e2e")
	configPath := filepath.Join(dir, "config.toml")
	writeProxyTLSConfig(t, configPath, tlsFiles)

	proxy := startProxy(t, func(s *server.Server) {
		s.ConfigPath = configPath
		s.TLSListenAddr = "127.0.0.1:0"
	})
	ensureTLSPortIsReady(t, proxy.TLSListenAddr)

	outputBytes, err := newRsyncSSLCommand(tlsFiles.certPath, getRsyncTLSPath(proxy, "/")).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		r.NoError(err)
	}

	r.Equal("bar\nfoo\n", normalizeRsyncSSLOutput(outputBytes))
}

func TestReloadTLSCertificateE2E(t *testing.T) {
	r := require.New(t)
	dir := t.TempDir()
	firstCert := writeTestTLSCert(t, dir, "first", "first-cert")
	secondCert := writeTestTLSCert(t, dir, "second", "second-cert")
	configPath := filepath.Join(dir, "config.toml")
	writeProxyTLSConfig(t, configPath, firstCert)

	proxy := startProxy(t, func(s *server.Server) {
		s.ConfigPath = configPath
		s.TLSListenAddr = "127.0.0.1:0"
	})
	ensureTLSPortIsReady(t, proxy.TLSListenAddr)

	outputBytes, err := newRsyncSSLCommand(firstCert.certPath, getRsyncTLSPath(proxy, "/")).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		r.NoError(err)
	}
	r.Equal("bar\nfoo\n", normalizeRsyncSSLOutput(outputBytes))

	writeProxyTLSConfig(t, configPath, secondCert)

	var reloadOutput bytes.Buffer
	err = cmd.SendReloadRequest(proxy.HTTPListenAddr, &reloadOutput, &reloadOutput)
	r.NoError(err)
	r.Contains(reloadOutput.String(), "Successfully reloaded")

	outputBytes, err = newRsyncSSLCommand(firstCert.certPath, getRsyncTLSPath(proxy, "/")).CombinedOutput()
	r.Error(err)

	outputBytes, err = newRsyncSSLCommand(secondCert.certPath, getRsyncTLSPath(proxy, "/")).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		r.NoError(err)
	}
	r.Equal("bar\nfoo\n", normalizeRsyncSSLOutput(outputBytes))
}
