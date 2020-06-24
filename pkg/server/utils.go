package server

import (
	"net"
	"time"
)

func (s *Server) readWithTimeout(conn net.Conn, buf []byte) (n int, err error) {
	if s.ReadTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(s.ReadTimeout))
	}
	n, err = conn.Read(buf)
	return
}

func (s *Server) writeWithTimeout(conn net.Conn, buf []byte) (n int, err error) {
	if s.WriteTimeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(s.ReadTimeout))
	}
	n, err = conn.Write(buf)
	return
}
