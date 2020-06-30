package e2e

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ustclug/rsync-proxy/pkg/log"
)

func TestMain(m *testing.M) {
	var e Env
	err := e.Setup()
	if err != nil {
		log.Fatalln(err)
	}

	code := m.Run()

	e.Teardown()
	os.Exit(code)
}

func TestListModules(t *testing.T) {
	outputBytes, err := exec.Command("rsync", "rsync://127.0.0.1:9527/").Output()
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
