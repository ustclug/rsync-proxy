package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ustclug/rsync-proxy/pkg/log"
)

const (
	TCPBufferSize     = 256
	DefaultModuleName = "*"
)

var (
	RsyncdVersionPrefix = []byte("@RSYNCD:")
	RsyncdVersion       = []byte("@RSYNCD: 31.0\n")
	RsyncdExit          = []byte("@RSYNCD: EXIT\n")
)

type Server struct {
	// --- Options section
	// Listen Address
	ListenAddr    string
	WebListenAddr string
	ConfigPath    string
	// name -> upstream
	Upstreams           map[string]*Upstream
	DefaultUpstreamName string
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
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
		if len(s.DefaultUpstreamName) == 0 {
			s.DefaultUpstreamName = upstreamName
		}
	}

	defaultUpstream, ok := s.Upstreams[s.DefaultUpstreamName]
	if !ok {
		return fmt.Errorf("default upstream not found, upstream=%s", s.DefaultUpstreamName)
	}

	log.V(3).Infof("[INFO] default upstream: %s", s.DefaultUpstreamName)

	s.reloadLock.Lock()
	s.modules = modules
	s.modules[DefaultModuleName] = net.JoinHostPort(defaultUpstream.Host, strconv.Itoa(defaultUpstream.Port))
	s.reloadLock.Unlock()

	// .Upstreams is no longer used, reclaims the memory
	s.Upstreams = nil

	return nil
}

func (s *Server) listAllModules(downConn net.Conn) error {
	var buf bytes.Buffer
	for name := range s.modules {
		if name == DefaultModuleName {
			continue
		}
		buf.WriteString(name + "\n")
	}
	buf.Write(RsyncdExit)
	_, _ = s.writeWithTimeout(downConn, buf.Bytes())
	return nil
}

func (s *Server) relay(ctx context.Context, downConn *net.TCPConn) error {
	defer downConn.Close()

	buf := s.bufPool.Get().([]byte)
	defer s.bufPool.Put(buf)

	n, err := s.readWithTimeout(downConn, buf)
	if err != nil {
		return fmt.Errorf("read version from client: %w", err)
	}
	data := buf[:n]
	if !bytes.HasPrefix(data, RsyncdVersionPrefix) {
		return fmt.Errorf("unknown version from client: %s", data)
	}

	_, err = s.writeWithTimeout(downConn, RsyncdVersion)
	if err != nil {
		return fmt.Errorf("send version to client: %w", err)
	}

	n, err = s.readWithTimeout(downConn, buf)
	if err != nil {
		return fmt.Errorf("read module from client: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("empty request from client")
	}
	data = buf[:n]
	if len(data) == 1 { // single '\n'
		return s.listAllModules(downConn)
	}

	s.reloadLock.RLock()
	moduleName := string(buf[:n-1]) // trim trailing \n
	upstreamAddr, ok := s.modules[moduleName]
	if !ok {
		log.V(4).Infof("[DEBUG] unknown module: %s, fallback to default upstream", moduleName)
		upstreamAddr = s.modules[DefaultModuleName]
	}
	s.reloadLock.RUnlock()

	conn, err := s.dialer.DialContext(ctx, "tcp", upstreamAddr)
	if err != nil {
		return fmt.Errorf("dial to upstream: %s: %w", upstreamAddr, err)
	}
	upConn := conn.(*net.TCPConn)
	defer upConn.Close()

	_, err = s.writeWithTimeout(upConn, RsyncdVersion)
	if err != nil {
		return fmt.Errorf("send version to upstream: %w", err)
	}

	n, err = s.readWithTimeout(upConn, buf)
	if err != nil {
		return fmt.Errorf("read version from upstream: %w", err)
	}
	data = buf[:n]
	if !bytes.HasPrefix(data, RsyncdVersionPrefix) {
		return fmt.Errorf("unknown version from upstream: %s", data)
	}

	_, err = s.writeWithTimeout(upConn, []byte(moduleName+"\n"))
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
		log.V(2).Infof("[WARN] handleConn: %s", err)
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
			log.V(1).Infof("[ERROR] Load config: %s", err)
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
			continue
		}
		go s.handleConn(ctx, conn)
	}
	return nil
}
