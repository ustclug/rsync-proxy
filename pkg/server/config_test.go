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
	err := s.ReadConfig(strings.NewReader(configContent))
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
	err := s.ReadConfig(strings.NewReader(configContent))
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
	err := s.ReadConfig(strings.NewReader(configContent))
	if err != nil {
		t.Fatalf("Load config: %s", err)
	}
	expectedMotd := "Proudly served by rsync-proxy\ntest newline"
	if !reflect.DeepEqual(expectedMotd, s.Motd) {
		t.Errorf("Wrong motd\nExpected: %#v\nGot: %#v\n", expectedMotd, s.modules)
	}
}

func TestLoadMaxConnectionsInConfig(t *testing.T) {
	s := New()
	configContent := `
[upstreams.u1]
address = "127.0.0.1:1234"
max_connections = 5
modules = ["foo1"]

[upstreams.u2]
address = "127.0.0.1:1235"
modules = ["bar1"]
`
	err := s.ReadConfig(strings.NewReader(configContent))
	if err != nil {
		t.Fatalf("Load config: %s", err)
	}

	// Check that queue manager has registered the upstreams
	addrs := s.queueManager.ListAddresses()
	if len(addrs) != 2 {
		t.Errorf("Expected 2 upstreams registered, got %d", len(addrs))
	}

	// Check queue info for u1 (with limit)
	active, max, waiting := s.queueManager.GetQueueInfo("127.0.0.1:1234")
	if max != 5 {
		t.Errorf("Expected max_connections=5 for u1, got %d", max)
	}
	if active != 0 {
		t.Errorf("Expected 0 active connections initially, got %d", active)
	}
	if waiting != 0 {
		t.Errorf("Expected 0 waiting connections initially, got %d", waiting)
	}

	// Check queue info for u2 (no limit, should be 0)
	_, max2, _ := s.queueManager.GetQueueInfo("127.0.0.1:1235")
	if max2 != 0 {
		t.Errorf("Expected max_connections=0 (unlimited) for u2, got %d", max2)
	}
}
