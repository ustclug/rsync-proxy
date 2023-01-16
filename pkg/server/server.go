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
	"os"
	"sort"
	"sync"
	"sync/atomic"
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

type ConnInfo struct {
	Index       uint64    `json:"index"`
	LocalAddr   string    `json:"local_addr"`
	RemoteAddr  string    `json:"remote_addr"`
	ConnectedAt time.Time `json:"connected_at"`
	Module      string    `json:"module"`
}

type Server struct {
	// --- Options section
	// Listen Address
	ListenAddr     string
	HTTPListenAddr string
	ConfigPath     string

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

	activeConnCount atomic.Int64
	connIndex       atomic.Uint64
	connInfo        sync.Map

	TCPListener  *net.TCPListener
	HTTPListener *net.TCPListener
}

func New() *Server {
	return &Server{
		bufPool: sync.Pool{
			New: func() any {
				buf := make([]byte, TCPBufferSize)
				return &buf
			},
		},
		dialer: net.Dialer{}, // customize keep alive interval?
	}
}

func (s *Server) loadConfig(c *Config) error {
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("no upstream found")
	}

	modules := map[string]string{}
	for upstreamName, v := range c.Upstreams {
		addr := v.Address
		_, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			return fmt.Errorf("resolve address: %w, upstream=%s, address=%s", err, upstreamName, addr)
		}
		for _, moduleName := range v.Modules {
			if _, ok := modules[moduleName]; ok {
				return fmt.Errorf("duplicate module name: %s, upstream=%s", moduleName, upstreamName)
			}
			modules[moduleName] = addr
		}
	}

	s.reloadLock.Lock()
	if s.ListenAddr == "" {
		s.ListenAddr = c.Proxy.Listen
	}
	if s.HTTPListenAddr == "" {
		s.HTTPListenAddr = c.Proxy.ListenHTTP
	}
	s.Motd = c.Proxy.Motd
	s.modules = modules
	s.reloadLock.Unlock()

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

func (s *Server) relay(ctx context.Context, index uint64, downConn *net.TCPConn) error {
	defer downConn.Close()

	info := ConnInfo{
		Index:       index,
		LocalAddr:   downConn.LocalAddr().String(),
		RemoteAddr:  downConn.RemoteAddr().String(),
		ConnectedAt: time.Now().Truncate(time.Second),
	}
	s.connInfo.Store(index, info)
	defer s.connInfo.Delete(index)

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
	info.Module = moduleName
	s.connInfo.Store(index, info)

	s.reloadLock.RLock()
	upstreamAddr, ok := s.modules[moduleName]
	s.reloadLock.RUnlock()

	if !ok {
		_, _ = writeWithTimeout(downConn, []byte(fmt.Sprintf("unknown module: %s\n", moduleName)), writeTimeout)
		_, _ = writeWithTimeout(downConn, RsyncdExit, writeTimeout)
		log.V(3).Infof("client %s requests non-existing module %s", ip, moduleName)
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
	log.V(3).Infof("client %s finishes module %s (sent: %d, received: %d)", ip, moduleName, upBytes, downBytes)

	return nil
}

func (s *Server) GetActiveConnectionCount() int64 {
	return s.activeConnCount.Load()
}

func (s *Server) ListConnectionInfo() (result []ConnInfo) {
	result = make([]ConnInfo, 0, s.GetActiveConnectionCount())
	s.connInfo.Range(func(_, value any) bool {
		result = append(result, value.(ConnInfo))
		return true
	})
	sort.Slice(result, func(i, j int) bool {
		return result[i].Index < result[j].Index
	})
	return
}

func (s *Server) runHTTPServer() error {
	var mux http.ServeMux
	mux.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var resp struct {
			Message string `json:"message"`
		}

		err := s.ReadConfigFromFile()
		if err != nil {
			log.Errorf("[ERROR] Load config: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			resp.Message = "Failed to reload config"
		} else {
			w.WriteHeader(http.StatusOK)
			resp.Message = "Successfully reloaded"
		}
		_ = json.NewEncoder(w).Encode(&resp)
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var status struct {
			Count       int        `json:"count"`
			Connections []ConnInfo `json:"connections"`
		}
		status.Connections = s.ListConnectionInfo()
		status.Count = len(status.Connections)
		_ = json.NewEncoder(w).Encode(&status)
	})

	mux.HandleFunc("/telegraf", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		timestamp := time.Now().Truncate(time.Second).UnixNano()
		count := s.GetActiveConnectionCount()
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "(unknown)"
		}
		// https://docs.influxdata.com/influxdb/latest/reference/syntax/line-protocol/
		_, _ = fmt.Fprintf(w, "rsync-proxy,host=%q count=%d %d\n", hostname, count, timestamp)
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

	l2, err := net.Listen("tcp", s.HTTPListenAddr)
	if err != nil {
		return fmt.Errorf("create http listener: %w", err)
	}
	s.HTTPListenAddr = l2.Addr().String()
	log.V(3).Infof("[INFO] HTTP server listening on %s", s.HTTPListenAddr)

	s.TCPListener = l1.(*net.TCPListener)
	s.HTTPListener = l2.(*net.TCPListener)
	return nil
}

func (s *Server) Close() {
	_ = s.TCPListener.Close()
	_ = s.HTTPListener.Close()
}

func (s *Server) handleConn(ctx context.Context, conn *net.TCPConn) {
	s.activeConnCount.Add(1)
	defer s.activeConnCount.Add(-1)
	connIndex := s.connIndex.Add(1)

	err := s.relay(ctx, connIndex, conn)
	if err != nil {
		log.V(2).Errorf("[WARN] handleConn: %s", err)
	}
}

func (s *Server) Run() error {
	errC := make(chan error)
	go func() {
		err := s.runHTTPServer()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			errC <- fmt.Errorf("run http server: %w", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for {
		select {
		case err := <-errC:
			return err
		default:
		}

		conn, err := s.TCPListener.AcceptTCP()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept rsync connection: %w", err)
		}
		go s.handleConn(ctx, conn)
	}
}
