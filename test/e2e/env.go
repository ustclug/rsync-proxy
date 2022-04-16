package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ustclug/rsync-proxy/cmd"
)

var (
	config1 = []byte(`
[upstreams.u1]
host = "127.0.0.1"
port = 1234
modules = ["foo"]

[upstreams.u2]
host = "127.0.0.1"
port = 1235
modules = ["bar"]
`)

	config2 = []byte(`
[upstreams.u1]
host = "127.0.0.1"
port = 1234
modules = ["foo"]

[upstreams.u2]
host = "127.0.0.1"
port = 1235
modules = ["bar", "baz"]
`)

	config3 = []byte(`
[upstreams.u1]
host = "127.0.0.1"
port = 1234
modules = ["foo"]

[upstreams.u2]
host = "127.0.0.1"
port = 1235
modules = ["bar", "foo"]
`)
)

type Env struct {
	cancel     context.CancelFunc
	rsyncds    []*exec.Cmd
	configFile string
	proxyOut   bytes.Buffer
	proxyErr   bytes.Buffer
}

func (e *Env) Setup() error {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel

	cwd, _ := os.Getwd()
	err := setupDataDirs()
	if err != nil {
		return err
	}

	f, err := ioutil.TempFile("", "rsync-proxy-e2e-*")
	if err != nil {
		return err
	}
	_, err = f.Write(config1)
	if err != nil {
		return err
	}
	_ = f.Close()
	e.configFile = f.Name()

	fixturesDir := filepath.Join(cwd, "../fixtures")
	r1, err := runRsyncd(ctx, 1234, filepath.Join(fixturesDir, "foo.conf"))
	if err != nil {
		return err
	}

	r2, err := runRsyncd(ctx, 1235, filepath.Join(fixturesDir, "bar.conf"))
	if err != nil {
		return err
	}
	e.rsyncds = []*exec.Cmd{r1, r2}

	proxyProg := cmd.New()
	proxyProg.SetOut(&e.proxyOut)
	proxyProg.SetErr(&e.proxyErr)
	proxyProg.SetArgs([]string{"--config", f.Name()})
	go func() {
		_ = proxyProg.ExecuteContext(ctx)
	}()

	err = ensureTCPPortIsReady(ctx, "9527")
	if err != nil {
		return err
	}

	return nil
}

func (e *Env) UpdateRsyncProxyConfig(data []byte) error {
	return ioutil.WriteFile(e.configFile, data, 0644)
}

func (e *Env) GetRsyncProxyOutput() {
	fmt.Printf("Stdout: %s\n", e.proxyOut.String())
	fmt.Printf("Stderr: %s\n", e.proxyErr.String())
}

func (e *Env) Teardown() {
	e.cancel()
	_ = os.RemoveAll("/tmp/rsync-proxy-e2e/")
	_ = os.Remove(e.configFile)
	for _, prog := range e.rsyncds {
		_ = prog.Wait()
	}
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
	prog := exec.CommandContext(ctx, "rsync", "--daemon", "--no-detach", "--port", p, "--config", configPath)
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
