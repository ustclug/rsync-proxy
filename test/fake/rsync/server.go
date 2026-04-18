package rsync

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

type Server struct {
	handler func(conn *Conn)

	Listener net.Listener
}

func NewServer(handler func(conn *Conn)) *Server {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("fakersyncd: fail to listen: %v", err))
	}
	return &Server{
		handler:  handler,
		Listener: l,
	}
}

func (r *Server) Start() {
	go r.handleConn()
}

func (r *Server) handleConn() {
	for {
		c, err := r.Listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				panic(fmt.Sprintf("fakersyncd: fail to accept connection: %v", err))
			}
			return
		}
		go r.handler(NewConn(c))
	}
}

func (r *Server) Close() {
	_ = r.Listener.Close()
}

func NewModuleListServer(modules []string) *Server {
	return NewServer(func(conn *Conn) {
		defer conn.Close()

		if _, err := conn.ReadLine(); err != nil {
			return
		}
		_, _ = conn.Write([]byte("@RSYNCD: 32.0 sha512 sha256 sha1 md5 md4\n"))

		line, err := conn.ReadLine()
		if err != nil || line != "\n" {
			return
		}

		for _, module := range modules {
			_, _ = conn.Write([]byte(module + "\t" + strings.ToUpper(module) + "\n"))
		}
		_, _ = conn.Write([]byte("@RSYNCD: EXIT\n"))
	})
}
