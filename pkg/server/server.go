package server

import (
	"bufio"
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ustclug/rsync-proxy/pkg/logging"
	"github.com/ustclug/rsync-proxy/pkg/queue"
)

const (
	ReadBufferSize = 256

	defaultRsyncPortString = "873"
)

var (
	RsyncdVersionPrefix = []byte("@RSYNCD:")
	// Daemon auth list is a must in server version since 32.0
	// See https://github.com/RsyncProject/rsync/blob/a6312e60c95e5ebb5764eaf18eb07be23420ebc6/clientserver.c#L203
	RsyncdServerVersion = []byte("@RSYNCD: 32.0 sha512 sha256 sha1 md5 md4\n")
	RsyncdExit          = []byte("@RSYNCD: EXIT\n")

	bufPool = &sync.Pool{
		New: func() any {
			buf := make([]byte, ReadBufferSize)
			return &buf
		},
	} // pool of (*[]byte)
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
	Upstream         string
	Addr             string
	UseProxyProtocol bool
}

type upstreamConfig struct {
	Name            string
	Target          Target
	Modules         []string
	DiscoverModules bool
	MaxActiveConns  int
	MaxQueuedConns  int
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

	Motd string
	// --- End of options section

	accessLog, errorLog *logging.FileLogger

	reloadLock sync.RWMutex
	dialer     net.Dialer
	// name -> upstream targets
	modules        map[string][]Target
	upstreams      []upstreamConfig
	tlsCertificate *tls.Certificate

	upstreamQueues map[string]*queue.Queue

	activeConnCount atomic.Int64
	connIndex       atomic.Uint32
	connInfo        sync.Map

	TCPListener  net.Listener
	TLSListener  net.Listener
	HTTPListener net.Listener
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
		dialer:         net.Dialer{}, // customize keep alive interval?
		accessLog:      accessLog,
		errorLog:       errorLog,
		upstreamQueues: make(map[string]*queue.Queue),
	}
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

	upstreams := make([]upstreamConfig, 0, len(c.Upstreams))
	upstreamNames := make([]string, 0, len(c.Upstreams))
	for upstreamName := range c.Upstreams {
		upstreamNames = append(upstreamNames, upstreamName)
	}
	sort.Strings(upstreamNames)
	for _, upstreamName := range upstreamNames {
		v := c.Upstreams[upstreamName]
		if len(v.Modules) == 0 && !v.DiscoverModules {
			return fmt.Errorf("upstream=%s must set modules or discover_modules", upstreamName)
		}
		addr := v.Address
		if err := validateTCPOrUnixAddr(addr); err != nil {
			return fmt.Errorf("resolve address: %w, upstream=%s, address=%s", err, upstreamName, addr)
		}
		upstreams = append(upstreams, upstreamConfig{
			Name:            upstreamName,
			Target:          Target{Upstream: upstreamName, Addr: addr, UseProxyProtocol: v.UseProxyProtocol},
			Modules:         slices.Clone(v.Modules),
			DiscoverModules: v.DiscoverModules,
			MaxActiveConns:  v.MaxActiveConns,
			MaxQueuedConns:  v.MaxQueuedConns,
		})
	}

	s.reloadLock.Lock()
	defer s.reloadLock.Unlock()

	var discoveredModules map[string][]string
	if openLog {
		var err error
		discoveredModules, err = s.discoverConfiguredModules(context.Background(), upstreams)
		if err != nil {
			return err
		}
	}
	resolvedUpstreams := resolveUpstreams(upstreams, discoveredModules)
	modules := buildModuleTargets(resolvedUpstreams)
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
	s.upstreams = resolvedUpstreams
	s.upstreamQueues = s.updateUpstreamQueuesLocked(resolvedUpstreams)
	s.tlsCertificate = tlsCertificate
	return nil
}

func resolveUpstreams(upstreams []upstreamConfig, discovered map[string][]string) []upstreamConfig {
	resolved := make([]upstreamConfig, 0, len(upstreams))
	for _, upstream := range upstreams {
		modules := slices.Clone(upstream.Modules)
		if upstream.DiscoverModules {
			modules = slices.Clone(discovered[upstream.Name])
		}
		resolved = append(resolved, upstreamConfig{
			Name:            upstream.Name,
			Target:          upstream.Target,
			Modules:         modules,
			DiscoverModules: upstream.DiscoverModules,
			MaxActiveConns:  upstream.MaxActiveConns,
			MaxQueuedConns:  upstream.MaxQueuedConns,
		})
	}
	return resolved
}

