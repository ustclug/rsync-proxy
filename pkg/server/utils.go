package server

import (
	"fmt"
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

func writeProxyProtocolHeader(conn net.Conn, sourceAddr, destAddr net.Addr, timeout time.Duration) error {
	var (
		sourceIP, destIP     net.IP
		sourcePort, destPort int
	)
	switch sourceTCP := sourceAddr.(type) {
	case *net.TCPAddr:
		sourceIP, sourcePort = sourceTCP.IP, sourceTCP.Port
	case *net.UnixAddr:
		sourceIP, sourcePort = net.IPv4(127, 0, 0, 1), 0
	default:
		return fmt.Errorf("invalid source address type %T", sourceAddr)
	}

	switch destTCP := destAddr.(type) {
	case *net.TCPAddr:
		destIP, destPort = destTCP.IP, destTCP.Port
	case *net.UnixAddr:
		destIP, destPort = net.IPv4(127, 0, 0, 1), 0
	default:
		return fmt.Errorf("invalid destination address type %T", destAddr)
	}

	ipVersion := "TCP4"
	if sourceIP.To4() == nil {
		ipVersion = "TCP6"
	}
	proxyHeader := fmt.Sprintf("PROXY %s %s %s %d %d\r\n", ipVersion, sourceIP.String(), destIP.String(), sourcePort, destPort)
	_, err := writeWithTimeout(conn, []byte(proxyHeader), timeout)
	return err
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
