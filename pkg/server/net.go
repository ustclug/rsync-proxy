package server

import (
	"context"
	"net"
	"os"
	"strings"
)

func listenTCPOrUnix(addr string) (net.Listener, error) {
	if strings.HasPrefix(addr, "/") {
		os.Remove(addr)
		return net.Listen("unix", addr)
	}
	return net.Listen("tcp", addr)
}

func dialContextTCPOrUnix(ctx context.Context, dialer net.Dialer, addr string) (net.Conn, error) {
	if strings.HasPrefix(addr, "/") {
		return dialer.DialContext(ctx, "unix", addr)
	}
	return dialer.DialContext(ctx, "tcp", addr)
}

func addDefaultTCPPort(addr string, defaultPort string) string {
	if strings.HasPrefix(addr, "/") {
		// don't touch Unix address
		return addr
	} else {
		_, _, err := net.SplitHostPort(addr)
		if err != nil {
			if addrErr, ok := err.(*net.AddrError); ok && addrErr.Err == "missing port in address" {
				return net.JoinHostPort(addr, defaultPort)
			}
			// invalid address, return as-is
		}
	}
	return addr
}

func validateTCPOrUnixAddr(addr string) error {
	if strings.HasPrefix(addr, "/") {
		_, err := net.ResolveUnixAddr("unix", addr)
		return err
	}
	_, err := net.ResolveTCPAddr("tcp", addr)
	return err
}

func closeRead(conn net.Conn, setLinger bool) error {
	if setLinger {
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			_ = tcpConn.SetLinger(0)
		}
	}

	if closeReader, ok := conn.(interface{ CloseRead() error }); ok {
		return closeReader.CloseRead()
	}
	return nil
}
