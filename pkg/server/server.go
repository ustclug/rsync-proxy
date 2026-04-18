package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ustclug/rsync-proxy/pkg/logging"
	"github.com/ustclug/rsync-proxy/pkg/queue"
)

const (
	TCPBufferSize = 256
)

var (
	RsyncdVersionPrefix = []byte("@RSYNCD:")
	// Daemon auth list is a must in server version since 32.0
	// See https://github.com/RsyncProject/rsync/blob/a6312e60c95e5ebb5764eaf18eb07be23420ebc6/clientserver.c#L203
	RsyncdServerVersion = []byte("@RSYNCD: 32.0 sha512 sha256 sha1 md5 md4\n")
	RsyncdExit          = []byte("@RSYNCD: EXIT\n")
)

const lineFeed = '\n'

type ConnInfo struct {
	Index         uint32
	LocalAddr     string
	RemoteAddr    string
	ConnectedAt   time.Time
	Module        string
	UpstreamAddr  string
	SentBytes     atomic.Int64
	ReceivedBytes atomic.Int64
}

func (c *ConnInfo) MarshalJSON() ([]byte, error) {
	// Handle atomic value (cannot marshal directly)
	return json.Marshal(struct {
		Index         uint32    `json:"index"`
		LocalAddr     string    `json:"local"`
		RemoteAddr    string    `json:"remote"`
		ConnectedAt   time.Time `json:"connected"`
		Module        string    `json:"module"`
		UpstreamAddr  string    `json:"upstream"`
		SentBytes     int64     `json:"sentBytes"`
		ReceivedBytes int64     `json:"receivedBytes"`
	}{
		Index:         c.Index,
		LocalAddr:     c.LocalAddr,
		RemoteAddr:    c.RemoteAddr,
		ConnectedAt:   c.ConnectedAt,
		Module:        c.Module,
		UpstreamAddr:  c.UpstreamAddr,
		SentBytes:     c.SentBytes.Load(),
		ReceivedBytes: c.ReceivedBytes.Load(),
	})
}

type Target struct {
	Addr             string
	UseProxyProtocol bool
}

type Server struct {
	// --- Options section
	// Listen Address
	ListenAddr     string
	TLSListenAddr  string
	HTTPListenAddr string
	ConfigPath     string

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// motd
	Motd string
	// --- End of options section

	accessLog, errorLog *logging.FileLogger

	reloadLock sync.RWMutex
	dialer     net.Dialer
	bufPool    sync.Pool
	// name -> upstream targets
	modules        map[string][]Target
	tlsCertificate *tls.Certificate
	tlsConfig      *tls.Config

	queue *queue.Queue

	activeConnCount atomic.Int64
	connIndex       atomic.Uint32
	connInfo        sync.Map

	TCPListener *net.TCPListener
	// TLSListener is not a TCPListener
	TLSListener  net.Listener
	HTTPListener *net.TCPListener
}

type countingReader struct {
	reader  io.Reader
	counter *atomic.Int64
}

func (cr *countingReader) Read(p []byte) (n int, err error) {
	n, err = cr.reader.Read(p)
	if n > 0 {
		cr.counter.Add(int64(n))
	}
	return n, err
}

func New() *Server {
	accessLog, _ := logging.NewFileLogger("")
	errorLog, _ := logging.NewFileLogger("")
	s := &Server{
		bufPool: sync.Pool{
			New: func() any {
				buf := make([]byte, TCPBufferSize)
				return &buf
			},
		},
		dialer:    net.Dialer{}, // customize keep alive interval?
		accessLog: accessLog,
		errorLog:  errorLog,
		queue:     queue.New(0, 0),
	}
	s.tlsConfig = &tls.Config{GetCertificate: s.getTLSCertificate}
	return s
}