func (s *Server) updateUpstreamQueuesLocked(upstreams []upstreamConfig) map[string]*queue.Queue {
	queues := make(map[string]*queue.Queue, len(upstreams))
	for _, upstream := range upstreams {
		q, ok := s.upstreamQueues[upstream.Name]
		if !ok {
			q = queue.New(upstream.MaxActiveConns, upstream.MaxQueuedConns)
		} else {
			q.SetMax(upstream.MaxActiveConns, upstream.MaxQueuedConns)
		}
		queues[upstream.Name] = q
	}
	return queues
}

func (s *Server) getTargetsForModule(moduleName string) ([]Target, bool) {
	s.reloadLock.RLock()
	defer s.reloadLock.RUnlock()
	targets, ok := s.modules[moduleName]
	return targets, ok
}

func (s *Server) getQueueForUpstream(name string) (*queue.Queue, bool) {
	s.reloadLock.RLock()
	defer s.reloadLock.RUnlock()
	q, ok := s.upstreamQueues[name]
	return q, ok
}

func buildModuleTargets(upstreams []upstreamConfig) map[string][]Target {
	modules := map[string][]Target{}
	for _, upstream := range upstreams {
		for _, moduleName := range upstream.Modules {
			modules[moduleName] = append(modules[moduleName], upstream.Target)
		}
	}
	return modules
}

func (s *Server) ListUpstreamModules(name string, forceDiscover bool) ([]string, error) {
	s.reloadLock.RLock()
	defer s.reloadLock.RUnlock()
	for _, upstream := range s.upstreams {
		if upstream.Name != name {
			continue
		}
		if forceDiscover {
			modules, err := s.discoverModulesFromUpstream(context.Background(), upstream)
			if err != nil {
				return nil, fmt.Errorf("discover modules from upstream %s (%s): %w", upstream.Name, upstream.Target.Addr, err)
			}
			return modules, nil
		} else {
			modules := append([]string(nil), upstream.Modules...)
			sort.Strings(modules)
			return modules, nil
		}
	}
	return nil, fmt.Errorf("unknown upstream: %s", name)
}

func (s *Server) DiscoverModules(addr string) ([]string, error) {
	return s.DiscoverModulesWithProxyProtocol(addr, false)
}

func (s *Server) DiscoverModulesWithProxyProtocol(addr string, useProxyProtocol bool) ([]string, error) {
	modules, err := s.discoverModulesFromUpstream(context.Background(), upstreamConfig{
		Name:   addr,
		Target: Target{Upstream: addr, Addr: addr, UseProxyProtocol: useProxyProtocol},
	})
	if err != nil {
		return nil, err
	}
	return modules, nil
}

func (s *Server) discoverConfiguredModules(ctx context.Context, upstreams []upstreamConfig) (map[string][]string, error) {
	discovered := map[string][]string{}
	for _, upstream := range upstreams {
		if !upstream.DiscoverModules {
			continue
		}
		modules, err := s.discoverModulesFromUpstream(ctx, upstream)
		if err != nil {
			s.logModuleDiscoveryFailure(upstream, err)
			return nil, fmt.Errorf("discover modules from upstream %s (%s): %w", upstream.Name, upstream.Target.Addr, err)
		}
		discovered[upstream.Name] = modules
		s.logModuleDiscoverySuccess(upstream, modules)
	}
	return discovered, nil
}

func (s *Server) discoverModulesFromUpstream(ctx context.Context, upstream upstreamConfig) ([]string, error) {
	addr := upstream.Target.Addr
	addr = addDefaultTCPPort(addr, defaultRsyncPortString)
	conn, err := dialContextTCPOrUnix(ctx, s.dialer, addr)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	if upstream.Target.UseProxyProtocol {
		err := writeProxyProtocolHeader(conn, conn.LocalAddr(), conn.RemoteAddr(), s.WriteTimeout)
		if err != nil {
			return nil, fmt.Errorf("send proxy protocol header: %w", err)
		}
	}

	reader := bufio.NewReaderSize(conn, ReadBufferSize)
	if _, err := writeWithTimeout(conn, RsyncdServerVersion, s.WriteTimeout); err != nil {
		return nil, fmt.Errorf("send version: %w", err)
	}

	if s.ReadTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(s.ReadTimeout))
	}
	line, err := reader.ReadString(lineFeed)
	if err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}
	if !bytes.HasPrefix([]byte(line), RsyncdVersionPrefix) {
		return nil, fmt.Errorf("unexpected version response: %q", line)
	}

	if _, err := writeWithTimeout(conn, []byte{'\n'}, s.WriteTimeout); err != nil {
		return nil, fmt.Errorf("request module list: %w", err)
	}

	modules := make([]string, 0)
	for {
		if s.ReadTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.ReadTimeout))
		}
		line, err = reader.ReadString(lineFeed)
		if err != nil {
			return nil, fmt.Errorf("read module list: %w", err)
		}
		line = strings.TrimSuffix(line, string(lineFeed))
		if strings.HasPrefix(line, string(RsyncdVersionPrefix)) {
			break
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			// Empty line, previous content is more likely part of MOTD, so discard them
			modules = modules[:0]
			continue
		}
		modules = append(modules, fields[0])
	}
	sort.Strings(modules)
	return modules, nil
}

