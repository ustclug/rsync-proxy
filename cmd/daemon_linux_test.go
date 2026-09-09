//go:build linux

package cmd

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ustclug/rsync-proxy/pkg/state"
)

// Re-exec the test executable so -race also instruments every generation. A
// private copy is atomically replaced during tests, just like an installed bin.
func TestDaemonProcess(t *testing.T) {
	config := os.Getenv("RSYNC_DAEMON_TEST_CONFIG")
	if config == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	version, err := os.ReadFile(exe + ".version")
	if err != nil {
		panic(err)
	}
	Version = string(version)
	c := New()
	c.SetArgs([]string{"--config", config})
	if err := c.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type daemonStatus struct {
	Count       int `json:"count"`
	Connections []struct {
		Generation string `json:"generation"`
		Index      uint32 `json:"index"`
	} `json:"connections"`
	Generations []state.Generation `json:"generations"`
}

type daemonFixture struct {
	t                                                                   *testing.T
	dir, binary, config, socket, listen, tlsListen, upstream, cert, key string
	log                                                                 *os.File
	pid                                                                 int
	max, perIP                                                          int
	clients                                                             []net.Conn
	listener                                                            net.Listener
}

func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func newDaemonFixture(t *testing.T, max, perIP int) *daemonFixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "rp-upgrade-")
	require.NoError(t, err)
	f := &daemonFixture{t: t, dir: dir, max: max, perIP: perIP, binary: filepath.Join(dir, "proxy"), config: filepath.Join(dir, "config.toml"), socket: filepath.Join(dir, "http.sock"), listen: freeAddress(t), tlsListen: freeAddress(t)}
	f.cert, f.key = testCertificate(t, dir)
	f.listener, err = net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f.upstream = f.listener.Addr().String()
	go func() {
		for {
			c, err := f.listener.Accept()
			if err != nil {
				return
			}
			go echoRsync(c)
		}
	}()
	f.install("A")
	f.writeConfig(0)
	f.log, err = os.Create(filepath.Join(dir, "process.log"))
	require.NoError(t, err)
	c := exec.Command(f.binary, "-test.run=^TestDaemonProcess$")
	c.Env = append(os.Environ(), "RSYNC_DAEMON_TEST_CONFIG="+f.config, "NOTIFY_SOCKET=")
	c.Stdout = f.log
	c.Stderr = f.log
	require.NoError(t, c.Start())
	f.pid = c.Process.Pid
	wait := make(chan error, 1)
	go func() { wait <- c.Wait() }()
	t.Cleanup(func() {
		for _, conn := range f.clients {
			_ = conn.Close()
		}
		_ = f.listener.Close()
		store, err := state.Open(dir)
		if err == nil {
			gens, _ := store.Generations()
			for _, g := range gens {
				if g.State != "exited" {
					_ = syscall.Kill(g.PID, syscall.SIGKILL)
				}
			}
			_ = store.Close()
		}
		_ = c.Process.Kill()
		select {
		case <-wait:
		case <-time.After(3 * time.Second):
		}
		_ = f.log.Close()
		b, _ := os.ReadFile(filepath.Join(dir, "process.log"))
		if bytes.Contains(b, []byte("DATA RACE")) {
			t.Errorf("race in daemon subprocess:\n%s", b)
		}
		if t.Failed() {
			t.Logf("daemon log:\n%s", b)
		}
		_ = os.RemoveAll(dir)
	})
	require.Eventually(t, func() bool { st, err := f.status(); return err == nil && len(st.Generations) == 1 }, 10*time.Second, 20*time.Millisecond)
	return f
}

func echoRsync(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	if _, err := r.ReadString('\n'); err != nil {
		return
	}
	if _, err := io.WriteString(c, "@RSYNCD: 32.0 sha512 sha256 sha1 md5 md4\n"); err != nil {
		return
	}
	if _, err := r.ReadString('\n'); err != nil {
		return
	}
	_, _ = io.WriteString(c, "READY\n")
	_, _ = io.Copy(c, r)
}

func testCertificate(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600))
	return certPath, keyPath
}

func (f *daemonFixture) install(version string) {
	f.t.Helper()
	exe, err := os.Executable()
	require.NoError(f.t, err)
	in, err := os.Open(exe)
	require.NoError(f.t, err)
	defer in.Close()
	out, err := os.OpenFile(f.binary+".new", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0700)
	require.NoError(f.t, err)
	_, err = io.Copy(out, in)
	require.NoError(f.t, err)
	require.NoError(f.t, out.Close())
	require.NoError(f.t, os.Rename(f.binary+".new", f.binary))
	require.NoError(f.t, os.WriteFile(f.binary+".version", []byte(version), 0600))
}

