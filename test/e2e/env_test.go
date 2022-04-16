package e2e

import (
	"bytes"
	"context"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ustclug/rsync-proxy/cmd"
)

var e Env

func TestMain(m *testing.M) {
	err := e.Setup()
	if err != nil {
		panic(err)
	}

	code := m.Run()
	if code != 0 {
		e.GetRsyncProxyOutput()
	}

	e.Teardown()
	os.Exit(code)
}

func TestListModules(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f, err := ioutil.TempFile("", "rsync-proxy-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write(config1)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	proxyProg := cmd.New()
	proxyProg.SetArgs([]string{"--config", f.Name(), "--listen-addr", "127.0.0.1:19527", "--web.listen-addr", "127.0.0.1:19528"})
	// nolint:errcheck
	go proxyProg.ExecuteContext(ctx)

	outputBytes, err := exec.Command("rsync", "rsync://127.0.0.1:19527/").Output()
	if err != nil {
		t.Error(err)
	}
	output := string(outputBytes)
	expectedOutput := "bar\nfoo\n"
	if output != expectedOutput {
		t.Errorf("Unexpected output\nExpected: %s\nGot: %s", expectedOutput, output)
	}
}

func TestSyncSingleFile(t *testing.T) {
	t.Parallel()

	dst, err := ioutil.TempFile("", "rsync-proxy-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = dst.Close() // we won't write to it here
	defer os.Remove(dst.Name())

	err = exec.Command("rsync", "rsync://127.0.0.1:9527/bar/v3.2/data", dst.Name()).Run()
	if err != nil {
		t.Fatal(err)
	}

	got, err := ioutil.ReadFile(dst.Name())
	if err != nil {
		t.Fatal(err)
	}

	expected := []byte("3.2")
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("Unexpected content\nExpected: %s\nGot: %s", string(expected), string(got))
	}
}

func TestSyncDir(t *testing.T) {
	t.Parallel()

	dir, err := ioutil.TempDir("", "rsync-proxy-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	err = exec.Command("rsync", "-a", "rsync://127.0.0.1:9527/foo/v3.0/", dir).Run()
	if err != nil {
		t.Fatal(err)
	}

	names := []string{"data1", "data2"}
	data := [][]byte{[]byte("3.0.1"), []byte("3.0.2")}
	for i, name := range names {
		fp := filepath.Join(dir, name)
		expected := data[i]
		got, err := ioutil.ReadFile(fp)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(expected, got) {
			t.Errorf("Unexpected content in %s\nExpected: %s\nGot: %s", fp, string(expected), string(got))
		}
	}
}

// NOTE: do not mark reloading related tests as parallel
// TODO: run a separate instance for each test
func TestReloadConfig(t *testing.T) {
	err := e.UpdateRsyncProxyConfig(config2)
	if err != nil {
		t.Fatal(err)
	}
	var reloadOutput bytes.Buffer
	err = cmd.SendReloadRequest("127.0.0.1:9528", &reloadOutput, &reloadOutput)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reloadOutput.String(), "Successfully reloaded") {
		t.Errorf("Unexpeceted output: %s", reloadOutput.String())
	}

	outputBytes, err := exec.Command("rsync", "rsync://127.0.0.1:9527/").Output()
	if err != nil {
		t.Error(err)
	}
	output := string(outputBytes)
	expectedOutput := "bar\nbaz\nfoo\n"
	if output != expectedOutput {
		t.Errorf("Unexpected output\nExpected: %s\nGot: %s", expectedOutput, output)
	}

	dst, err := ioutil.TempFile("", "rsync-proxy-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = dst.Close() // we won't write to it here
	defer os.Remove(dst.Name())

	err = exec.Command("rsync", "rsync://127.0.0.1:9527/baz/v3.4/data", dst.Name()).Run()
	if err != nil {
		t.Fatal(err)
	}

	got, err := ioutil.ReadFile(dst.Name())
	if err != nil {
		t.Fatal(err)
	}

	expected := []byte("3.4")
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("Unexpected content\nExpected: %s\nGot: %s", string(expected), string(got))
	}
}

// NOTE: do not mark reloading related tests as parallel
// TODO: run a separate instance for each test
func TestReloadConfigWithDuplicatedModules(t *testing.T) {
	err := e.UpdateRsyncProxyConfig(config3)
	if err != nil {
		t.Fatal(err)
	}
	var reloadOutput bytes.Buffer
	err = cmd.SendReloadRequest("127.0.0.1:9528", &reloadOutput, &reloadOutput)
	if err == nil {
		t.Errorf("Unexpected success")
	}
	if !strings.Contains(reloadOutput.String(), "Failed to reload config") {
		t.Errorf("Unexpeceted output: %s", reloadOutput.String())
	}
}