func (s *Server) loadConfig(c *Config, openLog bool) error {
	var tlsCertificate *tls.Certificate
	serverStarted := s.TCPListener != nil || s.HTTPListener != nil || s.TLSListener != nil

	if len(c.Upstreams) == 0 {
		return fmt.Errorf("no upstream found")
	}
	if serverStarted {
		switch {
		case s.TLSListener == nil && c.Proxy.ListenTLS != "":
			return fmt.Errorf("listen_tls cannot be enabled on reload; restart required")
		case s.TLSListener != nil && c.Proxy.ListenTLS == "":
			return fmt.Errorf("listen_tls cannot be disabled on reload; restart required")
		}
	}
	if c.Proxy.ListenTLS == "" {
		if c.Proxy.TLSCertFile != "" || c.Proxy.TLSKeyFile != "" {
			log.Print("[WARN] tls_cert_file or tls_key_file is set but listen_tls is not set")
		}
	} else {
		if c.Proxy.TLSCertFile == "" || c.Proxy.TLSKeyFile == "" {
			return fmt.Errorf("listen_tls requires tls_cert_file and tls_key_file")
		}
		cert, err := tls.LoadX509KeyPair(c.Proxy.TLSCertFile, c.Proxy.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("load tls certificate: %w", err)
		}
		tlsCertificate = &cert
	}

	s.queue.SetMax(c.Proxy.MaxActiveConns, c.Proxy.MaxQueuedConns)

	modules := map[string][]Target{}
	upstreamNames := make([]string, 0, len(c.Upstreams))
	for upstreamName := range c.Upstreams {
		upstreamNames = append(upstreamNames, upstreamName)
	}
	sort.Strings(upstreamNames)
	for _, upstreamName := range upstreamNames {
		v := c.Upstreams[upstreamName]
		addr := v.Address
		_, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			return fmt.Errorf("resolve address: %w, upstream=%s, address=%s", err, upstreamName, addr)
		}
		target := Target{Addr: addr, UseProxyProtocol: v.UseProxyProtocol}
		for _, moduleName := range v.Modules {
			modules[moduleName] = append(modules[moduleName], target)
		}
	}

	s.reloadLock.Lock()
	defer s.reloadLock.Unlock()
	if s.ListenAddr == "" {
		s.ListenAddr = c.Proxy.Listen
	}
	if s.TLSListenAddr == "" {
		s.TLSListenAddr = c.Proxy.ListenTLS
	}
	if s.HTTPListenAddr == "" {
		s.HTTPListenAddr = c.Proxy.ListenHTTP
	}
	if openLog {
		if err := s.accessLog.SetFile(c.Proxy.AccessLog); err != nil {
			return err
		}
		if err := s.errorLog.SetFile(c.Proxy.ErrorLog); err != nil {
			return err
		}
	}
	s.Motd = c.Proxy.Motd
	s.modules = modules
	s.tlsCertificate = tlsCertificate
	return nil
}

func chooseTargetByClientIP(ip net.IP, targets []Target) Target {
	if len(targets) == 1 {
		return targets[0]
	}

	normalized := ip.To4()
	if normalized == nil {
		normalized = ip.To16()
	}
	if normalized == nil {
		return targets[0]
	}

	h := fnv.New32a()
	_, _ = h.Write(normalized)
	idx := h.Sum32() % uint32(len(targets))
	return targets[idx]
}

func (s *Server) getTLSCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.reloadLock.RLock()
	defer s.reloadLock.RUnlock()
	if s.tlsCertificate == nil {
		return nil, fmt.Errorf("tls certificate is not configured")
	}
	return s.tlsCertificate, nil
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
	_, err := writeWithTimeout(downConn, buf.Bytes(), timeout)
	return err
}

