package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func writeWithTimeout(conn net.Conn, buf []byte, timeout time.Duration) (n int, err error) {
	if timeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	}
	n, err = conn.Write(buf)
	return
}

func netAddrToString(addr net.Addr) string {
	switch addr := addr.(type) {
	case *net.TCPAddr:
		return addr.IP.String()
	case *net.UnixAddr:
		return addr.String()
	default:
		return addr.String()
	}
}

func writeProxyProtocolHeader(conn net.Conn, sourceAddr, destAddr net.Addr, writeTimeout time.Duration) error {
	h, err := generateProxyProtocolHeader(sourceAddr, destAddr)
	if err != nil {
		return err
	}
	_, err = writeWithTimeout(conn, []byte(h), writeTimeout)
	return err
}

func generateProxyProtocolHeader(sourceAddr, destAddr net.Addr) (string, error) {
	var (
		sourceIP, destIP     net.IP
		sourcePort, destPort int
	)
	switch sourceTCP := sourceAddr.(type) {
	case *net.TCPAddr:
		sourceIP, sourcePort = sourceTCP.IP, sourceTCP.Port
	case *net.UnixAddr:
		sourceIP, sourcePort = net.IPv4(127, 0, 0, 1), 1
	default:
		return "", fmt.Errorf("invalid source address type %T", sourceAddr)
	}

	switch destTCP := destAddr.(type) {
	case *net.TCPAddr:
		destIP, destPort = destTCP.IP, destTCP.Port
	case *net.UnixAddr:
		destIP, destPort = net.IPv4(127, 0, 0, 1), 1
	default:
		return "", fmt.Errorf("invalid destination address type %T", destAddr)
	}

	ipVersion := "TCP4"
	if sourceIP.To4() == nil {
		ipVersion = "TCP6"
	}
	return fmt.Sprintf("PROXY %s %s %s %d %d\r\n", ipVersion, sourceIP.String(), destIP.String(), sourcePort, destPort), nil
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
