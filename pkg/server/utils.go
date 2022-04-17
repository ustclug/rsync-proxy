package server

import (
	"net"
	"time"
)

func writeWithTimeout(conn net.Conn, buf []byte, timeout time.Duration) (n int, err error) {
	if timeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	}
	n, err = conn.Write(buf)
	return
}

// readLine will read as much as it can until the last read character is a newline character.
func readLine(conn net.Conn, buf []byte, timeout time.Duration) (n int, err error) {
	max := len(buf)
	for {
		if timeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(timeout))
		}
		var nr int
		nr, err = conn.Read(buf[n:])
		n += nr
		if n > 0 && buf[n-1] == '\n' {
			return n, nil
		}
		if n == max {
			return n, nil
		}
		if err != nil {
			return
		}
	}
}