func (s *Server) relay(ctx context.Context, index uint32, downConn net.Conn) error {
	defer downConn.Close()

	info := ConnInfo{
		Index:       index,
		LocalAddr:   downConn.LocalAddr().String(),
		RemoteAddr:  downConn.RemoteAddr().String(),
		ConnectedAt: time.Now().Truncate(time.Second),
	}
	s.connInfo.Store(index, &info)
	defer s.connInfo.Delete(index)

	bufPtr := s.bufPool.Get().(*[]byte)
	defer s.bufPool.Put(bufPtr)
	buf := *bufPtr

	addr := downConn.RemoteAddr().String()
	ip := downConn.RemoteAddr().(*net.TCPAddr).IP.String()
	port := downConn.RemoteAddr().(*net.TCPAddr).Port

	writeTimeout := s.WriteTimeout
	readTimeout := s.ReadTimeout

	n, err := readLine(downConn, buf, readTimeout)
	if err != nil {
		return fmt.Errorf("read version from client %s: %w", addr, err)
	}
	rsyncdClientVersion := make([]byte, n)
	copy(rsyncdClientVersion, buf[:n])
	if !bytes.HasPrefix(rsyncdClientVersion, RsyncdVersionPrefix) {
		return fmt.Errorf("unknown version from client %s: %q", addr, rsyncdClientVersion)
	}

	_, err = writeWithTimeout(downConn, RsyncdServerVersion, writeTimeout)
	if err != nil {
		return fmt.Errorf("send version to client %s: %w", addr, err)
	}

	n, err = readLine(downConn, buf, readTimeout)
	if err != nil {
		return fmt.Errorf("read module from client %s: %w", addr, err)
	}
	if n == 0 {
		return fmt.Errorf("empty request from client %s", addr)
	}
	data := buf[:n]
	if s.Motd != "" {
		_, err = writeWithTimeout(downConn, []byte(s.Motd+"\n"), writeTimeout)
		if err != nil {
			return fmt.Errorf("send motd to client %s: %w", addr, err)
		}
	}

	chStatus := s.queue.Acquire()
	status := <-chStatus
	if !status.Ok() {
		// Send initial queuing notice
		msg := fmt.Sprintf("Server has reached the maximum number of %d connections. Your request is being queued.\n", s.queue.GetMax())
		msg += fmt.Sprintf("Your position: %d, Total queued: %d\n", status.Index+1, status.Max)
		_, err = writeWithTimeout(downConn, []byte(msg), writeTimeout)
		if err != nil {
			s.queue.Abort(chStatus)
			return fmt.Errorf("send queue notice to client %s: %w", addr, err)
		}

	queuing:
		for !status.Ok() {
			// Wait until status update, or repeat message after some time
			select {
			case status = <-chStatus:
				if status.Ok() {
					break queuing
				}
			case <-time.After(1 * time.Minute):
			}

			msg := fmt.Sprintf("Your position: %d, Total queued: %d\n", status.Index+1, status.Max)
			_, err = writeWithTimeout(downConn, []byte(msg), writeTimeout)
			if err != nil {
				s.queue.Abort(chStatus)
				return fmt.Errorf("send queue notice to client %s: %w", addr, err)
			}
		}
	}
	defer s.queue.Release()

	if len(data) == 1 { // single '\n'
		s.accessLog.F("client %s requests listing all modules", addr)
		return s.listAllModules(downConn)
	}

	moduleName := string(buf[:n-1]) // trim trailing \n
	info.Module = moduleName
	s.connInfo.Store(index, &info)

	s.reloadLock.RLock()
	targets, ok := s.modules[moduleName]
	s.reloadLock.RUnlock()

	if !ok {
		_, _ = writeWithTimeout(downConn, []byte(fmt.Sprintf("unknown module: %s\n", moduleName)), writeTimeout)
		_, _ = writeWithTimeout(downConn, RsyncdExit, writeTimeout)
		s.accessLog.F("client %s requests non-existing module %s", ip, moduleName)
		return nil
	}

	target := chooseTargetByClientIP(net.ParseIP(ip), targets)
	upstreamAddr := target.Addr
	useProxyProtocol := target.UseProxyProtocol
	info.UpstreamAddr = upstreamAddr
	s.connInfo.Store(index, &info)

	conn, err := s.dialer.DialContext(ctx, "tcp", upstreamAddr)
	if err != nil {
		return fmt.Errorf("dial to upstream: %s: %w", upstreamAddr, err)
	}
	upConn := conn.(*net.TCPConn)
	defer upConn.Close()
	upIp := upConn.RemoteAddr().(*net.TCPAddr).IP.String()
	upPort := upConn.RemoteAddr().(*net.TCPAddr).Port

	if useProxyProtocol {
		var IPVersion string
		if strings.Contains(ip, ":") {
			IPVersion = "TCP6"
		} else {
			IPVersion = "TCP4"
		}
		proxyHeader := fmt.Sprintf("PROXY %s %s %s %d %d\r\n", IPVersion, ip, upIp, port, upPort)
		_, err = writeWithTimeout(upConn, []byte(proxyHeader), writeTimeout)
		if err != nil {
			return fmt.Errorf("send proxy protocol header to upstream %s: %w", upIp, err)
		}
	}

	_, err = writeWithTimeout(upConn, rsyncdClientVersion, writeTimeout)
	if err != nil {
		return fmt.Errorf("send version to upstream %s: %w", upIp, err)
	}

	n, err = readLine(upConn, buf, readTimeout)
	if err != nil {
		return fmt.Errorf("read version from upstream %s: %w", upIp, err)
	}
	data = buf[:n]
	if !bytes.HasPrefix(data, RsyncdVersionPrefix) {
		return fmt.Errorf("unknown version from upstream %s: %s", upIp, data)
	}

	// send back the motd
	idx := bytes.IndexByte(data, lineFeed)
	if idx+1 < n {
		_, err = writeWithTimeout(downConn, data[idx+1:], writeTimeout)
		if err != nil {
			return fmt.Errorf("send motd to client %s: %w", ip, err)
		}
	}

	_, err = writeWithTimeout(upConn, []byte(moduleName+"\n"), writeTimeout)
	if err != nil {
		return fmt.Errorf("send module to upstream %s: %w", upIp, err)
	}

	s.accessLog.F("client %s starts requesting module %s", ip, moduleName)

	// reset read and write deadline for upConn and downConn
	zeroTime := time.Time{}
	_ = upConn.SetDeadline(zeroTime)
	_ = downConn.SetDeadline(zeroTime)

	// Use countingReader to track bytes in real-time
	// <sent> and <received> are with the client, not upstream
	downReader := &countingReader{reader: downConn, counter: &info.ReceivedBytes}
	upReader := &countingReader{reader: upConn, counter: &info.SentBytes}

	sentClosed := make(chan struct{})
	receivedClosed := make(chan struct{})

	go func() {
		_, err := io.Copy(upConn, downReader)
		if err != nil {
			s.errorLog.F("copy from downstream to upstream: %v", err)
		}
		close(sentClosed)
	}()
	go func() {
		_, err := io.Copy(downConn, upReader)
		if err != nil {
			s.errorLog.F("copy from upstream to downstream: %v", err)
		}
		close(receivedClosed)
	}()
	select {
	case <-receivedClosed:
		_ = upConn.SetLinger(0)
		_ = upConn.CloseRead()
	case <-sentClosed:
		if closeReader, ok := downConn.(interface{ CloseRead() error }); ok {
			_ = closeReader.CloseRead()
		}
	}

	sentBytes := info.SentBytes.Load()
	receivedBytes := info.ReceivedBytes.Load()

	duration := time.Since(info.ConnectedAt)
	s.accessLog.F("client %s finishes module %s (sent: %d, received: %d, duration: %s)", ip, moduleName, sentBytes, receivedBytes, duration)
	return nil
}

