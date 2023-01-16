package logging

import (
	"os"
	"testing"
)

func assertFileContent(t *testing.T, filename, content string) {
	f, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != int64(len(content)) {
		t.Fatalf("file size is %d, want %d", fi.Size(), len(content))
	}

	buf := make([]byte, len(content))
	_, err = f.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
}

func TestFileLogger(t *testing.T) {
	f1, err := os.CreateTemp("", "test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	f2, err := os.CreateTemp("", "test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	n1 := f1.Name()
	n2 := f2.Name()
	f1.Close()
	f2.Close()
	defer os.Remove(n1)
	defer os.Remove(n2)

	l, err := NewFileLogger(n1)
	if err != nil {
		t.Fatal(err)
	}
	l.SetFlags(0) // don't worry about prefixes
	l.F("test test test")

	_ = os.Rename(n1, n2)
	err = l.SetFile(n1)
	if err != nil {
		t.Fatal(err)
	}
	l.F("vvvvvvvvvv")
	l.Close()

	assertFileContent(t, n2, "test test test\n")
	assertFileContent(t, n1, "vvvvvvvvvv\n")
}
