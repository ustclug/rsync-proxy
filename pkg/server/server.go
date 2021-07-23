package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ustclug/rsync-proxy/pkg/log"
)

const (
	TCPBufferSize = 256
)

var (
	RsyncdVersionPrefix = []byte("@RSYNCD:")
	RsyncdExit          = []byte("@RSYNCD: EXIT\n")
)

type Server struct {
	// --- Options section
	// Listen Address
	ListenAddr    string
	WebListenAddr string
	ConfigPath    string
	// name -> upstream
	Upstreams    map[string]*Upstream
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// ---

	reloadLock sync.RWMutex
	dialer     net.Dialer
	bufPool    sync.Pool
	// name -> address
	modules map[string]string
}

func New() *Server {
	return &Server{
		bufPool: sync.Pool{
			New: func() interface{} {
				return make([]byte, TCPBufferSize)
			},
		},
		dialer: net.Dialer{}, // customize keep alive interval?
	}
}

func (s *Server) complete() error {
	if len(s.Upstreams) == 0 {
		return fmt.Errorf("no upstream found")
	}

	modules := map[string]string{}
	for upstreamName, v := range s.Upstreams {
		addr := net.JoinHostPort(v.Host, strconv.Itoa(v.Port))
		_, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			return fmt.Errorf("resolve address: %w, upstream=%s, address=%s", err, upstreamName, addr)
		}
		for _, moduleName := range v.Modules {
			if _, ok := modules[moduleName]; ok {
				return fmt.Errorf("duplicated module name: %s, upstream=%s", moduleName, upstreamName)
			}
			modules[moduleName] = addr
		}
	}

	s.reloadLock.Lock()
	s.modules = modules
	s.reloadLock.Unlock()

	// .Upstreams is no longer used, reclaims the memory
	s.Upstreams = nil

	return nil
}

func (s *Server) listAllModules(downConn net.Conn) error {
	var buf bytes.Buffer
	modules := make([]string, 0, len(s.modules))

	s.reloadLock.RLock()
	for name := range s.modules {
		modules = append(modules, name)
	}
	timeout := s.WriteTimeout
	s.reloadLock.RUnlock()

	sort.Strings(modules)
	for _, name := range modules {
		buf.WriteString(name)
		buf.WriteRune('\n')
	}
	buf.Write(RsyncdExit)
	_, _ = writeWithTimeout(downConn, buf.Bytes(), timeout)
	return nil
}

func (s *Server) relay(ctx context.Context, downConn *net.TCPConn) error {
	defer downConn.Close()

	buf := s.bufPool.Get().([]byte)
	// nolint:staticcheck
	defer s.bufPool.Put(buf)

	writeTimeout := s.WriteTimeout
	readTimeout := s.ReadTimeout

	n, err := readLine(downConn, buf, readTimeout)
	if err != nil {
		return fmt.Errorf("read version from client: %w", err)
	}
	var RsyncdVersion = make([]byte, n)
	copy(RsyncdVersion, buf[:n])
	if !bytes.HasPrefix(RsyncdVersion, RsyncdVersionPrefix) {
		return fmt.Errorf("unknown version from client: %s", RsyncdVersion)
	}

	_, err = writeWithTimeout(downConn, RsyncdVersion, writeTimeout)
	if err != nil {
		return fmt.Errorf("send version to client: %w", err)
	}

	n, err = readLine(downConn, buf, readTimeout)
	if err != nil {
		return fmt.Errorf("read module from client: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("empty request from client")
	}
	data := buf[:n]
	if len(data) == 1 { // single '\n'
		return s.listAllModules(downConn)
	}

	moduleName := string(buf[:n-1]) // trim trailing \n

	s.reloadLock.RLock()
	upstreamAddr, ok := s.modules[moduleName]
	s.reloadLock.RUnlock()

	if !ok {
		_, _ = writeWithTimeout(downConn, []byte(fmt.Sprintf("unknown module: %s\n", moduleName)), writeTimeout)
		_, _ = writeWithTimeout(downConn, RsyncdExit, writeTimeout)
		return nil
	}

	conn, err := s.dialer.DialContext(ctx, "tcp", upstreamAddr)
	if err != nil {
		return fmt.Errorf("dial to upstream: %s: %w", upstreamAddr, err)
	}
	upConn := conn.(*net.TCPConn)
	defer upConn.Close()

	_, err = writeWithTimeout(upConn, RsyncdVersion, writeTimeout)
	if err != nil {
		return fmt.Errorf("send version to upstream: %w", err)
	}

	n, err = readLine(upConn, buf, readTimeout)
	if err != nil {
		return fmt.Errorf("read version from upstream: %w", err)
	}
	data = buf[:n]
	if !bytes.HasPrefix(data, RsyncdVersionPrefix) {
		return fmt.Errorf("unknown version from upstream: %s", data)
	}

	_, err = writeWithTimeout(upConn, []byte(moduleName+"\n"), writeTimeout)
	if err != nil {
		return fmt.Errorf("send module to upstream: %w", err)
	}

	upClosed := make(chan struct{})
	downClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(upConn, downConn)
		close(downClosed)
	}()
	go func() {
		_, _ = io.Copy(downConn, upConn)
		close(upClosed)
	}()
	var waitFor chan struct{}
	select {
	case <-downClosed:
		_ = upConn.SetLinger(0)
		_ = upConn.CloseRead()
		waitFor = upClosed
	case <-upClosed:
		_ = downConn.CloseRead()
		waitFor = downClosed
	}
	<-waitFor
	return nil
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	downConn := conn.(*net.TCPConn)
	err := s.relay(ctx, downConn)
	if err != nil {
		log.V(2).Errorf("[WARN] handleConn: %s", err)
	}
}

func (s *Server) runHTTPServer() error {
	type Response struct {
		Message string `json:"message"`
	}

	var mux http.ServeMux
	mux.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var resp Response
		enc := json.NewEncoder(w)

		err := s.LoadConfigFromFile()
		if err != nil {
			log.Errorf("[ERROR] Load config: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			resp.Message = "Failed to reload config"
		} else {
			w.WriteHeader(http.StatusOK)
			resp.Message = "Successfully reloaded"
		}
		_ = enc.Encode(&resp)
	})
	log.V(3).Infof("[INFO] HTTP server listening on %s", s.WebListenAddr)
	err := http.ListenAndServe(s.WebListenAddr, &mux)
	if err != nil {
		return err
	}
	return nil
}

func (s *Server) Run(ctx context.Context) error {
	go func() {
		err := s.runHTTPServer()
		if err != nil {
			log.Fatalln(err)
		}
	}()

	log.V(3).Infof("[INFO] Rsync proxy listening on %s", s.ListenAddr)
	listener, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		default:
		}

		conn, err := listener.Accept()
		if err != nil {
			log.V(2).Errorf("[WARN] Accept connection: %s", err)
			continue
		}
		go s.handleConn(ctx, conn)
	}
}
