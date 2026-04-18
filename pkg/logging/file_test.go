package logging

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertFileContent(t *testing.T, filename, content string) {
	f, err := os.Open(filename)
	require.NoError(t, err)
	defer f.Close()
	fi, err := f.Stat()
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), fi.Size())

	buf := make([]byte, len(content))
	_, err = f.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, content, string(buf))
}

func TestFileLogger(t *testing.T) {
	f1, err := os.CreateTemp("", "test-*.log")
	require.NoError(t, err)
	f2, err := os.CreateTemp("", "test-*.log")
	require.NoError(t, err)
	n1 := f1.Name()
	n2 := f2.Name()
	f1.Close()
	f2.Close()
	defer os.Remove(n1)
	defer os.Remove(n2)

	l, err := NewFileLogger(n1)
	require.NoError(t, err)
	l.SetFlags(0) // don't worry about prefixes
	l.F("test test test")

	_ = os.Rename(n1, n2)
	err = l.SetFile(n1)
	require.NoError(t, err)
	l.F("vvvvvvvvvv")
	l.Close()

	assertFileContent(t, n2, "test test test\n")
	assertFileContent(t, n1, "vvvvvvvvvv\n")
}
