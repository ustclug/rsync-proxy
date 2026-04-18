package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ustclug/rsync-proxy/cmd"
	"github.com/ustclug/rsync-proxy/pkg/server"
)

func TestListModules(t *testing.T) {
	proxy := startProxy(t)

	outputBytes, err := newRsyncCommand(getRsyncPath(proxy, "/")).CombinedOutput()
	require.NoError(t, err)

	output := string(outputBytes)
	expectedOutput := "bar\nfoo\n"
	assert.Equal(t, expectedOutput, output)
}

func TestSyncSingleFile(t *testing.T) {
	proxy := startProxy(t)

	dst, err := os.CreateTemp("", "rsync-proxy-e2e-*")
	require.NoError(t, err)
	_ = dst.Close() // we won't write to it here
	defer os.Remove(dst.Name())

	outputBytes, err := newRsyncCommand(getRsyncPath(proxy, "/bar/v3.2/data"), dst.Name()).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		require.NoError(t, err)
	}

	got, err := os.ReadFile(dst.Name())
	require.NoError(t, err)

	assert.Equal(t, "3.2", string(got))
}

func TestSyncDir(t *testing.T) {
	proxy := startProxy(t)

	dir, err := os.MkdirTemp("", "rsync-proxy-e2e-*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	outputBytes, err := newRsyncCommand("-a", getRsyncPath(proxy, "/foo/v3.0/"), dir).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		require.NoError(t, err)
	}

	names := []string{"data1", "data2"}
	data := [][]byte{[]byte("3.0.1"), []byte("3.0.2")}
	for i, name := range names {
		fp := filepath.Join(dir, name)
		expected := data[i]
		got, err := os.ReadFile(fp)
		require.NoError(t, err)
		assert.Equal(t, string(expected), string(got))
	}
}

func TestReloadConfig(t *testing.T) {
	dst, err := os.CreateTemp("", "rsync-proxy-e2e-*")
	require.NoError(t, err)
	require.NoError(t, dst.Close())

	require.NoError(t, copyFile(getProxyConfigPath("config1.toml"), dst.Name()))

	proxy := startProxy(t, func(s *server.Server) {
		s.ConfigPath = dst.Name()
	})

	require.NoError(t, copyFile(getProxyConfigPath("config2.toml"), dst.Name()))

	var reloadOutput bytes.Buffer
	err = cmd.SendReloadRequest(proxy.HTTPListenAddr, &reloadOutput, &reloadOutput)
	require.NoError(t, err)
	assert.Contains(t, reloadOutput.String(), "Successfully reloaded")

	outputBytes, err := newRsyncCommand(getRsyncPath(proxy, "/")).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		require.NoError(t, err)
	}

	assert.Equal(t, "bar\nbaz\nfoo\n", string(outputBytes))

	tmpFile, err := os.CreateTemp("", "rsync-proxy-e2e-*")
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())
	defer os.Remove(tmpFile.Name())

	outputBytes, err = newRsyncCommand(getRsyncPath(proxy, "/baz/v3.4/data"), tmpFile.Name()).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		require.NoError(t, err)
	}

	got, err := os.ReadFile(tmpFile.Name())
	require.NoError(t, err)
	assert.Equal(t, "3.4", string(got))
}

func TestReloadConfigWithDuplicatedModules(t *testing.T) {
	dst, err := os.CreateTemp("", "rsync-proxy-e2e-*")
	require.NoError(t, err)
	require.NoError(t, dst.Close())

	require.NoError(t, copyFile(getProxyConfigPath("config1.toml"), dst.Name()))

	proxy := startProxy(t, func(s *server.Server) {
		s.ConfigPath = dst.Name()
	})

	require.NoError(t, copyFile(getProxyConfigPath("config3.toml"), dst.Name()))

	var reloadOutput bytes.Buffer
	err = cmd.SendReloadRequest(proxy.HTTPListenAddr, &reloadOutput, &reloadOutput)
	assert.Error(t, err)
	assert.Contains(t, reloadOutput.String(), "Failed to reload config")
}

func TestProxyProtocol(t *testing.T) {
	dst, err := os.CreateTemp("", "rsync-proxy-e2e-*")
	require.NoError(t, err)
	require.NoError(t, dst.Close())

	require.NoError(t, copyFile(getProxyConfigPath("config4.toml"), dst.Name()))

	proxy := startProxy(t, func(s *server.Server) {
		s.ConfigPath = dst.Name()
	})

	tmpFile, err := os.CreateTemp("", "rsync-proxy-e2e-*")
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())
	defer os.Remove(tmpFile.Name())

	outputBytes, err := newRsyncCommand(getRsyncPath(proxy, "/pro/v3.5/data"), tmpFile.Name()).CombinedOutput()
	if err != nil {
		t.Log(string(outputBytes))
		require.NoError(t, err)
	}

	got, err := os.ReadFile(tmpFile.Name())
	require.NoError(t, err)
	assert.Equal(t, "3.5", string(got))
}
