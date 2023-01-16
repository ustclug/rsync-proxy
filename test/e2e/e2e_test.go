package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ustclug/rsync-proxy/cmd"
	"github.com/ustclug/rsync-proxy/pkg/server"
)

func TestListModules(t *testing.T) {
	proxy := startProxy(t)

	r := require.New(t)

	outputBytes, err := newRsyncCommand(getRsyncPath(proxy, "/")).CombinedOutput()
	r.NoError(err)

	output := string(outputBytes)
	expectedOutput := "bar\nfoo\n"
	r.Equal(expectedOutput, output)

}

func TestSyncSingleFile(t *testing.T) {
	proxy := startProxy(t)

	r := require.New(t)

	dst, err := os.CreateTemp("", "rsync-proxy-e2e-*")
	r.NoError(err)
	_ = dst.Close() // we won't write to it here
	defer os.Remove(dst.Name())

	outputBytes, err := newRsyncCommand(getRsyncPath(proxy, "/bar/v3.2/data"), dst.Name()).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		r.NoError(err)
	}

	got, err := os.ReadFile(dst.Name())
	r.NoError(err)

	r.Equal("3.2", string(got))
}

func TestSyncDir(t *testing.T) {
	proxy := startProxy(t)

	r := require.New(t)

	dir, err := os.MkdirTemp("", "rsync-proxy-e2e-*")
	r.NoError(err)
	defer os.RemoveAll(dir)

	outputBytes, err := newRsyncCommand("-a", getRsyncPath(proxy, "/foo/v3.0/"), dir).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		r.NoError(err)
	}

	names := []string{"data1", "data2"}
	data := [][]byte{[]byte("3.0.1"), []byte("3.0.2")}
	for i, name := range names {
		fp := filepath.Join(dir, name)
		expected := data[i]
		got, err := os.ReadFile(fp)
		r.NoError(err)
		r.Equal(string(expected), string(got))
	}
}

func TestReloadConfig(t *testing.T) {
	r := require.New(t)
	dst, err := os.CreateTemp("", "rsync-proxy-e2e-*")
	r.NoError(err)
	r.NoError(dst.Close())

	r.NoError(copyFile(getProxyConfigPath("config1.toml"), dst.Name()))

	proxy := startProxy(t, func(s *server.Server) {
		s.ConfigPath = dst.Name()
	})

	r.NoError(copyFile(getProxyConfigPath("config2.toml"), dst.Name()))

	var reloadOutput bytes.Buffer
	err = cmd.SendReloadRequest(proxy.HTTPListenAddr, &reloadOutput, &reloadOutput)
	r.NoError(err)
	r.Contains(reloadOutput.String(), "Successfully reloaded")

	outputBytes, err := newRsyncCommand(getRsyncPath(proxy, "/")).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		r.NoError(err)
	}

	r.Equal("bar\nbaz\nfoo\n", string(outputBytes))

	tmpFile, err := os.CreateTemp("", "rsync-proxy-e2e-*")
	r.NoError(err)
	r.NoError(tmpFile.Close())
	defer os.Remove(tmpFile.Name())

	outputBytes, err = newRsyncCommand(getRsyncPath(proxy, "/baz/v3.4/data"), tmpFile.Name()).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		r.NoError(err)
	}

	got, err := os.ReadFile(tmpFile.Name())
	r.NoError(err)
	r.Equal("3.4", string(got))
}

func TestReloadConfigWithDuplicatedModules(t *testing.T) {
	r := require.New(t)
	dst, err := os.CreateTemp("", "rsync-proxy-e2e-*")
	r.NoError(err)
	r.NoError(dst.Close())

	r.NoError(copyFile(getProxyConfigPath("config1.toml"), dst.Name()))

	proxy := startProxy(t, func(s *server.Server) {
		s.ConfigPath = dst.Name()
	})

	r.NoError(copyFile(getProxyConfigPath("config3.toml"), dst.Name()))

	var reloadOutput bytes.Buffer
	err = cmd.SendReloadRequest(proxy.HTTPListenAddr, &reloadOutput, &reloadOutput)
	r.Error(err)
	r.Contains(reloadOutput.String(), "Failed to reload config")
}
