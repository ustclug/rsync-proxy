//go:build linux

package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ustclug/rsync-proxy/pkg/server"
	"github.com/ustclug/rsync-proxy/pkg/state"
)

const handoffEnv = "RSYNC_PROXY_HANDOFF"

type handoffMessage struct {
	Protocol     int
	Parent       string
	StateDir     string
	Listeners    [3]string
	Stage        string
	ID           string
	DrainTimeout time.Duration
}

type daemon struct {
	ready           atomic.Bool
	s               *server.Server
	store           *state.Store
	id, dir, binary string
	mu              sync.Mutex // serializes reload and upgrade through the commit point
	private         *http.Server
	privateListener net.Listener
	lock            *os.File
	runResult       chan error
	done            chan struct{}
	backgroundDone  chan struct{}
	drainOnce       sync.Once
	drainTimer      *time.Timer
	stopAdmission   func()
}

func runDaemon(s *server.Server) error {
	dir := s.StateDir
	if dir == "" {
		dir = "/run/rsync-proxy"
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	s.StateDir = dir
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	startup, err := state.Lock(filepath.Join(dir, "startup.lock"))
	if err != nil {
		return fmt.Errorf("another startup is in progress: %w", err)
	}
	defer startup.Close()
	store, err := state.Open(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	idBytes := make([]byte, 12)
	if _, err = rand.Read(idBytes); err != nil {
		return err
	}
	d := &daemon{s: s, store: store, dir: dir, id: hex.EncodeToString(idBytes), binary: binary, runResult: make(chan error, 1), done: make(chan struct{}), backgroundDone: make(chan struct{})}
	var parent net.Conn
	var decoder *json.Decoder
	var hello handoffMessage
	if os.Getenv(handoffEnv) != "" {
		if os.Getenv(handoffEnv) != strconv.Itoa(state.Protocol) {
			return errors.New("unsupported handoff protocol")
		}
		f := os.NewFile(3, "handoff")
		parent, err = net.FileConn(f)
		_ = f.Close()
		if err != nil {
			return err
		}
		defer parent.Close()
		decoder = json.NewDecoder(parent)
		if err = decoder.Decode(&hello); err != nil {
			return err
		}
		if hello.Protocol != state.Protocol || hello.StateDir != dir {
			return errors.New("incompatible handoff protocol or state_dir")
		}
	} else {
		gens, err := store.Generations()
		if err != nil {
			return err
		}
		for _, g := range gens {
			live, err := state.IsLocked(filepath.Join(dir, g.ID+".lock"))
			if err != nil {
				return err
			}
			if live {
				return fmt.Errorf("generation %s is still running; use upgrade", g.ID)
			}
		}
		if err = store.Reset(); err != nil {
			return err
		}
		for _, g := range gens {
			_ = os.Remove(filepath.Join(dir, g.ID+".lock"))
			_ = os.Remove(g.Control)
		}
	}
	d.lock, err = state.Lock(filepath.Join(dir, d.id+".lock"))
	if err != nil {
		return err
	}
	defer d.lock.Close()
	control := filepath.Join(dir, d.id+".sock")
	d.privateListener, err = net.Listen("unix", control)
	if err != nil {
		return err
	}
	defer d.privateListener.Close()
	if err = os.Chmod(control, 0600); err != nil {
		return err
	}
	if err = store.Register(state.Generation{ID: d.id, PID: os.Getpid(), Version: Version, Control: control}); err != nil {
		return err
	}
	defer func() {
		if err := store.Finish(d.id); err != nil {
			log.Printf("[ERROR] retire generation: %v", err)
		}
	}()
	s.Generation = d.id
	// Configuration was loaded before attaching the store: a prospective child
	// must not publish new limits until the parent authorizes the commit.
	s.Shared = store
	s.ControlHandler = d.control
	defer s.CloseLogs()
	if parent != nil {
		n := 2
		if hello.Listeners[1] != "" {
			n++
		}
		files := make([]*os.File, n)
		for i := range files {
			files[i] = os.NewFile(uintptr(4+i), "listener")
		}
		err = s.ListenInherited(files, hello.Listeners)
		for _, f := range files {
			_ = f.Close()
		}
	} else {
		err = s.Listen()
	}
	if err != nil {
		return err
	}
	defer s.Close()
	d.private = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/reopen" {
			if err := s.ReopenLogs(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		if r.URL.Path == "/internal/stop" {
			d.beginDrain(0)
			return
		}
		if !d.control(w, r) {
			http.NotFound(w, r)
		}
	})}
	go func() {
		if err := d.private.Serve(d.privateListener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("[ERROR] private control: %v", err)
		}
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.private.Shutdown(ctx)
	}()
	if parent != nil {
		enc := json.NewEncoder(parent)
		if err = enc.Encode(handoffMessage{Stage: "ready", ID: d.id, DrainTimeout: s.DrainTimeout()}); err != nil {
			return err
		}
		var commit handoffMessage
		if err = decoder.Decode(&commit); err != nil {
			return err
		}
		if commit.Stage != "commit" {
			return errors.New("upgrade was not committed")
		}
	}
	if err = store.Activate(d.id, hello.Parent, s.Limits()); err != nil {
		return err
	}
	d.stopAdmission = s.StartAdmission()
	defer d.stopAdmission()
	go func() { d.runResult <- s.Run() }()
	runFinished := false
	defer func() {
		if !runFinished {
			s.Close()
			<-d.runResult
		}
	}()
	<-s.Started
	if err = s.PublishSnapshot(); err != nil {
		return err
	}
	if parent == nil {
		if err = notifySystemd("READY=1\nMAINPID=" + strconv.Itoa(os.Getpid())); err != nil {
			return err
		}
	} else {
		if err = json.NewEncoder(parent).Encode(handoffMessage{Stage: "active", ID: d.id}); err != nil {
			return err
		}
		// Keep the parent alive until it has transferred systemd ownership.
		var ack handoffMessage
		if err = decoder.Decode(&ack); err != nil {
			return err
		}
		if ack.Stage != "owned" {
			return errors.New("missing ownership acknowledgement")
		}
	}
	_ = startup.Close()
	d.ready.Store(true)
	go d.background()
	defer func() { close(d.done); <-d.backgroundDone }()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	var runErr error
	select {
	case runErr = <-d.runResult:
	case <-signals:
		d.stopAll()
		select {
		case runErr = <-d.runResult:
		case <-signals:
			s.AbortConnections()
			runErr = <-d.runResult
		}
	}
	runFinished = true
	if d.drainTimer != nil {
		d.drainTimer.Stop()
	}
	// Finish HTTP handlers before closing their database/log dependencies.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.HTTPServer != nil {
		_ = s.HTTPServer.Shutdown(ctx)
	}
	if err = s.PublishSnapshot(); err != nil {
		log.Printf("[ERROR] final snapshot: %v", err)
	}
	current, _ := store.Current()
	if current == d.id {
		for _, addr := range s.ConfiguredListeners() {
			if strings.HasPrefix(addr, "/") {
				_ = os.Remove(addr)
			}
		}
	}
	return runErr
}

