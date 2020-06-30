package server

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	s := New()
	configContent := `
[upstreams.u1]
host = "127.0.0.1"
port = 1234
modules = ["foo1", "foo2"]

[upstreams.u2]
host = "127.0.0.1"
port = 1235
modules = ["bar1"]

[upstreams.u3]
host = "example.com"
port = 1235
modules = ["bar2"]
`
	err := s.LoadConfig(strings.NewReader(configContent))
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
host = "127.0.0.1"
port = 1234
modules = ["foo1", "foo2"]

[upstreams.u2]
host = "127.0.0.1"
port = 1235
modules = ["foo1"]
`
	err := s.LoadConfig(strings.NewReader(configContent))
	if err == nil {
		t.Fatalf("Unexpected success")
	}
	if !strings.Contains(err.Error(), "duplicated module name") {
		t.Errorf("Unexpected error. Got: %s", err)
	}
}
