package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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

var errLineTooLong = errors.New("handshake line too long")

// readLine reads exactly one '\n'-terminated line (the returned slice includes
// the '\n') from br. Bytes the peer sent after the newline are NOT part of the
// returned line: they stay buffered in br and are handed to the relay later.
// A line that does not fit into br's buffer is an error instead of being
// returned without its terminator.
//
// The caller is responsible for the read deadline, so that a whole handshake
// phase can be bounded by a single absolute deadline instead of one timeout
// per received byte.
func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errLineTooLong
	}
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(line))
	copy(out, line)
	return out, nil
}

// isValidGreeting reports whether line is a single well-formed daemon greeting
// ("@RSYNCD: <ver>[.<sub>][ <digest list>]\n") made of printable ASCII only.
func isValidGreeting(line []byte) bool {
	if !bytes.HasPrefix(line, RsyncdVersionPrefix) || len(line) < len(RsyncdVersionPrefix)+3 {
		return false
	}
	if line[len(line)-1] != '\n' || line[len(RsyncdVersionPrefix)] != ' ' {
		return false
	}
	rest := line[len(RsyncdVersionPrefix)+1 : len(line)-1]
	if len(rest) == 0 || rest[0] < '0' || rest[0] > '9' {
		return false
	}
	for _, c := range rest {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// isValidModuleName rejects names that can never be a configured rsyncd
// module and that are dangerous to copy into line-oriented logs or terminals:
// the empty name and anything containing an ASCII control character (CR, LF,
// NUL, ESC, DEL, ...). Everything else is decided by the module table lookup;
// bytes that are not valid UTF-8 are deliberately tolerated because module
// names are plain byte strings for rsync.
func isValidModuleName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func listenTCPOrUnix(addr string) (net.Listener, error) {
	if strings.HasPrefix(addr, "/") {
		os.Remove(addr)
		l, err := net.Listen("unix", addr)
		if err != nil {
			return l, err
		}
		err = os.Chmod(addr, 0o660)
		return l, err
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
