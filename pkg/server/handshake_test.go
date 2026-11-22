package server

import (
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ustclug/rsync-proxy/pkg/queue"
	"github.com/ustclug/rsync-proxy/test/fake/rsync"
)

// recordingUpstream remembers every byte each connection received from the
// proxy during the first few hundred milliseconds.
type recordingUpstream struct {
	*rsync.Server
	mu   sync.Mutex
	seen []string
}

func newRecordingUpstream() *recordingUpstream {
	u := &recordingUpstream{}
	u.Server = rsync.NewServer(func(conn *rsync.Conn) {
		defer conn.Close()
		_, _ = conn.Write(RsyncdServerVersion)

		u.mu.Lock()
		idx := len(u.seen)
		u.seen = append(u.seen, "")
		u.mu.Unlock()

		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, 4096)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					u.mu.Lock()
					u.seen[idx] += string(buf[:n])
					u.mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
	})
	return u
}

func (u *recordingUpstream) received() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

func setupSmugglingTest(t *testing.T) (*Server, *recordingUpstream, string) {
	t.Helper()
	srv := startServer(t)
	t.Cleanup(srv.Close)
	accessLogPath := setupAccessLog(t, srv)

	up := newRecordingUpstream()
	up.Start()
	t.Cleanup(up.Close)

	srv.reloadLock.Lock()
	srv.modules = map[string][]Target{
		"allowed": {{Upstream: "u1", Addr: up.Listener.Addr().String()}},
	}
	srv.upstreams = []upstreamConfig{{Name: "u1"}}
	srv.upstreamQueues = map[string]*queue.Queue{"u1": queue.New(0, 0)}
	srv.reloadLock.Unlock()
	return srv, up, accessLogPath
}

// sendChunks writes every chunk with a single Write call (one TCP segment on
// loopback) and returns everything the proxy answered.
func sendChunks(t *testing.T, srv *Server, chunks ...string) string {
	t.Helper()
	conn, err := net.Dial("tcp", srv.TCPListener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	for i, chunk := range chunks {
		_, err = conn.Write([]byte(chunk))
		require.NoError(t, err)
		if i == 0 {
			// wait for the proxy greeting before sending the "module line"
			buf := make([]byte, len(RsyncdServerVersion))
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			_, _ = io.ReadFull(conn, buf)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = conn.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	data, _ := io.ReadAll(conn)
	return string(data)
}

// The first chunk of a client must not be replayed verbatim to the upstream:
// only one greeting line may precede the module name the proxy validated.
func TestClientCannotSmuggleModuleInGreetingChunk(t *testing.T) {
	const greeting = "@RSYNCD: 32.0 sha512 sha256 sha1 md5 md4\n"
	earlyInput := greeting + "#early_input=205\n"
	earlyInput += strings.Repeat("A", ReadBufferSize-len(earlyInput))
	option := greeting + "secret\n--server\x00--sender\x00-re.iLsfxCIvu\x00--suffix="
	option += strings.Repeat("X", ReadBufferSize-len(option))

	for name, firstChunk := range map[string]string{
		"second line in the same segment":   greeting + "secret\n",
		"module list request":               greeting + "\n",
		"early_input swallows proxy module": earlyInput,
		"option value absorbs proxy module": option,
	} {
		t.Run(name, func(t *testing.T) {
			srv, up, _ := setupSmugglingTest(t)
			_ = sendChunks(t, srv, firstChunk, "allowed\n")

			require.Eventually(t, func() bool {
				return srv.GetActiveConnectionCount() == 0
			}, 3*time.Second, 10*time.Millisecond)
			// None of these first chunks is a greeting followed by a
			// configured module, so nothing at all may reach the upstream
			// (TestPipelinedGreetingAndModule is the positive control).
			assert.Empty(t, up.received(),
				"the upstream may only ever see one greeting line followed by the validated module")
		})
	}
}

// Pipelined but honest clients keep working: greeting and module in one
// segment are split into two lines instead of being glued together.
func TestPipelinedGreetingAndModule(t *testing.T) {
	srv, up, _ := setupSmugglingTest(t)
	_ = sendChunks(t, srv, "@RSYNCD: 31.0\nallowed\n--server\x00")

	const want = "@RSYNCD: 31.0\nallowed\n--server\x00"
	require.Eventually(t, func() bool {
		got := up.received()
		return len(got) == 1 && got[0] == want
	}, 3*time.Second, 10*time.Millisecond, "upstream saw: %q", up.received())
}

// A module "line" with embedded line feeds must not create extra access-log
// lines (which fail2ban would attribute to somebody else).
func TestModuleNameCannotForgeAccessLogLines(t *testing.T) {
	srv, _, accessLogPath := setupSmugglingTest(t)
	reply := sendChunks(t, srv, "@RSYNCD: 31.0\n",
		"x\nclient 203.0.113.77 requests non-existing module y\n")
	assert.Contains(t, reply, "@ERROR")

	require.Eventually(t, func() bool {
		return srv.GetActiveConnectionCount() == 0
	}, 3*time.Second, 10*time.Millisecond)
	logData, err := os.ReadFile(accessLogPath)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(logData)), "\n")
	require.Len(t, lines, 1, "exactly one access-log line per request: %q", logData)
	assert.Contains(t, lines[0], `requests non-existing module "x"`)
	assert.NotContains(t, string(logData), "203.0.113.77")
}

// Control characters never reach the text log unescaped.
func TestModuleNameControlCharactersAreEscapedInLog(t *testing.T) {
	srv, _, accessLogPath := setupSmugglingTest(t)
	reply := sendChunks(t, srv, "@RSYNCD: 31.0\n", "evil\r\x1b[2Jclient 203.0.113.77 requests non-existing module y\n")
	assert.Contains(t, reply, "@ERROR: invalid module name")

	require.Eventually(t, func() bool {
		return srv.GetActiveConnectionCount() == 0
	}, 3*time.Second, 10*time.Millisecond)
	logData, err := os.ReadFile(accessLogPath)
	require.NoError(t, err)
	assert.NotContains(t, string(logData), "\r")
	assert.NotContains(t, string(logData), "\x1b")
	assert.Equal(t, 1, strings.Count(string(logData), "\n"))
}
