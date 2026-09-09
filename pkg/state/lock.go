package state

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Lock is an open-file-description lock. It must not be passed to children.
// Closing the file (including process death) releases it; no PID/heartbeat
// heuristic can mistakenly reclaim a still-live generation's tickets.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func IsLocked(path string) (bool, error) {
	f, err := Lock(path)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, f.Close()
}