func (f *daemonFixture) writeConfig(timeout int) {
	f.t.Helper()
	config := fmt.Sprintf(`[proxy]
listen = %q
listen_tls = %q
listen_http = %q
state_dir = %q
tls_cert_file = %q
tls_key_file = %q
access_log = %q
upgrade_drain_timeout = %d
[upstreams.u]
address = %q
modules = ["foo"]
max_active_connections = %d
max_queued_connections = 100
per_ip_max_active_connections = %d
`, f.listen, f.tlsListen, f.socket, f.dir, f.cert, f.key, filepath.Join(f.dir, "access.log"), timeout, f.upstream, f.max, f.perIP)
	require.NoError(f.t, os.WriteFile(f.config, []byte(config), 0600))
}

func (f *daemonFixture) status() (daemonStatus, error) {
	var st daemonStatus
	resp, err := makeHttpClient(f.socket).Get("http://./status")
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("status: %s", resp.Status)
	}
	err = json.NewDecoder(resp.Body).Decode(&st)
	return st, err
}

func (f *daemonFixture) upgrade() {
	f.t.Helper()
	var out bytes.Buffer
	require.NoError(f.t, sendControl(f.socket, "/upgrade?timeout=10s", 10*time.Second, &out), out.String())
}

type relayReader struct {
	*bufio.Reader
	ready bool
}

func (f *daemonFixture) connect(useTLS bool) (net.Conn, *relayReader) {
	f.t.Helper()
	var c net.Conn
	var err error
	if useTLS {
		c, err = tls.Dial("tcp", f.tlsListen, &tls.Config{InsecureSkipVerify: true})
	} else {
		c, err = net.Dial("tcp", f.listen)
	}
	require.NoError(f.t, err)
	f.clients = append(f.clients, c)
	require.NoError(f.t, c.SetDeadline(time.Now().Add(5*time.Second)))
	r := bufio.NewReader(c)
	_, err = io.WriteString(c, "@RSYNCD: 32.0\n")
	require.NoError(f.t, err)
	line, err := r.ReadString('\n')
	require.NoError(f.t, err)
	require.Contains(f.t, line, "@RSYNCD:")
	_, err = io.WriteString(c, "foo\n")
	require.NoError(f.t, err)
	return c, &relayReader{Reader: r}
}

func checkEcho(t *testing.T, c net.Conn, r *relayReader) {
	t.Helper()
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
	if !r.ready {
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "READY\n", line)
		r.ready = true
	}
	_, err := io.WriteString(c, "ping\n")
	require.NoError(t, err)
	line, err := r.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ping\n", line)
}

func TestMultiGenerationUpgrade(t *testing.T) {
	f := newDaemonFixture(t, 3, 10)
	a, ar := f.connect(false)
	checkEcho(t, a, ar)
	// Accept a TLS connection before upgrade, but defer its handshake until B.
	raw, err := net.Dial("tcp", f.tlsListen)
	require.NoError(t, err)
	f.clients = append(f.clients, raw)
	require.Eventually(t, func() bool { st, err := f.status(); return err == nil && st.Count == 2 }, 3*time.Second, 10*time.Millisecond)
	f.install("B")
	f.upgrade()
	b, br := f.connect(true)
	checkEcho(t, b, br)
	f.install("C")
	f.upgrade()
	checkEcho(t, a, ar)
	checkEcho(t, b, br)
	lazy := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	require.NoError(t, lazy.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, lazy.Handshake())
	lr := &relayReader{Reader: bufio.NewReader(lazy)}
	_, err = io.WriteString(lazy, "@RSYNCD: 32.0\n")
	require.NoError(t, err)
	_, err = lr.ReadString('\n')
	require.NoError(t, err)
	_, err = io.WriteString(lazy, "foo\n")
	require.NoError(t, err)
	checkEcho(t, lazy, lr)
	c, cr := f.connect(false)
	line, err := cr.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, line, "queued")
	_, err = cr.ReadString('\n')
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		st, err := f.status()
		if err != nil {
			return false
		}
		n := 0
		for _, g := range st.Generations {
			if g.State != "exited" {
				n++
			}
		}
		return n == 3 && st.Count == 4
	}, 4*time.Second, 20*time.Millisecond)
	// Releasing an A slot must promote C without exceeding the global cap.
	require.NoError(t, a.Close())
	checkEcho(t, c, cr)
	resp, err := makeHttpClient(f.socket).Get("http://./metrics")
	require.NoError(t, err)
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Contains(t, string(data), "generation=\"")
	require.Contains(t, string(data), `version="A"`)
	require.Contains(t, string(data), `version="C"`)
	require.NoError(t, os.Rename(filepath.Join(f.dir, "access.log"), filepath.Join(f.dir, "access.log.old")))
	var out bytes.Buffer
	require.NoError(t, sendControl(f.socket, "/reopen-logs", 5*time.Second, &out))
	require.NoError(t, lazy.Close())
	require.NoError(t, b.Close())
	require.NoError(t, c.Close())
	require.Eventually(t, func() bool {
		st, err := f.status()
		if err != nil {
			return false
		}
		n := 0
		for _, g := range st.Generations {
			if g.State != "exited" {
				n++
			}
		}
		return n == 1 && st.Count == 0
	}, 5*time.Second, 20*time.Millisecond)
	_, err = os.Stat(f.socket)
	require.NoError(t, err)
	logs, err := os.ReadFile(filepath.Join(f.dir, "access.log"))
	require.NoError(t, err)
	require.Contains(t, string(logs), "finishes module foo")
}

