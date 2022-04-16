package rsync

import (
	"errors"
	"fmt"
	"net"
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
