package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	stdlog "log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ustclug/rsync-proxy/pkg/log"
	"github.com/ustclug/rsync-proxy/pkg/server"
)

func TestMain(m *testing.M) {
	code, err := testMain(m)
	if err != nil {
		stdlog.Fatal(err)
	}
	os.Exit(code)
}

func testMain(m *testing.M) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := setupDataDirs()
	if err != nil {
		return 0, err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return 0, err
	}

	rsyncdConfDir := filepath.Join(cwd, "..", "fixtures", "rsyncd")

	var rsyncds []*exec.Cmd
	for _, cfg := range []struct {
		port int
		name string
	}{
		{
			port: 1234,
			name: "foo.conf",
		},
		{
			port: 1235,
			name: "bar.conf",
		},
	} {
		prog, err := runRsyncd(ctx, cfg.port, filepath.Join(rsyncdConfDir, cfg.name))
		if err != nil {
			return 0, err
		}
		rsyncds = append(rsyncds, prog)
	}

	defer func() {
		cancel()
		_ = os.RemoveAll("/tmp/rsync-proxy-e2e/")
		for _, prog := range rsyncds {
			_ = prog.Wait()
		}
	}()

	return m.Run(), nil
}

func getProxyConfigPath(name string) string {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	fp := filepath.Join(cwd, "..", "fixtures", "proxy", name)
	if _, err := os.Stat(fp); err != nil && os.IsNotExist(err) {
		panic(err)
	}
	return fp
}

func startProxy(t *testing.T, overrides ...func(*server.Server)) *server.Server {
	var buf bytes.Buffer
	log.SetOutput(&buf, &buf)

	s := server.New()
	s.ConfigPath = getProxyConfigPath("config1.toml")
	timeout := time.Minute
	s.ReadTimeout, s.WriteTimeout = timeout, timeout

	for _, override := range overrides {
		override(s)
	}

	err := s.ReadConfigFromFile()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}
	s.ListenAddr = "127.0.0.1:0"
	s.HTTPListenAddr = "127.0.0.1:0"

	err = s.Listen()
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}

	go func() {
		err := s.Run()
		if err != nil {
			t.Errorf("Failed to run: %v", err)
		}
	}()

	_, port, err := net.SplitHostPort(s.ListenAddr)
	if err != nil {
		t.Fatalf("Failed to get port: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	err = ensureTCPPortIsReady(ctx, port)
	if err != nil {
		t.Fatalf("Failed to wait for TCP port to be ready: %v", err)
	}

	t.Cleanup(func() {
		s.Close()
		if t.Failed() {
			t.Log("rsync-proxy output:")
			t.Log(buf.String())
		}
	})
	return s
}

func newRsyncCommand(args ...string) *exec.Cmd {
	return exec.Command("rsync", args...)
}

func getRsyncPath(s *server.Server, path string) string {
	return fmt.Sprintf("rsync://%s%s", s.ListenAddr, path)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func ensureTCPPortIsReady(ctx context.Context, port string) error {
	d := net.Dialer{
		Timeout: time.Second * 1,
	}
	addr := net.JoinHostPort("127.0.0.1", port)
	count := time.Duration(1)
	for {
		c, err := d.DialContext(ctx, "tcp4", addr)
		if err == nil {
			_ = c.Close()
			return nil
		}
		if err == context.DeadlineExceeded || count >= 10 {
			return fmt.Errorf("cannot connect to %s", addr)
		}
		time.Sleep(time.Second * count)
		count *= 2
	}
}

func runRsyncd(ctx context.Context, port int, configPath string) (*exec.Cmd, error) {
	p := strconv.Itoa(port)
	prog := exec.CommandContext(ctx, "rsync", "-v", "--daemon", "--no-detach", "--port", p, "--config", configPath)
	prog.Stdout = os.Stdout
	prog.Stderr = os.Stderr
	err := prog.Start()
	if err != nil {
		return nil, err
	}
	err = ensureTCPPortIsReady(ctx, p)
	if err != nil {
		return nil, err
	}
	return prog, nil
}

func writeFile(fp string, data []byte) error {
	err := os.MkdirAll(filepath.Dir(fp), os.ModePerm)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func setupDataDirs() error {
	files := map[string][]byte{
		"/tmp/rsync-proxy-e2e/foo/v3.0/data1": []byte("3.0.1"),
		"/tmp/rsync-proxy-e2e/foo/v3.0/data2": []byte("3.0.2"),
		"/tmp/rsync-proxy-e2e/foo/v3.1/data":  []byte("3.1"),
		"/tmp/rsync-proxy-e2e/bar/v3.2/data":  []byte("3.2"),
		"/tmp/rsync-proxy-e2e/bar/v3.3/data":  []byte("3.3"),
		"/tmp/rsync-proxy-e2e/baz/v3.4/data":  []byte("3.4"),
	}
	for fp, data := range files {
		err := writeFile(fp, data)
		if err != nil {
			return err
		}
	}
	return nil
}
