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

func readLine(conn net.Conn, buf []byte, timeout time.Duration) (n int, err error) {
	// 这个只是特殊场景下的 readLine
	// rsync 在握手过程中除了 protocol version 跟 module name 以外并不会传输其他数据，而这些数据又是以 '\n' 分割
	// 所以可以直接尽力读满传进来的 buffer 直到读到 '\n' 为止
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
