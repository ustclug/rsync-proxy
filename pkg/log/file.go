package log

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

type FileLogger struct {
	filename string
	f        *os.File
	l        *log.Logger
	mu       sync.Mutex
}

func NewFileLogger(filename string) (l *FileLogger, err error) {
	l = &FileLogger{
		filename: filename,
		f:        nil,
		l:        log.New(io.Discard, "", log.Ldate|log.Ltime|log.Lshortfile),
	}

	if filename != "" {
		if err := l.reopen(); err != nil {
			l = nil
		}
	}
	return
}

func (l *FileLogger) F(format string, a ...any) {
	l.l.Printf(format, a...)
}

func (l *FileLogger) Ln(a ...any) {
	l.l.Println(a...)
}

// A mutex-free version
func (l *FileLogger) reopen() error {
	if l.filename == "" {
		if l.f != nil {
			l.f.Close()
		}
		l.f = nil
		l.l.SetOutput(io.Discard)
		return nil
	}

	err := os.MkdirAll(filepath.Dir(l.filename), 0755)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(l.filename, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	if l.f != nil {
		l.f.Close()
	}
	l.f = f
	l.l.SetOutput(f)
	return nil
}

func (l *FileLogger) Reopen() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reopen()
}

func (l *FileLogger) SetFile(filename string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.filename = filename
	return l.reopen()
}
