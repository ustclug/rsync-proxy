package server

import (
	"reflect"
	"strings"
	"testing"
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
	if err != nil {
		t.Fatalf("Load config: %s", err)
	}
	expectedMods := map[string]string{
		"foo1": "127.0.0.1:1234",
		"foo2": "127.0.0.1:1234",
		"bar1": "127.0.0.1:1235",
		"bar2": "example.com:1235",
	}
	if !reflect.DeepEqual(expectedMods, s.modules) {
		t.Errorf("Wrong modules\nExpected: %#v\nGot: %#v\n", expectedMods, s.modules)
	}
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
	if err == nil {
		t.Fatalf("Unexpected success")
	}
	if !strings.Contains(err.Error(), "duplicate module name") {
		t.Errorf("Unexpected error. Got: %s", err)
	}
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
	if err != nil {
		t.Fatalf("Load config: %s", err)
	}
	expectedMotd := "Proudly served by rsync-proxy\ntest newline"
	if !reflect.DeepEqual(expectedMotd, s.Motd) {
		t.Errorf("Wrong motd\nExpected: %#v\nGot: %#v\n", expectedMotd, s.modules)
	}
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
	if err != nil {
		t.Fatalf("Load config: %s", err)
	}
	if s.TLSListenAddr != "127.0.0.1:8731" {
		t.Fatalf("Wrong tls listen addr: %s", s.TLSListenAddr)
	}
	if s.tlsCertificate == nil {
		t.Fatal("TLS certificate was not loaded")
	}
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
	if err == nil {
		t.Fatal("Unexpected success")
	}
	if !strings.Contains(err.Error(), "listen_tls requires tls_cert_file and tls_key_file") {
		t.Fatalf("Unexpected error. Got: %s", err)
	}
}
