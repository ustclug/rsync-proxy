//go:build linux

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ustclug/rsync-proxy/pkg/server"
	"github.com/ustclug/rsync-proxy/pkg/state"
)

type workerProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error  // written before done closes
	id   string // protected by supervisor.mu
}

type supervisor struct {
	mu          sync.Mutex
	s           *server.Server
	store       *state.Store
	dir, binary string
	workers     map[int]*workerProcess
	exited      chan *workerProcess
	stopping    bool
}

func runDaemon(s *server.Server) error {
	worker := os.Getenv(handoffEnv) != ""
	// Only workers discover upstream modules and open transfer logs. The
	// supervisor needs configuration solely to own the listening sockets.
	if err := s.ReadConfigFromFile(worker); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if worker {
		return runWorker(s)
	}
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
	lock, err := state.Lock(filepath.Join(dir, "startup.lock"))
	if err != nil {
		return fmt.Errorf("instance already running: %w", err)
	}
	defer lock.Close()
	// A previous supervisor may have died before its workers finished exiting.
	// Refuse to start with an empty authority while those workers are still live.
	locks, err := filepath.Glob(filepath.Join(dir, "*.lock"))
	if err != nil {
		return err
	}
	for _, path := range locks {
		if path == filepath.Join(dir, "startup.lock") {
			continue
		}
		live, e := state.IsLocked(path)
		if e != nil {
			return e
		}
		if live {
			return fmt.Errorf("worker still running (%s); wait for it to exit before restarting", path)
		}
	}
	path := filepath.Join(dir, "supervisor.sock")
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	p := &supervisor{s: s, store: state.New(), dir: dir, binary: binary, workers: make(map[int]*workerProcess), exited: make(chan *workerProcess, 64)}
	closeIPC, err := state.Serve(dir, p.store, p.upgrade)
	if err != nil {
		return err
	}
	defer closeIPC()
	defer s.CloseLogs()
	if err = s.Listen(); err != nil {
		return err
	}
	defer s.Close()
	defer p.killAndWait()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	p.mu.Lock()
	err = p.startWorker("", 30*time.Second)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if err = notifySystemd("READY=1\nMAINPID=" + strconv.Itoa(os.Getpid())); err != nil {
		return err
	}
	for {
		select {
		case <-signals:
			p.mu.Lock()
			if p.stopping {
				for _, w := range p.workers {
					_ = w.cmd.Process.Kill()
				}
			} else {
				p.stopping = true
				for _, w := range p.workers {
					_ = w.cmd.Process.Signal(syscall.SIGTERM)
				}
			}
			empty := len(p.workers) == 0
			p.mu.Unlock()
			if empty {
				return nil
			}
		case w := <-p.exited:
			p.mu.Lock()
			delete(p.workers, w.cmd.Process.Pid)
			p.retire(w)
			current, _ := p.store.Current()
			if w.err != nil {
				log.Printf("[ERROR] worker %s exited: %v", w.id, w.err)
			}
			if !p.stopping && current == w.id {
				log.Printf("[INFO] restarting current worker %s", w.id)
				err = p.startWorker(current, 30*time.Second)
			}
			empty := len(p.workers) == 0
			p.mu.Unlock()
			if err != nil {
				return err
			}
			if empty {
				return nil
			}
		}
	}
}

func (p *supervisor) retire(w *workerProcess) {
	// Registration may have succeeded even if the readiness response was lost.
	gens, _ := p.store.Generations()
	for _, g := range gens {
		if (w.id != "" && g.ID == w.id) || (w.id == "" && g.PID == w.cmd.Process.Pid && g.State != state.Exited) {
			w.id = g.ID
			_ = p.store.Finish(g.ID)
			_ = os.Remove(g.Control)
			_ = os.Remove(filepath.Join(p.dir, g.ID+".lock"))
		}
	}
}

func (p *supervisor) killAndWait() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopping = true
	for _, w := range p.workers {
		_ = w.cmd.Process.Kill()
	}
	for _, w := range p.workers {
		<-w.done
		p.retire(w)
	}
}

func (p *supervisor) upgrade(id string, timeout time.Duration) error {
	if timeout <= 0 {
		return errors.New("invalid upgrade timeout")
	}
	if !p.mu.TryLock() {
		return errors.New("supervisor is busy")
	}
	defer p.mu.Unlock()
	if p.stopping {
		return errors.New("supervisor is stopping")
	}
	current, err := p.store.Current()
	if err != nil {
		return err
	}
	if id != current {
		return state.ErrNotCurrent
	}
	return p.startWorker(current, timeout)
}