func (s *Server) logModuleDiscoveryFailure(upstream upstreamConfig, err error) {
	log.Printf("[WARN] discover modules from upstream %s (%s): %v", upstream.Name, upstream.Target.Addr, err)
	s.errorLog.F("[WARN] discover modules from upstream %s (%s): %v", upstream.Name, upstream.Target.Addr, err)
}

func (s *Server) logModuleDiscoverySuccess(upstream upstreamConfig, modules []string) {
	log.Printf("[INFO] discovered modules from upstream %s (%s): %s", upstream.Name, upstream.Target.Addr, strings.Join(modules, ", "))
	s.errorLog.F("[INFO] discovered modules from upstream %s (%s): %s", upstream.Name, upstream.Target.Addr, strings.Join(modules, ", "))
}

func chooseTargetByClientIP(ip net.IP, targetCount int) int {
	if targetCount <= 1 {
		return 0
	}

	normalized := ip.To4()
	if normalized == nil {
		normalized = ip.To16()
	}
	if normalized == nil {
		return 0
	}

	h := fnv.New32a()
	_, _ = h.Write(normalized)
	return int(h.Sum32() % uint32(targetCount))
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

	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)
	buf := *bufPtr

	addr := downConn.RemoteAddr().String()
	ip := netAddrToString(downConn.RemoteAddr())

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

	if len(data) == 1 { // single '\n'
		s.accessLog.F("client %s requests listing all modules", addr)
		return s.listAllModules(downConn)
	}

	moduleName := string(buf[:n-1]) // trim trailing \n
	info.Module = moduleName
	s.connInfo.Store(index, &info)

	targets, ok := s.getTargetsForModule(moduleName)
	if !ok {
		_, _ = writeWithTimeout(downConn, fmt.Appendf(nil, "unknown module: %s\n", moduleName), writeTimeout)
		_, _ = writeWithTimeout(downConn, RsyncdExit, writeTimeout)
		s.accessLog.F("client %s requests non-existing module %s", ip, moduleName)
		return nil
	}

	target := targets[chooseTargetByClientIP(net.ParseIP(ip), len(targets))]
	upstreamAddr := target.Addr
	useProxyProtocol := target.UseProxyProtocol
	info.UpstreamAddr = upstreamAddr
	s.connInfo.Store(index, &info)

	upstreamQueue, ok := s.getQueueForUpstream(target.Upstream)
	if !ok {
		return fmt.Errorf("no queue configured for upstream %s", target.Upstream)
	}

	handle := upstreamQueue.Acquire()
	defer handle.Release()
	status := <-handle.C
	if status.Full {
		s.accessLog.F("client %s queue full for module %s", ip, moduleName)
		_, _ = writeWithTimeout(downConn, []byte("Server queue is full for this upstream. Please retry later.\n"), writeTimeout)
		_, _ = writeWithTimeout(downConn, RsyncdExit, writeTimeout)
		return nil
	}
	if !status.Ok {
		s.accessLog.F("client %s starts queueing for module %s", ip, moduleName)
		// Queueing is isolated per upstream.
		msg := fmt.Sprintf("Upstream %s has reached the maximum number of %d connections. Your request is being queued.\n", target.Upstream, upstreamQueue.GetMax())
		msg += fmt.Sprintf("Your position: %d, Total queued: %d\n", status.Index+1, status.Max)
		_, err = writeWithTimeout(downConn, []byte(msg), writeTimeout)
		if err != nil {
			return fmt.Errorf("send queue notice to client %s: %w", addr, err)
		}

	queuing:
		for !status.Ok {
			select {
			case status = <-handle.C:
				if status.Ok {
					break queuing
				}
			case <-time.After(1 * time.Minute):
			}

			msg := fmt.Sprintf("Your position: %d, Total queued: %d\n", status.Index+1, status.Max)
			_, err = writeWithTimeout(downConn, []byte(msg), writeTimeout)
			if err != nil {
				return fmt.Errorf("send queue notice to client %s: %w", addr, err)
			}
		}
	}

	upConn, err := dialContextTCPOrUnix(ctx, s.dialer, upstreamAddr)
	if err != nil {
		return fmt.Errorf("dial to upstream: %s: %w", upstreamAddr, err)
	}
	defer upConn.Close()
	upAddr := netAddrToString(upConn.RemoteAddr())
	if useProxyProtocol {
		err := writeProxyProtocolHeader(upConn, downConn.RemoteAddr(), upConn.RemoteAddr(), s.WriteTimeout)
		if err != nil {
			return fmt.Errorf("send proxy protocol header to upstream %s: %w", upAddr, err)
		}
	}

	_, err = writeWithTimeout(upConn, rsyncdClientVersion, writeTimeout)
	if err != nil {
		return fmt.Errorf("send version to upstream %s: %w", upAddr, err)
	}

	n, err = readLine(upConn, buf, readTimeout)
	if err != nil {
		return fmt.Errorf("read version from upstream %s: %w", upAddr, err)
	}
	data = buf[:n]
	if !bytes.HasPrefix(data, RsyncdVersionPrefix) {
		return fmt.Errorf("unknown version from upstream %s: %s", upAddr, data)
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
		return fmt.Errorf("send module to upstream %s: %w", upAddr, err)
	}

	s.accessLog.F("client %s starts requesting module %s", ip, moduleName)

	// reset read and write deadline for upConn and downConn
	zeroTime := time.Time{}
	_ = upConn.SetDeadline(zeroTime)
	_ = downConn.SetDeadline(zeroTime)

	// Use countingReader to track bytes in real-time
	// <sent> and <received> are relative to the client, not upstream
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
		if err := closeRead(upConn, true); err != nil {
			s.errorLog.F("close upstream read: %v", err)
		}
	case <-sentClosed:
		if err := closeRead(downConn, false); err != nil {
			s.errorLog.F("close downstream read: %v", err)
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
	mux.HandleFunc("/upstream-modules", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		name := r.URL.Query().Get("name")
		if name == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(struct {
				Message string `json:"message"`
			}{Message: "missing upstream name"})
			return
		}

		forceDiscover := false
		forceDiscoverValues, ok := r.URL.Query()["force_discover"]
		if ok && len(forceDiscoverValues) > 0 {
			forceDiscover, err = strconv.ParseBool(forceDiscoverValues[0])
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(struct {
					Message string `json:"message"`
				}{Message: "invalid force_discover value"})
				return
			}
		}

		modules, err := s.ListUpstreamModules(name, forceDiscover)
		if err != nil {
			statusCode := http.StatusInternalServerError
			if strings.Contains(err.Error(), "unknown upstream") {
				statusCode = http.StatusNotFound
			}
			w.WriteHeader(statusCode)
			_ = json.NewEncoder(w).Encode(struct {
				Message string `json:"message"`
			}{Message: err.Error()})
			return
		}

		_ = json.NewEncoder(w).Encode(struct {
			Modules []string `json:"modules"`
		}{Modules: modules})
	})

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
	l1, err := listenTCPOrUnix(s.ListenAddr)
	if err != nil {
		return fmt.Errorf("create tcp listener: %w", err)
	}
	s.ListenAddr = l1.Addr().String()
	log.Printf("[INFO] Rsync proxy listening on %s", s.ListenAddr)

	var lTLS net.Listener
	if s.TLSListenAddr != "" {
		lTLS, err = listenTCPOrUnix(s.TLSListenAddr)
		if err != nil {
			_ = l1.Close()
			return fmt.Errorf("create tls listener: %w", err)
		}
		s.TLSListenAddr = lTLS.Addr().String()
		log.Printf("[INFO] Rsync TLS proxy listening on %s", s.TLSListenAddr)
		lTLS = tls.NewListener(lTLS, &tls.Config{GetCertificate: s.getTLSCertificate})
	}

	l2, err := listenTCPOrUnix(s.HTTPListenAddr)
	if err != nil {
		_ = l1.Close()
		if lTLS != nil {
			_ = lTLS.Close()
		}
		return fmt.Errorf("create http listener: %w", err)
	}
	s.HTTPListenAddr = l2.Addr().String()
	log.Printf("[INFO] HTTP server listening on %s", s.HTTPListenAddr)

	s.TCPListener = l1
	s.TLSListener = lTLS
	s.HTTPListener = l2
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

	defer func() {
		err := recover()
		if err != nil {
			s.errorLog.F("handleConn panicked: %s", err)
		}
	}()

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
	errC := make(chan error, 1)
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