func (d *daemon) beginDrain(timeout time.Duration) {
	d.drainOnce.Do(func() {
		var deadline time.Time
		if timeout > 0 {
			deadline = time.Now().Add(timeout)
			d.drainTimer = time.AfterFunc(timeout, func() { log.Printf("[INFO] generation %s drain timeout reached", d.id); d.s.AbortConnections() })
		}
		if err := d.store.Drain(d.id, deadline); err != nil {
			log.Printf("[ERROR] record drain: %v", err)
		}
		log.Printf("[INFO] draining generation %s (timeout %s)", d.id, timeout)
		d.s.StopAccepting()
	})
}

func (d *daemon) background() {
	defer close(d.backgroundDone)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			if err := d.s.PublishSnapshot(); err != nil {
				log.Printf("[ERROR] publish snapshot: %v", err)
			}
			gens, err := d.store.Generations()
			if err != nil {
				continue
			}
			for _, g := range gens {
				if g.ID == d.id || g.State == state.Exited {
					continue
				}
				live, err := state.IsLocked(filepath.Join(d.dir, g.ID+".lock"))
				if err == nil && !live {
					if err := d.store.Finish(g.ID); err != nil {
						log.Printf("[ERROR] reclaim generation: %v", err)
					}
				}
			}
		}
	}
}

func (d *daemon) stopAll() {
	current, err := d.store.Current()
	if err == nil && current == d.id {
		gens, err := d.store.Generations()
		if err == nil {
			for _, g := range gens {
				if g.ID != d.id && g.State != state.Exited {
					if err := privatePost(g.Control, "/internal/stop"); err != nil {
						log.Printf("[ERROR] stop generation %s: %v", g.ID, err)
					}
				}
			}
		}
	}
	d.beginDrain(0)
}

func privatePost(addr, path string) error {
	client := makeHttpClient(addr)
	client.Timeout = 3 * time.Second
	resp, err := client.Post("http://."+path, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, b)
	}
	return nil
}

