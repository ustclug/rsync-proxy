package rsync

import (
	"bufio"
	"net"
)

type Conn struct {
	br   *bufio.Reader
	conn net.Conn
}

func (c *Conn) Read(b []byte) (int, error) {
	return c.br.Read(b)
}

func (c *Conn) Write(b []byte) (int, error) {
	return c.conn.Write(b)
}

func (c *Conn) ReadLine() (string, error) {
	return c.br.ReadString('\n')
}

func (c *Conn) Close() error {
	return c.conn.Close()
}

func NewConn(c net.Conn) *Conn {
	return &Conn{
		br:   bufio.NewReader(c),
		conn: c,
	}
}
