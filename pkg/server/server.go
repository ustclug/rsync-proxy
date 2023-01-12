package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	RsyncdServerVersion = []byte("@RSYNCD: 31.0\n")
	RsyncdExit          = []byte("@RSYNCD: EXIT\n")
)

const lineFeed = '\n'

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
	// motd
	Motd string
	// --- End of options section

	reloadLock sync.RWMutex
	dialer     net.Dialer
	bufPool    sync.Pool
	// name -> address
	modules map[string]string

	TCPListener  net.Listener
	HTTPListener net.Listener
}

func New() *Server {
	return &Server{
		bufPool: sync.Pool{
			New: func() interface{} {
				buf := make([]byte, TCPBufferSize)
				return &buf
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
		buf.WriteRune(lineFeed)
	}
	buf.Write(RsyncdExit)
	_, _ = writeWithTimeout(downConn, buf.Bytes(), timeout)
	return nil
}

func (s *Server) relay(ctx context.Context, downConn *net.TCPConn) error {
	defer downConn.Close()

	bufPtr := s.bufPool.Get().(*[]byte)
	defer s.bufPool.Put(bufPtr)
	buf := *bufPtr

	ip := downConn.RemoteAddr()

	writeTimeout := s.WriteTimeout
	readTimeout := s.ReadTimeout

	n, err := readLine(downConn, buf, readTimeout)
	if err != nil {
		return fmt.Errorf("read version from client: %w", err)
	}
	rsyncdClientVersion := make([]byte, n)
	copy(rsyncdClientVersion, buf[:n])
	if !bytes.HasPrefix(rsyncdClientVersion, RsyncdVersionPrefix) {
		return fmt.Errorf("unknown version from client: %s", rsyncdClientVersion)
	}

	_, err = writeWithTimeout(downConn, RsyncdServerVersion, writeTimeout)
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
	if s.Motd != "" {
		_, err = writeWithTimeout(downConn, []byte(s.Motd+"\n"), writeTimeout)
		if err != nil {
			return fmt.Errorf("send motd to downstream: %w", err)
		}
	}
	if len(data) == 1 { // single '\n'
		log.V(3).Infof("client %s requests listing all modules", ip)
		return s.listAllModules(downConn)
	}

	moduleName := string(buf[:n-1]) // trim trailing \n

	s.reloadLock.RLock()
	upstreamAddr, ok := s.modules[moduleName]
	s.reloadLock.RUnlock()

	if !ok {
		_, _ = writeWithTimeout(downConn, []byte(fmt.Sprintf("unknown module: %s\n", moduleName)), writeTimeout)
		_, _ = writeWithTimeout(downConn, RsyncdExit, writeTimeout)
		log.V(3).Infof("client %s requests an non-existing module %s", ip, moduleName)
		return nil
	}

	conn, err := s.dialer.DialContext(ctx, "tcp", upstreamAddr)
	if err != nil {
		return fmt.Errorf("dial to upstream: %s: %w", upstreamAddr, err)
	}
	upConn := conn.(*net.TCPConn)
	defer upConn.Close()

	_, err = writeWithTimeout(upConn, rsyncdClientVersion, writeTimeout)
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

	// send back the motd
	idx := bytes.IndexByte(data, lineFeed)
	if idx+1 < n {
		_, err = writeWithTimeout(downConn, data[idx+1:], writeTimeout)
		if err != nil {
			return fmt.Errorf("send motd to client: %w", err)
		}
	}

	_, err = writeWithTimeout(upConn, []byte(moduleName+"\n"), writeTimeout)
	if err != nil {
		return fmt.Errorf("send module to upstream: %w", err)
	}

	log.V(3).Infof("client %s starts requesting module %s", ip, moduleName)

	// reset read and write deadline for upConn and downConn
	zeroTime := time.Time{}
	_ = upConn.SetDeadline(zeroTime)
	_ = downConn.SetDeadline(zeroTime)

	// <dir>Bytes means bytes *read* from <dir>stream connection
	upBytesC := make(chan int64)
	downBytesC := make(chan int64)
	go func() {
		n, gerr := io.Copy(upConn, downConn)
		if gerr != nil {
			log.V(3).Errorf("copy from downstream to upstream: %v", gerr)
		}
		downBytesC <- n
		close(downBytesC)
	}()
	go func() {
		n, gerr := io.Copy(downConn, upConn)
		if gerr != nil {
			log.V(3).Errorf("copy from upstream to downstream: %v", gerr)
		}
		upBytesC <- n
		close(upBytesC)
	}()
	var upBytes, downBytes int64
	select {
	case downBytes = <-downBytesC:
		_ = upConn.SetLinger(0)
		_ = upConn.CloseRead()
		upBytes = <-upBytesC
	case upBytes = <-upBytesC:
		_ = downConn.CloseRead()
		downBytes = <-downBytesC
	}
	log.V(3).Infof("client %s finishes module %s (TX: %d, RX: %d)", ip, moduleName, upBytes, downBytes)

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

	return http.Serve(s.HTTPListener, &mux)
}

func (s *Server) Listen() error {
	l1, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return fmt.Errorf("create tcp listener: %w", err)
	}
	s.ListenAddr = l1.Addr().String()
	log.V(3).Infof("[INFO] Rsync proxy listening on %s", s.ListenAddr)

	l2, err := net.Listen("tcp", s.WebListenAddr)
	if err != nil {
		return fmt.Errorf("create http listener: %w", err)
	}
	s.WebListenAddr = l2.Addr().String()
	log.V(3).Infof("[INFO] HTTP server listening on %s", s.WebListenAddr)

	s.TCPListener = l1
	s.HTTPListener = l2
	return nil
}

func (s *Server) Close() {
	_ = s.TCPListener.Close()
	_ = s.HTTPListener.Close()
}

func (s *Server) Run() error {
	errCh := make(chan error)
	go func() {
		err := s.runHTTPServer()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			errCh <- fmt.Errorf("run http server: %w", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for {
		select {
		case err := <-errCh:
			return err
		default:
		}

		conn, err := s.TCPListener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept rsync connection: %w", err)
		}
		go s.handleConn(ctx, conn)
	}
}