func (d *daemon) control(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/upgrade", "/reload", "/reopen-logs":
	default:
		return false
	}
	if !d.ready.Load() {
		http.Error(w, "generation is starting", http.StatusServiceUnavailable)
		return true
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return true
	}
	if !d.mu.TryLock() {
		http.Error(w, "upgrade or reload in progress", http.StatusConflict)
		return true
	}
	defer d.mu.Unlock()
	current, err := d.store.Current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return true
	}
	if current != d.id {
		// A keepalive HTTP connection can still belong to an older generation.
		gens, err := d.store.Generations()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return true
		}
		for _, g := range gens {
			if g.ID == current {
				client := makeHttpClient(g.Control)
				client.Timeout = 0
				req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://."+r.URL.RequestURI(), nil)
				resp, err := client.Do(req)
				if err != nil {
					http.Error(w, err.Error(), http.StatusServiceUnavailable)
					return true
				}
				defer resp.Body.Close()
				w.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(w, resp.Body)
				return true
			}
		}
		http.Error(w, "current generation unavailable", http.StatusServiceUnavailable)
		return true
	}
	switch r.URL.Path {
	case "/reload":
		if err := d.s.ReadConfigFromFile(true); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			_, _ = fmt.Fprintln(w, `{"message":"Successfully reloaded"}`)
		}
	case "/reopen-logs":
		gens, err := d.store.Generations()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return true
		}
		for _, g := range gens {
			if g.State != state.Exited {
				if err := privatePost(g.Control, "/internal/reopen"); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return true
				}
			}
		}
		_, _ = fmt.Fprintln(w, `{"message":"Reopened logs in all generations"}`)
	case "/upgrade":
		timeout := 30 * time.Second
		if raw := r.URL.Query().Get("timeout"); raw != "" {
			timeout, err = time.ParseDuration(raw)
			if err != nil || timeout <= 0 {
				http.Error(w, "invalid timeout", http.StatusBadRequest)
				return true
			}
		}
		if err := d.upgrade(timeout); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			_, _ = fmt.Fprintln(w, `{"message":"Upgrade completed"}`)
		}
	}
	return true
}

func (d *daemon) upgrade(timeout time.Duration) (resultErr error) {
	files, err := d.s.ListenerFiles()
	if err != nil {
		return err
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	pf := os.NewFile(uintptr(pair[0]), "parent")
	childFile := os.NewFile(uintptr(pair[1]), "child")
	defer childFile.Close()
	conn, err := net.FileConn(pf)
	_ = pf.Close()
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	env := make([]string, 0, len(os.Environ())+1)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, handoffEnv+"=") {
			env = append(env, v)
		}
	}
	env = append(env, handoffEnv+"="+strconv.Itoa(state.Protocol))
	child := &exec.Cmd{Path: d.binary, Args: append([]string{d.binary}, os.Args[1:]...), Env: env, ExtraFiles: append([]*os.File{childFile}, files...), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	if err = child.Start(); err != nil {
		return err
	}
	_ = childFile.Close()
	wait := make(chan error, 1)
	go func() { wait <- child.Wait() }()
	success := false
	defer func() {
		if success {
			return
		}
		_ = child.Process.Kill()
		<-wait
		// The old listeners have not been closed. Restore admission policy if
		// the child failed after committing but before acknowledging ownership.
		current, e := d.store.Current()
		if e == nil && current != d.id {
			if e = d.store.Activate(d.id, current, d.s.Limits()); e != nil {
				log.Printf("[ERROR] restore serving generation: %v", e)
			}
		}
		_ = notifySystemd("MAINPID=" + strconv.Itoa(os.Getpid()))
	}()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err = enc.Encode(handoffMessage{Protocol: state.Protocol, Parent: d.id, StateDir: d.dir, Listeners: d.s.ConfiguredListeners()}); err != nil {
		return err
	}
	var ready handoffMessage
	if err = dec.Decode(&ready); err != nil {
		return fmt.Errorf("new generation did not become ready: %w", err)
	}
	if ready.Stage != "ready" {
		return errors.New("invalid readiness acknowledgement")
	}
	if err = enc.Encode(handoffMessage{Stage: "commit"}); err != nil {
		return err
	}
	var active handoffMessage
	if err = dec.Decode(&active); err != nil {
		return fmt.Errorf("new generation failed to activate: %w", err)
	}
	if active.Stage != "active" || active.ID != ready.ID {
		return errors.New("invalid activation acknowledgement")
	}
	if err = notifySystemd("MAINPID=" + strconv.Itoa(child.Process.Pid)); err != nil {
		return fmt.Errorf("transfer systemd ownership: %w", err)
	}
	if err = enc.Encode(handoffMessage{Stage: "owned"}); err != nil {
		return err
	}
	success = true
	d.beginDrain(ready.DrainTimeout)
	log.Printf("[INFO] upgraded %s to %s (pid %d)", d.id, ready.ID, child.Process.Pid)
	return nil
}

// A barrier makes ownership changes visible to PID 1 before the old main
// process is allowed to exit. Only the current main process sends MAINPID.
func notifySystemd(message string) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	addr := &unix.SockaddrUnix{Name: path}
	if err = unix.Sendto(fd, []byte(message), 0, addr); err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	defer w.Close()
	if err = unix.Sendmsg(fd, []byte("BARRIER=1"), unix.UnixRights(int(w.Fd())), addr, 0); err != nil {
		return err
	}
	_ = w.Close()
	if err = r.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	var b [1]byte
	_, err = r.Read(b[:])
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
