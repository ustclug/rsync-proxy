package server

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func (s *Server) ConfiguredListeners() [3]string { return s.configuredListeners }

// ListenInherited imports raw listeners, never TLS connection state. The
// descriptor order is plain rsync, optional TLS rsync, then management HTTP.
func (s *Server) ListenInherited(files []*os.File, expected [3]string) error {
	if s.configuredListeners != expected {
		return fmt.Errorf("listener addresses changed; hot upgrade requires unchanged listen/listen_tls/listen_http")
	}
	want := 2
	if s.TLSListenAddr != "" {
		want++
	}
	if len(files) != want {
		return fmt.Errorf("expected %d listener descriptors, got %d", want, len(files))
	}
	i := 0
	s.listenFactory = func(_ string) (net.Listener, error) {
		f := files[i]
		i++
		l, err := net.FileListener(f)
		if u, ok := l.(*net.UnixListener); ok {
			u.SetUnlinkOnClose(false)
		}
		return l, err
	}
	defer func() { s.listenFactory = nil }()
	return s.Listen()
}

func (s *Server) ListenerFiles() ([]*os.File, error) {
	var files []*os.File
	for _, l := range []net.Listener{s.TCPListener, s.rawTLSListener, s.HTTPListener} {
		if l == nil {
			continue
		}
		raw, ok := l.(syscall.Conn)
		if !ok {
			for _, f := range files {
				_ = f.Close()
			}
			return nil, fmt.Errorf("listener %T does not support handoff", l)
		}
		rc, err := raw.SyscallConn()
		fd := -1
		if err == nil {
			var dupErr error
			err = rc.Control(func(original uintptr) { fd, dupErr = unix.FcntlInt(original, unix.F_DUPFD_CLOEXEC, 0) })
			if err == nil {
				err = dupErr
			}
		}
		if err != nil {
			for _, f := range files {
				_ = f.Close()
			}
			return nil, err
		}
		// net.Listener.File followed by exec's File.Fd makes the shared socket
		// blocking. An unsuccessful/non-Go child would then wedge the old Accept.
		// NewFile preserves O_NONBLOCK when exec obtains the inherited descriptor.
		f := os.NewFile(uintptr(fd), "listener")
		files = append(files, f)
	}
	return files, nil
}

func (s *Server) DrainTimeout() time.Duration {
	s.reloadLock.RLock()
	defer s.reloadLock.RUnlock()
	return s.UpgradeDrainTimeout
}

func (s *Server) ReopenLogs() error {
	if err := s.accessLog.Reopen(); err != nil {
		return err
	}
	if err := s.accessJSONLog.Reopen(); err != nil {
		return err
	}
	return s.errorLog.Reopen()
}

func (s *Server) CloseLogs() { s.accessLog.Close(); s.accessJSONLog.Close(); s.errorLog.Close() }
