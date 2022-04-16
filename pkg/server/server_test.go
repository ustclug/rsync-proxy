package server

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ustclug/rsync-proxy/test/fake/rsync"
)

func startServer(t *testing.T) *Server {
	srv := New()
	const (
		addr    = "127.0.0.1:0"
		timeout = time.Second
	)
	srv.WebListenAddr = addr
	srv.ListenAddr = addr
	srv.ReadTimeout = timeout
	srv.WriteTimeout = timeout
	err := srv.Listen()
	require.NoErrorf(t, err, "Fail to listen")

	go func() {
		t.Logf("rsync-proxy is running on: %s", srv.TCPListener.Addr())
		err := srv.Run()
		assert.NoErrorf(t, err, "Fail to run server")
	}()
	return srv
}

func doClientHandshake(conn *rsync.Conn, version []byte, module string) (svrVersion string, err error) {
	_, err = conn.Write(version)
	if err != nil {
		return
	}

	svrVersion, err = conn.ReadLine()
	if err != nil {
		return
	}

	_, err = conn.Write([]byte(module + "\n"))
	return
}

func doServerHandshake(conn *rsync.Conn, data []byte) (cliVersion, module string, err error) {
	// read protocol version from client
	cliVersion, err = conn.ReadLine()
	if err != nil {
		return
	}

	_, err = conn.Write(data)
	if err != nil {
		return
	}

	// read module name from client
	module, err = conn.ReadLine()
	return
}

// See also: https://github.com/ustclug/rsync-proxy/issues/11
func TestMotdFromServer(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()
	proxyMotd := "Hello\n"
	srv.Motd = proxyMotd

	l := strings.Repeat("a", TCPBufferSize)
	serverMotd := fmt.Sprintf("%s\n%s\n\n", l, l)

	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()

		_, _, err := doServerHandshake(conn, append(RsyncdServerVersion, []byte(serverMotd)...))
		if err != nil {
			t.Errorf("server handshake: %v", err)
		}
	})
	fakeRsync.Start()
	defer fakeRsync.Close()

	srv.modules = map[string]string{
		"fake": fakeRsync.Listener.Addr().String(),
	}

	r := require.New(t)

	rawConn, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	r.NoError(err)
	conn := rsync.NewConn(rawConn)
	defer conn.Close()

	_, err = doClientHandshake(conn, RsyncdServerVersion, "fake")
	r.NoError(err)

	allData, err := io.ReadAll(conn)
	r.NoError(err)

	r.Equal(proxyMotd+"\n"+serverMotd, string(allData))
}

// See also: https://github.com/ustclug/rsync-proxy/commit/d581c18dab8008c5bc9c1a5d667b49d67a4edfed
func TestClientReadTimeout(t *testing.T) {
	srv := startServer(t)
	defer srv.Close()

	fakeRsync := rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()

		_, _, err := doServerHandshake(conn, RsyncdServerVersion)
		if err != nil {
			t.Errorf("server handshake: %v", err)
			return
		}

		for i := 0; i < 3; i++ {
			_, err = conn.Write([]byte("data\n"))
			if err != nil {
				t.Errorf("write data: %v", err)
				return
			}
			time.Sleep(srv.ReadTimeout)
		}
	})
	fakeRsync.Start()
	defer fakeRsync.Close()

	srv.modules = map[string]string{
		"fake": fakeRsync.Listener.Addr().String(),
	}

	r := require.New(t)

	rawConn, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	r.NoError(err)
	conn := rsync.NewConn(rawConn)
	defer conn.Close()

	_, err = doClientHandshake(conn, RsyncdServerVersion, "fake")
	r.NoError(err)

	allData, err := io.ReadAll(conn)
	r.NoError(err)

	expected := strings.Repeat("data\n", 3)
	r.Equal(expected, string(allData))
}