func TestUpgradeFailureAndDrainTimeout(t *testing.T) {
	f := newDaemonFixture(t, 1, 2)
	a, ar := f.connect(false)
	checkEcho(t, a, ar)
	queued, qr := f.connect(false)
	line, err := qr.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, line, "queued")
	f.install("B")
	original, err := os.ReadFile(f.config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.config, []byte("invalid toml ["), 0600))
	var out bytes.Buffer
	require.Error(t, sendControl(f.socket, "/upgrade?timeout=2s", 2*time.Second, &out))
	checkEcho(t, a, ar)
	require.NoError(t, os.WriteFile(f.config, original, 0600))
	// A silent executable never acknowledges readiness; rollback must keep A.
	require.NoError(t, os.WriteFile(f.binary+".new", []byte("#!/bin/sh\nexec sleep 10\n"), 0700))
	require.NoError(t, os.Rename(f.binary+".new", f.binary))
	require.Error(t, sendControl(f.socket, "/upgrade?timeout=100ms", time.Second, &out))
	checkEcho(t, a, ar)
	f.install("B")
	f.writeConfig(2)
	f.upgrade()
	// Per-IP cap includes A's active and queued sessions after handoff.
	c, cr := f.connect(false)
	line, err = cr.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, line, "per-IP connection limit")
	_ = c.Close()
	f.install("C")
	f.writeConfig(0)
	f.upgrade()
	// C's unlimited drain setting must not reset A's previously assigned timer.
	require.NoError(t, a.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = ar.ReadString('\n')
	require.Error(t, err)
	require.NoError(t, queued.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _ = io.Copy(io.Discard, qr)
	require.Eventually(t, func() bool {
		st, err := f.status()
		if err != nil {
			return false
		}
		for _, g := range st.Generations {
			if g.Version == "A" {
				return g.State == "exited"
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)
	n, nr := f.connect(false)
	checkEcho(t, n, nr)
}

func TestDeadGenerationReleasesCapacity(t *testing.T) {
	f := newDaemonFixture(t, 1, 0)
	a, ar := f.connect(false)
	checkEcho(t, a, ar)
	f.install("B")
	f.upgrade()
	b, br := f.connect(false)
	line, err := br.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, line, "queued")
	_, err = br.ReadString('\n')
	require.NoError(t, err)
	require.NoError(t, syscall.Kill(f.pid, syscall.SIGKILL))
	checkEcho(t, b, br)
}

func TestConnectionsDuringUpgrade(t *testing.T) {
	f := newDaemonFixture(t, 0, 0)
	var wg sync.WaitGroup
	errorsC := make(chan error, 200)
	wg.Go(func() {
		for range 200 {
			c, err := net.DialTimeout("tcp", f.listen, time.Second)
			if err != nil {
				errorsC <- err
				continue
			}
			_ = c.SetDeadline(time.Now().Add(time.Second))
			r := bufio.NewReader(c)
			_, err = io.WriteString(c, "@RSYNCD: 32.0\n")
			if err == nil {
				var line string
				line, err = r.ReadString('\n')
				if err == nil && !strings.HasPrefix(line, "@RSYNCD:") {
					err = fmt.Errorf("bad greeting %q", line)
				}
			}
			if err == nil {
				_, err = io.WriteString(c, "foo\n")
			}
			if err == nil {
				var line string
				line, err = r.ReadString('\n')
				if err == nil && line != "READY\n" {
					err = fmt.Errorf("bad upstream response %q", line)
				}
			}
			_ = c.Close()
			if err != nil {
				errorsC <- err
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	f.install("B")
	f.upgrade()
	f.install("C")
	f.upgrade()
	wg.Wait()
	close(errorsC)
	for err := range errorsC {
		require.NoError(t, err)
	}
}

func TestUpgradeRejectsIncompatibleConfig(t *testing.T) {
	f := newDaemonFixture(t, 0, 0)
	c, r := f.connect(false)
	checkEcho(t, c, r)
	original, err := os.ReadFile(f.config)
	require.NoError(t, err)
	for _, tc := range []struct{ name, old, replacement string }{
		{"listener", f.listen, freeAddress(t)},
		{"certificate", f.cert, filepath.Join(f.dir, "missing.pem")},
		{"state directory", "state_dir = " + fmt.Sprintf("%q", f.dir), "state_dir = " + fmt.Sprintf("%q", filepath.Join(f.dir, "other"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(f.config, bytes.Replace(original, []byte(tc.old), []byte(tc.replacement), 1), 0600))
			var out bytes.Buffer
			require.Error(t, sendControl(f.socket, "/upgrade?timeout=2s", 2*time.Second, &out))
			checkEcho(t, c, r)
			require.NoError(t, os.WriteFile(f.config, original, 0600))
		})
	}
	f.install("B")
	f.upgrade()
	checkEcho(t, c, r)
}

func TestRealRsyncTransferAcrossUpgrades(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync is not installed")
	}
	f := newDaemonFixture(t, 2, 0)
	source := filepath.Join(f.dir, "source")
	destination := filepath.Join(f.dir, "destination")
	require.NoError(t, os.Mkdir(source, 0755))
	require.NoError(t, os.Mkdir(destination, 0755))
	data := make([]byte, 1024*1024)
	_, err := rand.Read(data)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "payload"), data, 0644))
	address := freeAddress(t)
	_, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	rsyncConfig := filepath.Join(f.dir, "rsyncd.conf")
	require.NoError(t, os.WriteFile(rsyncConfig, []byte(fmt.Sprintf("use chroot = no\n[foo]\npath = %s\nread only = yes\n", source)), 0600))
	daemon := exec.Command("rsync", "--daemon", "--no-detach", "--address=127.0.0.1", "--port="+port, "--config="+rsyncConfig)
	daemon.Stdout = f.log
	daemon.Stderr = f.log
	require.NoError(t, daemon.Start())
	t.Cleanup(func() { _ = daemon.Process.Kill(); _ = daemon.Wait() })
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 3*time.Second, 10*time.Millisecond)
	f.upstream = address
	f.writeConfig(0)
	var reload bytes.Buffer
	require.NoError(t, sendControl(f.socket, "/reload", 5*time.Second, &reload))
	client := exec.Command("rsync", "--bwlimit=256", "rsync://"+f.listen+"/foo/payload", destination+"/")
	var output bytes.Buffer
	client.Stdout = &output
	client.Stderr = &output
	require.NoError(t, client.Start())
	finished := make(chan error, 1)
	go func() { finished <- client.Wait() }()
	t.Cleanup(func() { _ = client.Process.Kill() })
	require.Eventually(t, func() bool {
		select {
		case err := <-finished:
			t.Fatalf("rsync exited before upgrade: %v: %s", err, output.String())
		default:
		}
		st, err := f.status()
		return err == nil && st.Count > 0
	}, 3*time.Second, 10*time.Millisecond)
	f.install("B")
	f.upgrade()
	f.install("C")
	f.upgrade()
	listing, err := exec.Command("rsync", "--list-only", "rsync://"+f.listen+"/foo/").CombinedOutput()
	require.NoError(t, err, string(listing))
	require.Contains(t, string(listing), "payload")
	select {
	case err := <-finished:
		require.NoError(t, err, output.String())
	case <-time.After(15 * time.Second):
		t.Fatal("rsync transfer did not finish")
	}
	actual, err := os.ReadFile(filepath.Join(destination, "payload"))
	require.NoError(t, err)
	require.Equal(t, data, actual)
}
