package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadConfig(t *testing.T) {
	s := New()
	configContent := `
[upstreams.u1]
address = "127.0.0.1:1234"
modules = ["foo1", "foo2"]

[upstreams.u2]
address = "127.0.0.1:1235"
modules = ["bar1"]

[upstreams.u3]
address = "example.com:1235"
modules = ["bar2"]
`
	err := s.ReadConfig(strings.NewReader(configContent), true)
	require.NoError(t, err, "load config")
	expectedMods := map[string]string{
		"foo1": "127.0.0.1:1234",
		"foo2": "127.0.0.1:1234",
		"bar1": "127.0.0.1:1235",
		"bar2": "example.com:1235",
	}
	assert.Equal(t, expectedMods, s.modules, "wrong modules")
}

func TestDuplicatedModulesInConfig(t *testing.T) {
	s := New()
	configContent := `
[upstreams.u1]
address = "127.0.0.1:1234"
modules = ["foo1", "foo2"]

[upstreams.u2]
address = "127.0.0.1:1235"
modules = ["foo1"]
`
	err := s.ReadConfig(strings.NewReader(configContent), true)
	require.Error(t, err, "unexpected success")
	assert.Contains(t, err.Error(), "duplicate module name", "unexpected error message")
}

func TestLoadMotdInConfig(t *testing.T) {
	s := New()
	configContent := `
[proxy]
motd = "Proudly served by rsync-proxy\ntest newline"

[upstreams.u1]
address = "127.0.0.1:1234"
modules = ["foo1", "foo2"]
`
	err := s.ReadConfig(strings.NewReader(configContent), true)
	require.NoError(t, err, "load config")
	expectedMotd := "Proudly served by rsync-proxy\ntest newline"
	assert.Equal(t, expectedMotd, s.Motd, "wrong modules")
}

func TestLoadTLSConfig(t *testing.T) {
	tlsFiles := writeTestTLSCert(t, t.TempDir(), "server", "rsync-proxy-test")

	s := New()
	configContent := `
[proxy]
listen_tls = "127.0.0.1:8731"
tls_cert_file = "` + tlsFiles.certPath + `"
tls_key_file = "` + tlsFiles.keyPath + `"

[upstreams.u1]
address = "127.0.0.1:1234"
modules = ["foo1"]
`
	err := s.ReadConfig(strings.NewReader(configContent), true)
	require.NoError(t, err, "load config")
	assert.Equal(t, "127.0.0.1:8731", s.TLSListenAddr, "wrong TLS listen addr")
	assert.NotNil(t, s.tlsCertificate, "no tls cert")
}

func TestLoadTLSConfigWithoutKeyPair(t *testing.T) {
	s := New()
	configContent := `
[proxy]
listen_tls = "127.0.0.1:8731"

[upstreams.u1]
address = "127.0.0.1:1234"
modules = ["foo1"]
`
	err := s.ReadConfig(strings.NewReader(configContent), true)
	require.Error(t, err, "load config")
	assert.Contains(t, err.Error(), "listen_tls requires tls_cert_file and tls_key_file", "unexpected error message")
}
