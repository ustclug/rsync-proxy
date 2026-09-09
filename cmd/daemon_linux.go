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
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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
	id, dir         string
	mu              sync.Mutex // serializes reload and upgrade through the commit point
	private         *http.Server
	privateListener net.Listener
	lock            *os.File
	runResult       chan error
	done            chan struct{}
	backgroundDone  chan struct{}
	drainOnce       sync.Once
	drainMu         sync.Mutex
	drainTimer      *time.Timer
	stopAdmission   func()
}

func runWorker(s *server.Server) error {
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
	store, err := state.Open(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	idBytes := make([]byte, 12)
	if _, err = rand.Read(idBytes); err != nil {
		return err
	}
	d := &daemon{s: s, store: store, dir: dir, id: hex.EncodeToString(idBytes), runResult: make(chan error, 1), done: make(chan struct{}), backgroundDone: make(chan struct{})}
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
		return errors.New("worker requires supervisor handoff")
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
			timeout, err := time.ParseDuration(r.URL.Query().Get("timeout"))
			if err != nil {
				timeout = 0
			}
			d.beginDrain(timeout)
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
	if err = json.NewEncoder(parent).Encode(handoffMessage{Stage: "active", ID: d.id}); err != nil {
		return err
	}
	var ack handoffMessage
	if err = decoder.Decode(&ack); err != nil {
		return err
	}
	if ack.Stage != "owned" {
		return errors.New("missing supervisor acknowledgement")
	}

	d.ready.Store(true)
	if err = json.NewEncoder(parent).Encode(handoffMessage{Stage: "serving", ID: d.id}); err != nil {
		return err
	}
	go d.background()
	defer func() { close(d.done); <-d.backgroundDone }()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	var runErr error
	select {
	case runErr = <-d.runResult:
	case <-store.Done():
		s.Close()
		<-d.runResult
		runErr = errors.New("supervisor connection lost")
	case <-signals:
		d.beginDrain(0)
		select {
		case runErr = <-d.runResult:
		case <-store.Done():
			s.Close()
			<-d.runResult
			runErr = errors.New("supervisor connection lost while draining")
		case <-signals:
			s.AbortConnections()
			runErr = <-d.runResult
		}
	}
	runFinished = true
	d.drainMu.Lock()
	if d.drainTimer != nil {
		d.drainTimer.Stop()
	}
	d.drainMu.Unlock()
	// Finish HTTP handlers before closing their supervisor/log dependencies.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.HTTPServer != nil {
		_ = s.HTTPServer.Shutdown(ctx)
	}
	if err = s.PublishSnapshot(); err != nil {
		log.Printf("[ERROR] final snapshot: %v", err)
	}

	return runErr
}

func (d *daemon) beginDrain(timeout time.Duration) {
	d.drainOnce.Do(func() {
		d.drainMu.Lock()
		defer d.drainMu.Unlock()
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
		}
	}
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
		if err := d.store.Upgrade(d.id, timeout); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			_, _ = fmt.Fprintln(w, `{"message":"Upgrade completed"}`)
		}
	}
	return true
}