// startWorker runs with mu held. Listener ownership remains in the supervisor,
// so failure before readiness never removes the old worker's listening sockets.
func (p *supervisor) startWorker(parent string, timeout time.Duration) (result error) {
	files, err := p.s.ListenerFiles()
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
	pf := os.NewFile(uintptr(pair[0]), "supervisor-handoff")
	childFile := os.NewFile(uintptr(pair[1]), "worker-handoff")
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
		if !strings.HasPrefix(v, handoffEnv+"=") && !strings.HasPrefix(v, "NOTIFY_SOCKET=") {
			env = append(env, v)
		}
	}
	env = append(env, handoffEnv+"="+strconv.Itoa(state.Protocol))
	child := &exec.Cmd{Path: p.binary, Args: append([]string{p.binary}, os.Args[1:]...), Env: env, ExtraFiles: append([]*os.File{childFile}, files...), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, SysProcAttr: &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}}
	w := &workerProcess{cmd: child, done: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		// Linux delivers Pdeathsig when the creating thread dies. Keep that
		// thread alive until Wait completes, even if Go retires other threads.
		// This also covers workers stuck in config loading, before IPC starts.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		e := child.Start()
		started <- e
		if e != nil {
			return
		}
		w.err = child.Wait()
		close(w.done)
		p.exited <- w
	}()
	if err = <-started; err != nil {
		return err
	}
	_ = childFile.Close()
	p.workers[child.Process.Pid] = w

	previous, _ := p.store.Queues()
	oldLimits := make([]state.Limit, 0, len(previous))
	for _, q := range previous {
		oldLimits = append(oldLimits, q.Limit)
	}
	success := false
	defer func() {
		if success {
			return
		}
		_ = child.Process.Kill()
		<-w.done
		p.retire(w)
		delete(p.workers, child.Process.Pid)
		current, _ := p.store.Current()
		if parent != "" && current != parent {
			if e := p.store.Activate(parent, current, oldLimits); e != nil {
				log.Printf("[ERROR] restore previous worker: %v", e)
			}
		}
	}()
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	if err = enc.Encode(handoffMessage{Protocol: state.Protocol, Parent: parent, StateDir: p.dir, Listeners: p.s.ConfiguredListeners()}); err != nil {
		return err
	}
	var ready handoffMessage
	if err = dec.Decode(&ready); err != nil {
		return fmt.Errorf("worker did not become ready: %w", err)
	}
	if ready.Stage != "ready" || ready.ID == "" {
		return errors.New("invalid worker readiness acknowledgement")
	}
	w.id = ready.ID
	if err = enc.Encode(handoffMessage{Stage: "commit"}); err != nil {
		return err
	}
	var active handoffMessage
	if err = dec.Decode(&active); err != nil {
		return fmt.Errorf("worker failed to activate: %w", err)
	}
	if active.Stage != "active" || active.ID != ready.ID {
		return errors.New("invalid activation acknowledgement")
	}
	if err = enc.Encode(handoffMessage{Stage: "owned"}); err != nil {
		return err
	}
	var serving handoffMessage
	if err = dec.Decode(&serving); err != nil {
		return fmt.Errorf("worker did not confirm ownership: %w", err)
	}
	if serving.Stage != "serving" || serving.ID != ready.ID {
		return errors.New("invalid serving acknowledgement")
	}
	success = true
	if parent != "" {
		gens, _ := p.store.Generations()
		for _, g := range gens {
			if g.ID == parent && g.State != state.Exited {
				if e := privatePost(g.Control, "/internal/stop?timeout="+ready.DrainTimeout.String()); e != nil {
					// StopAccepting must be confirmed before acknowledging an upgrade. If the
					// old worker is unresponsive, kill and reap it before releasing its grants.
					for _, old := range p.workers {
						if old.id == parent {
							_ = old.cmd.Process.Kill()
							<-old.done
							p.retire(old)
						}
					}
					log.Printf("[ERROR] retiring unresponsive worker: %v", e)
				}
			}
		}
	}
	log.Printf("[INFO] supervisor %d activated worker %s (pid %d)", os.Getpid(), w.id, child.Process.Pid)
	return nil
}

// Only the fixed supervisor sends readiness; no MAINPID handoff is required.
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
	return unix.Sendto(fd, []byte(message), 0, &unix.SockaddrUnix{Name: path})
}