func (s *Server) GetActiveConnectionCount() int64 {
	return s.activeConnCount.Load()
}

func (s *Server) ListConnectionInfo() (result []*ConnInfo) {
	result = make([]*ConnInfo, 0, s.GetActiveConnectionCount())
	s.connInfo.Range(func(_, value any) bool {
		result = append(result, value.(*ConnInfo))
		return true
	})
	sort.Slice(result, func(i, j int) bool {
		return result[i].Index < result[j].Index
	})
	return
}

func (s *Server) runHTTPServer() error {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "(unknown)"
	}

	var mux http.ServeMux
	mux.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var resp struct {
			Message string `json:"message"`
		}

		err := s.ReadConfigFromFile(true)
		if err != nil {
			log.Printf("[ERROR] Load config: %s", err)
			s.errorLog.F("[ERROR] Load config: %s", err)
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
			Count       int         `json:"count"`
			Connections []*ConnInfo `json:"connections"`
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
		// https://docs.influxdata.com/influxdb/latest/reference/syntax/line-protocol/
		_, _ = fmt.Fprintf(w, "rsync-proxy,host=%s count=%d %d\n", hostname, count, timestamp)
	})

	return http.Serve(s.HTTPListener, &mux)
}

func (s *Server) Listen() error {
	l1, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return fmt.Errorf("create tcp listener: %w", err)
	}
	s.ListenAddr = l1.Addr().String()
	log.Printf("[INFO] Rsync proxy listening on %s", s.ListenAddr)

	var lTLS net.Listener
	if s.TLSListenAddr != "" {
		lTLS, err = net.Listen("tcp", s.TLSListenAddr)
		if err != nil {
			_ = l1.Close()
			return fmt.Errorf("create tls listener: %w", err)
		}
		s.TLSListenAddr = lTLS.Addr().String()
		log.Printf("[INFO] Rsync TLS proxy listening on %s", s.TLSListenAddr)
		lTLS = tls.NewListener(lTLS, s.tlsConfig)
	}

	l2, err := net.Listen("tcp", s.HTTPListenAddr)
	if err != nil {
		_ = l1.Close()
		if lTLS != nil {
			_ = lTLS.Close()
		}
		return fmt.Errorf("create http listener: %w", err)
	}
	s.HTTPListenAddr = l2.Addr().String()
	log.Printf("[INFO] HTTP server listening on %s", s.HTTPListenAddr)

	s.TCPListener = l1.(*net.TCPListener)
	s.TLSListener = lTLS
	s.HTTPListener = l2.(*net.TCPListener)
	return nil
}

func (s *Server) Close() {
	if s.TCPListener != nil {
		_ = s.TCPListener.Close()
	}
	if s.TLSListener != nil {
		_ = s.TLSListener.Close()
	}
	if s.HTTPListener != nil {
		_ = s.HTTPListener.Close()
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	s.activeConnCount.Add(1)
	defer s.activeConnCount.Add(-1)
	connIndex := s.connIndex.Add(1)

	err := s.relay(ctx, connIndex, conn)
	if err != nil {
		s.errorLog.F("handleConn: %s", err)
	}
}

func (s *Server) runRsyncServer(ctx context.Context, listener net.Listener, acceptErr string) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("%s: %w", acceptErr, err)
		}
		go s.handleConn(ctx, conn)
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
			errC <- fmt.Errorf("start http server: %w", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		err := s.runRsyncServer(ctx, s.TCPListener, "accept rsync connection")
		if err != nil {
			errC <- err
		}
	}()
	if s.TLSListener != nil {
		go func() {
			err := s.runRsyncServer(ctx, s.TLSListener, "accept tls rsync connection")
			if err != nil {
				errC <- err
			}
		}()
	}

	for {
		err := <-errC
		if err != nil {
			return err
		}
	}
}
