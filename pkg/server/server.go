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
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ustclug/rsync-proxy/pkg/logging"
	"github.com/ustclug/rsync-proxy/pkg/queue"
	"github.com/ustclug/rsync-proxy/pkg/state"
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
	mu            sync.RWMutex
	Index         uint32
	Generation    string
	LocalAddr     string
	RemoteAddr    string
	ConnectedAt   time.Time
	Module        string
	Upstream      string
	SentBytes     atomic.Int64
	ReceivedBytes atomic.Int64
}

type connInfoSnapshot struct {
	Index         uint32    `json:"index"`
	Generation    string    `json:"generation,omitempty"`
	LocalAddr     string    `json:"local"`
	RemoteAddr    string    `json:"remote"`
	ConnectedAt   time.Time `json:"connected"`
	Module        string    `json:"module"`
	Upstream      string    `json:"upstream"`
	SentBytes     int64     `json:"sentBytes"`
	ReceivedBytes int64     `json:"receivedBytes"`
}

type accessJSONRecord struct {
	LogType        string  `json:"log_type"`
	Timestamp      float64 `json:"timestamp"`
	ClientEndpoint string  `json:"client_endpoint"`
	ServerEndpoint string  `json:"server_endpoint"`
	Module         string  `json:"module"`
	Backend        string  `json:"backend"`
	Status         int     `json:"status"`
	BytesReceived  int64   `json:"bytes_received"`
	BytesSent      int64   `json:"bytes_sent"`
	SessionTime    float64 `json:"session_time"`
	RsyncVersion   string  `json:"rsync_version"`
	RsyncdVersion  string  `json:"rsyncd_version"`
	TLSProtocol    string  `json:"tls_protocol"`
}

func (c *ConnInfo) SetModule(module string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Module = module
}

func (c *ConnInfo) SetUpstream(upstream string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Upstream = upstream
}

func (c *ConnInfo) snapshot() connInfoSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return connInfoSnapshot{
		Index:         c.Index,
		Generation:    c.Generation,
		LocalAddr:     c.LocalAddr,
		RemoteAddr:    c.RemoteAddr,
		ConnectedAt:   c.ConnectedAt,
		Module:        c.Module,
		Upstream:      c.Upstream,
		SentBytes:     c.SentBytes.Load(),
		ReceivedBytes: c.ReceivedBytes.Load(),
	}
}

func (c *ConnInfo) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.snapshot())
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
	// PerIPMaxActiveConns is the resolved (effective) limit on the
	// number of concurrent active relay connections from a single
	// client IP to this upstream. 0 means no per-IP cap. Computed at
	// load time as: per-upstream override (Upstream.PerIPMaxActiveConns)
	// or, if zero, the proxy-wide default (Proxy.PerIPMaxActiveConns).
	PerIPMaxActiveConns int
}

// upstreamCounters holds per-upstream failure counters.
type upstreamCounters struct {
	queueFull                 atomic.Uint64
	dialError                 atomic.Uint64
	perIPRejected             atomic.Uint64
	idleTerminated            atomic.Uint64
	maxDurationTerminated     atomic.Uint64
	throughputFloorTerminated atomic.Uint64
}

// perIPCountKey identifies a (upstream, client IP) pair for tracking
// per-IP per-upstream concurrent active relay connections.
type perIPCountKey struct {
	upstream string
	ip       string
}

// moduleUpstreamKey identifies a (module, upstream) pair for per-module
// lifetime counters.
type moduleUpstreamKey struct {
	module   string
	upstream string
}

// moduleCounters holds per-(module, upstream) lifetime counters that are
// updated when a relay finishes successfully.
type moduleCounters struct {
	completed atomic.Uint64
	sentBytes atomic.Uint64
	recvBytes atomic.Uint64
}

type Server struct {
	Started chan struct{}
	// --- Options section
	// Listen Address
	ListenAddr          string
	TLSListenAddr       string
	HTTPListenAddr      string
	ConfigPath          string
	StateDir            string
	UpgradeDrainTimeout time.Duration
	Generation          string
	Shared              *state.Store
	// ControlHandler handles daemon lifecycle requests before the normal API.
	ControlHandler      func(http.ResponseWriter, *http.Request) bool
	listenFactory       func(string) (net.Listener, error)
	rawTLSListener      net.Listener
	configuredListeners [3]string
	lifecycleMu         sync.Mutex
	forceClosed         bool
	acceptWG            sync.WaitGroup
	sessions            map[net.Conn]context.CancelFunc
	sessionsWG          sync.WaitGroup
	stopOnce            sync.Once
	stopC               chan struct{}
	admission           *admission
	HTTPServer          *http.Server

	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// RelayIdleTimeout is the idle (no I/O activity in either
	// direction) timeout applied during the bidirectional relay phase
	// of a connection. A value of 0 (the default) disables the
	// timeout, matching rsyncd's "timeout = 0" behavior. See
	// rsyncd.conf(5).
	RelayIdleTimeout time.Duration

	// RelayMaxDuration is a hard cap on the total wall-clock
	// duration of the bidirectional relay phase of a connection.
	// When exceeded the proxy closes both directions regardless of
	// activity. 0 (the default) disables the cap.
	RelayMaxDuration time.Duration

	// TCPKeepAlive is the keepalive period applied to accepted
	// client connections and to dialed upstream connections. 0 (the
	// default) leaves the OS-default keepalive behavior in place
	// (typically: disabled, or ~2 hours).
	TCPKeepAlive time.Duration

	// MinThroughputBytes, MinThroughputWindow and
	// MinThroughputGrace together implement a throughput floor
	// during the relay phase. Within any window of
	// MinThroughputWindow the connection must transfer at least
	// MinThroughputBytes (counting both directions); otherwise the
	// proxy closes the connection. The grace period suppresses the
	// check for the first MinThroughputGrace after the relay
	// starts. Setting MinThroughputBytes or MinThroughputWindow to
	// zero disables the floor.
	MinThroughputBytes  int64
	MinThroughputWindow time.Duration
	MinThroughputGrace  time.Duration

	Motd string
	// --- End of options section

	accessLog, accessJSONLog, errorLog *logging.FileLogger

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

	acceptedConnCount  atomic.Uint64
	completedConnCount atomic.Uint64
	sentBytesTotal     atomic.Uint64
	recvBytesTotal     atomic.Uint64

	// Per-upstream failure counters. Lazy-initialized via getUpstreamCounters.
	// map key is upstream name. Value is *upstreamCounters.
	upstreamCounters   sync.Map
	unknownModuleCount atomic.Uint64

	// Per-(module, upstream) counters tracked when a relay finishes
	// successfully. Lazy-initialized via getModuleCounters.
	// map key is moduleUpstreamKey. Value is *moduleCounters.
	moduleCounters sync.Map

	// Per-(upstream, client IP) active-connection counters.
	// Lazy-initialized via getPerIPCounter.
	// map key is perIPCountKey. Value is *atomic.Int64.
	perIPCounts sync.Map

	TCPListener  net.Listener
	TLSListener  net.Listener
	HTTPListener net.Listener
}

type countingReader struct {
	reader  io.Reader
	counter *atomic.Int64
	// lastActivity is the UnixNano timestamp of the most recent
	// successful read (n > 0). It is updated atomically so that an
	// idle watcher goroutine can observe activity without locking.
	// May be nil when activity tracking is not needed.
	lastActivity *atomic.Int64
}

func (cr *countingReader) Read(p []byte) (n int, err error) {
	n, err = cr.reader.Read(p)
	if n > 0 {
		cr.counter.Add(int64(n))
		if cr.lastActivity != nil {
			cr.lastActivity.Store(time.Now().UnixNano())
		}
	}
	return n, err
}

func New() *Server {
	accessLog, _ := logging.NewFileLogger("")
	accessJSONLog, _ := logging.NewFileLogger("")
	accessJSONLog.SetFlags(0)
	errorLog, _ := logging.NewFileLogger("")
	s := &Server{
		Started:        make(chan struct{}),
		dialer:         net.Dialer{}, // customize keep alive interval?
		accessLog:      accessLog,
		accessJSONLog:  accessJSONLog,
		errorLog:       errorLog,
		upstreamQueues: make(map[string]*queue.Queue),
		sessions:       make(map[net.Conn]context.CancelFunc),
		stopC:          make(chan struct{}),
	}
	return s
}

func (s *Server) loadConfig(c *Config, openLog bool) error {
	var tlsCertificate *tls.Certificate
	serverStarted := s.TCPListener != nil || s.HTTPListener != nil || s.TLSListener != nil

	if c.Proxy.UpgradeDrainTimeoutSecs < 0 || c.Proxy.RelayIdleTimeoutSecs < 0 {
		return fmt.Errorf("timeouts must be non-negative")
	}
	if c.Proxy.StateDir != "" {
		var err error
		c.Proxy.StateDir, err = filepath.Abs(c.Proxy.StateDir)
		if err != nil {
			return err
		}
	}
	if serverStarted && c.Proxy.StateDir != "" && c.Proxy.StateDir != s.StateDir {
		return fmt.Errorf("state_dir cannot change on reload")
	}
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

	if c.Proxy.RelayMaxDurationSecs < 0 {
		return fmt.Errorf("relay_max_duration must be non-negative, got %d", c.Proxy.RelayMaxDurationSecs)
	}
	if c.Proxy.TCPKeepAliveSecs < 0 {
		return fmt.Errorf("tcp_keepalive must be non-negative, got %d", c.Proxy.TCPKeepAliveSecs)
	}
	if c.Proxy.PerIPMaxActiveConns < 0 {
		return fmt.Errorf("per_ip_max_active_connections must be non-negative, got %d", c.Proxy.PerIPMaxActiveConns)
	}
	if c.Proxy.DialTimeoutSecs < 0 {
		return fmt.Errorf("dial_timeout must be non-negative, got %d", c.Proxy.DialTimeoutSecs)
	}
	if c.Proxy.MinThroughputBytes < 0 {
		return fmt.Errorf("min_throughput_bytes must be non-negative, got %d", c.Proxy.MinThroughputBytes)
	}
	if c.Proxy.MinThroughputWindowSecs < 0 {
		return fmt.Errorf("min_throughput_window must be non-negative, got %d", c.Proxy.MinThroughputWindowSecs)
	}
	if c.Proxy.MinThroughputGraceSecs < 0 {
		return fmt.Errorf("min_throughput_grace must be non-negative, got %d", c.Proxy.MinThroughputGraceSecs)
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
		if v.MaxActiveConns < 0 || v.MaxQueuedConns < 0 {
			return fmt.Errorf("upstream=%s: connection limits must be non-negative", upstreamName)
		}
		if v.PerIPMaxActiveConns < 0 {
			return fmt.Errorf("upstream=%s: per_ip_max_active_connections must be non-negative, got %d", upstreamName, v.PerIPMaxActiveConns)
		}
		addr := v.Address
		if err := validateTCPOrUnixAddr(addr); err != nil {
			return fmt.Errorf("resolve address: %w, upstream=%s, address=%s", err, upstreamName, addr)
		}
		// Resolve effective per-IP cap: per-upstream override wins;
		// fall back to proxy-wide default; 0 means no cap.
		effectivePerIP := v.PerIPMaxActiveConns
		if effectivePerIP == 0 {
			effectivePerIP = c.Proxy.PerIPMaxActiveConns
		}
		upstreams = append(upstreams, upstreamConfig{
			Name:                upstreamName,
			Target:              Target{Upstream: upstreamName, Addr: addr, UseProxyProtocol: v.UseProxyProtocol},
			Modules:             slices.Clone(v.Modules),
			DiscoverModules:     v.DiscoverModules,
			MaxActiveConns:      v.MaxActiveConns,
			MaxQueuedConns:      v.MaxQueuedConns,
			PerIPMaxActiveConns: effectivePerIP,
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
		if err := s.accessJSONLog.SetFile(c.Proxy.AccessJSONLog); err != nil {
			return err
		}
		if err := s.errorLog.SetFile(c.Proxy.ErrorLog); err != nil {
			return err
		}
	}
	if s.Shared != nil {
		if err := s.Shared.SetLimits(s.Generation, sharedLimits(resolvedUpstreams)); err != nil {
			return err
		}
	}
	if !serverStarted {
		s.configuredListeners = [3]string{c.Proxy.Listen, c.Proxy.ListenTLS, c.Proxy.ListenHTTP}
		s.StateDir = c.Proxy.StateDir
	}
	s.UpgradeDrainTimeout = time.Duration(c.Proxy.UpgradeDrainTimeoutSecs) * time.Second
	s.Motd = c.Proxy.Motd
	s.RelayIdleTimeout = time.Duration(c.Proxy.RelayIdleTimeoutSecs) * time.Second
	s.RelayMaxDuration = time.Duration(c.Proxy.RelayMaxDurationSecs) * time.Second
	s.TCPKeepAlive = time.Duration(c.Proxy.TCPKeepAliveSecs) * time.Second
	s.MinThroughputBytes = c.Proxy.MinThroughputBytes
	windowSecs := c.Proxy.MinThroughputWindowSecs
	if windowSecs == 0 {
		windowSecs = 60
	}
	s.MinThroughputWindow = time.Duration(windowSecs) * time.Second
	graceSecs := c.Proxy.MinThroughputGraceSecs
	if graceSecs == 0 {
		// Default the grace period to the window itself: a fresh
		// connection gets one full window to ramp up before the
		// floor is enforced.
		graceSecs = windowSecs
	}
	s.MinThroughputGrace = time.Duration(graceSecs) * time.Second
	// Reflect the new keepalive and dial-timeout settings on the
	// dialer used to dial upstreams. The dialer is consulted under
	// reloadLock via getDialer() to avoid racing with reloads.
	s.dialer.KeepAlive = s.TCPKeepAlive
	s.dialer.Timeout = time.Duration(c.Proxy.DialTimeoutSecs) * time.Second
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
			Name:                upstream.Name,
			Target:              upstream.Target,
			Modules:             modules,
			DiscoverModules:     upstream.DiscoverModules,
			MaxActiveConns:      upstream.MaxActiveConns,
			MaxQueuedConns:      upstream.MaxQueuedConns,
			PerIPMaxActiveConns: upstream.PerIPMaxActiveConns,
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

// getUpstreamCounters returns the per-upstream counters, creating them lazily
// on first reference. Safe for concurrent use.
func (s *Server) getUpstreamCounters(name string) *upstreamCounters {
	if v, ok := s.upstreamCounters.Load(name); ok {
		return v.(*upstreamCounters)
	}
	v, _ := s.upstreamCounters.LoadOrStore(name, &upstreamCounters{})
	return v.(*upstreamCounters)
}

// getModuleCounters returns the per-(module, upstream) counters, creating
// them lazily on first reference. Safe for concurrent use.
//
// Empty module/upstream values are normalized to "unknown" so that the
// internal sync.Map key matches what prometheusLabelValueOrUnknown emits at
// scrape time. Otherwise an empty string and the literal "unknown" would
// produce two distinct map entries that render to the same Prometheus label
// set, leading to duplicate output lines.
func (s *Server) getModuleCounters(module, upstream string) *moduleCounters {
	key := moduleUpstreamKey{
		module:   prometheusLabelValueOrUnknown(module),
		upstream: prometheusLabelValueOrUnknown(upstream),
	}
	if v, ok := s.moduleCounters.Load(key); ok {
		return v.(*moduleCounters)
	}
	v, _ := s.moduleCounters.LoadOrStore(key, &moduleCounters{})
	return v.(*moduleCounters)
}

// getPerIPCounter returns the (upstream, ip) active-connection counter,
// creating it lazily. Safe for concurrent use.
func (s *Server) getPerIPCounter(upstream, ip string) *atomic.Int64 {
	key := perIPCountKey{upstream: upstream, ip: ip}
	if v, ok := s.perIPCounts.Load(key); ok {
		return v.(*atomic.Int64)
	}
	v, _ := s.perIPCounts.LoadOrStore(key, &atomic.Int64{})
	return v.(*atomic.Int64)
}

// getDialer returns a copy of the current upstream dialer, taking the
// reload lock to avoid racing with config reloads that mutate the
// dialer (e.g. KeepAlive). The returned value is safe to pass by value
// to dialContextTCPOrUnix.
func (s *Server) getDialer() net.Dialer {
	s.reloadLock.RLock()
	defer s.reloadLock.RUnlock()
	return s.dialer
}

// relayTimings is a snapshot of all relay-phase timing settings,
// captured atomically under the reload lock. Callers should consume
// this snapshot for the lifetime of a single relay connection so the
// behavior remains consistent across reloads.
type relayTimings struct {
	idle         time.Duration
	maxDuration  time.Duration
	tcpKeepAlive time.Duration
	minBytes     int64
	minWindow    time.Duration
	minGrace     time.Duration
}

// getRelayTimings returns a snapshot of the relay-phase timing
// settings under the reload lock. These fields are mutated by config
// reloads, so callers must not read them directly.
func (s *Server) getRelayTimings() relayTimings {
	s.reloadLock.RLock()
	defer s.reloadLock.RUnlock()
	return relayTimings{
		idle:         s.RelayIdleTimeout,
		maxDuration:  s.RelayMaxDuration,
		tcpKeepAlive: s.TCPKeepAlive,
		minBytes:     s.MinThroughputBytes,
		minWindow:    s.MinThroughputWindow,
		minGrace:     s.MinThroughputGrace,
	}
}

// getTCPKeepAlive returns the configured TCP keepalive period under
// the reload lock.
func (s *Server) getTCPKeepAlive() time.Duration {
	s.reloadLock.RLock()
	defer s.reloadLock.RUnlock()
	return s.TCPKeepAlive
}

// getPerIPLimitForUpstream returns the configured per-IP active
// connection cap for the named upstream, or 0 if none is set or the
// upstream is unknown. Safe for concurrent use.
func (s *Server) getPerIPLimitForUpstream(name string) int {
	s.reloadLock.RLock()
	defer s.reloadLock.RUnlock()
	for i := range s.upstreams {
		if s.upstreams[i].Name == name {
			return s.upstreams[i].PerIPMaxActiveConns
		}
	}
	return 0
}

// applyTCPKeepAlive enables TCP keepalive on the given connection if
// it is a *net.TCPConn, possibly wrapped by a connection such as
// *tls.Conn that exposes its underlying connection via NetConn.
// Other connection types (e.g. unix sockets) are silently ignored.
func applyTCPKeepAlive(conn net.Conn, period time.Duration) {
	if period <= 0 {
		return
	}
	if wrapped, ok := conn.(interface{ NetConn() net.Conn }); ok {
		conn = wrapped.NetConn()
	}
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(period)
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

func (s *Server) listAllModules(downConn net.Conn) (int, error) {
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
	return writeWithTimeout(downConn, buf.Bytes(), timeout)
}

func (s *Server) relay(ctx context.Context, index uint32, downConn net.Conn) (returnErr error) {
	defer downConn.Close()
	sessionStartedAt := time.Now()

	info := ConnInfo{
		Index:       index,
		Generation:  s.Generation,
		LocalAddr:   downConn.LocalAddr().String(),
		RemoteAddr:  downConn.RemoteAddr().String(),
		ConnectedAt: time.Now().Truncate(time.Second),
	}
	record := accessJSONRecord{
		LogType:        "rsync",
		ClientEndpoint: downConn.RemoteAddr().String(),
		Status:         http.StatusOK,
		RsyncVersion:   "-",
		RsyncdVersion:  "-",
		TLSProtocol:    tlsProtocolVersion(downConn),
	}
	defer func() {
		now := time.Now()
		if returnErr != nil && record.Status == http.StatusOK {
			record.Status = http.StatusInternalServerError
		}
		record.Timestamp = float64(now.UnixMilli()) / 1000
		record.BytesReceived = info.ReceivedBytes.Load()
		record.BytesSent = info.SentBytes.Load()
		record.SessionTime = float64(now.Sub(sessionStartedAt).Milliseconds()) / 1000
		encoded, err := json.Marshal(record)
		if err != nil {
			s.errorLog.F("marshal JSON access log: %v", err)
			return
		}
		s.accessJSONLog.Ln(string(encoded))
	}()
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
	record.RsyncVersion = rsyncProtocolVersion(rsyncdClientVersion)

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
		n, err := s.listAllModules(downConn)
		info.SentBytes.Add(int64(n))
		return err
	}

	moduleName := string(buf[:n-1]) // trim trailing \n
	info.SetModule(moduleName)
	record.Module = moduleName

	targets, ok := s.getTargetsForModule(moduleName)
	if !ok {
		// Use the rsyncd "@ERROR:" wire format so that the rsync
		// client treats this as a fatal protocol error and exits with
		// a non-zero status (RERR_FERROR_XFER, exit 5), matching the
		// behavior of a real rsyncd. Sending only "unknown module:
		// ...\n" followed by "@RSYNCD: EXIT" caused the client to
		// exit 0, which masked the failure for downstream tools such
		// as tunasync (which then marked the job as success).
		s.unknownModuleCount.Add(1)
		record.Status = http.StatusNotFound
		_, _ = writeWithTimeout(downConn, fmt.Appendf(nil, "@ERROR: Unknown module '%s'\n", moduleName), writeTimeout)
		s.accessLog.F("client %s requests non-existing module %s", ip, moduleName)
		return nil
	}

	target := targets[chooseTargetByClientIP(net.ParseIP(ip), len(targets))]
	upstreamAddr := target.Addr
	useProxyProtocol := target.UseProxyProtocol
	info.SetUpstream(target.Upstream)
	record.Backend = target.Upstream
	record.ServerEndpoint = upstreamAddr

	upstreamQueue, ok := s.getQueueForUpstream(target.Upstream)
	if !ok {
		return fmt.Errorf("no queue configured for upstream %s", target.Upstream)
	}

	handle, err := s.acquire(ctx, target.Upstream, ip, upstreamQueue)
	if errors.Is(err, state.ErrPerIP) {
		s.getUpstreamCounters(target.Upstream).perIPRejected.Add(1)
		record.Status = http.StatusTooManyRequests
		s.accessLog.F("client %s rejected for upstream %s module %s: %v", ip, target.Upstream, moduleName, err)
		_, _ = writeWithTimeout(downConn, fmt.Appendf(nil, "@ERROR: per-IP connection limit for upstream %s reached, retry later\n", target.Upstream), writeTimeout)
		return nil
	}
	if err != nil {
		return fmt.Errorf("admit connection: %w", err)
	}
	defer handle.release()
	var status queue.Status
	select {
	case status = <-handle.c:
	case <-ctx.Done():
		return ctx.Err()
	}
	if status.Full {
		s.getUpstreamCounters(target.Upstream).queueFull.Add(1)
		record.Status = http.StatusServiceUnavailable
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
			case <-ctx.Done():
				return ctx.Err()
			case status = <-handle.c:
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

	upConn, err := dialContextTCPOrUnix(ctx, s.getDialer(), upstreamAddr)
	if err != nil {
		s.getUpstreamCounters(target.Upstream).dialError.Add(1)
		record.Status = http.StatusBadGateway
		return fmt.Errorf("dial to upstream: %s: %w", upstreamAddr, err)
	}
	defer upConn.Close()
	stopUpstream := context.AfterFunc(ctx, func() { _ = upConn.Close() })
	defer stopUpstream()
	record.ServerEndpoint = upConn.RemoteAddr().String()
	// Enable TCP keepalive on the upstream-side connection so that a
	// dead/half-open peer is detected within the configured period
	// rather than relying on the OS default (commonly ~2 hours, or
	// disabled). dialer.KeepAlive only takes effect for the initial
	// SYN window; explicitly setting it on the resulting *TCPConn
	// is portable and idempotent.
	applyTCPKeepAlive(upConn, s.getTCPKeepAlive())
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
	record.RsyncdVersion = rsyncProtocolVersion(data)

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

	// Optional watchers on the bidirectional relay phase:
	//   * RelayIdleTimeout terminates a connection that has had no
	//     data flow in either direction for the configured duration
	//     (rsyncd "timeout" semantics, rsyncd.conf(5)).
	//   * RelayMaxDuration is a hard wall-clock cap on the entire
	//     relay phase; any connection still alive past this point is
	//     terminated regardless of activity.
	//   * MinThroughputBytes/MinThroughputWindow enforce a minimum
	//     average throughput on the relay; a connection that has
	//     transferred fewer than minBytes over the last window is
	//     considered to be slow-leeching and terminated. The grace
	//     period suppresses the check during initial ramp-up so
	//     short rsync exchanges (file lists, etc.) are not killed.
	// A single goroutine handles all checks. The shared
	// watcherClosed flag is used by the io.Copy goroutines to
	// suppress the expected "use of closed network connection" error
	// log when the watcher initiated the close. The reason for the
	// closure is recorded directly by the watcher into per-upstream
	// counters and access log.
	timings := s.getRelayTimings()
	idleTimeout := timings.idle
	maxDuration := timings.maxDuration
	minBytes := timings.minBytes
	minWindow := timings.minWindow
	minGrace := timings.minGrace
	throughputEnabled := minBytes > 0 && minWindow > 0
	relayStartedAt := time.Now()
	var idleTimedOut atomic.Bool
	sentClosed := make(chan struct{})
	receivedClosed := make(chan struct{})

	if idleTimeout > 0 || maxDuration > 0 || throughputEnabled {
		var lastActivity *atomic.Int64
		if idleTimeout > 0 {
			lastActivity = &atomic.Int64{}
			lastActivity.Store(relayStartedAt.UnixNano())
			downReader.lastActivity = lastActivity
			upReader.lastActivity = lastActivity
		}

		// Pick a wakeup interval as 1/4 of the smallest enabled window
		// so the worst-case detection latency is roughly 1.25x the
		// configured value, while keeping wakeup overhead negligible.
		// A 1s floor is then applied: detection latency therefore
		// degrades for sub-4-second settings (e.g. a 500ms idle
		// timeout will detect inside ~1s + the 500ms slack rather
		// than 125ms). In practice all production-meaningful values
		// are well above 4 seconds, so this only matters for tests.
		interval := time.Duration(0)
		bumpInterval := func(d time.Duration) {
			if d <= 0 {
				return
			}
			if interval == 0 || d < interval {
				interval = d
			}
		}
		if idleTimeout > 0 {
			bumpInterval(idleTimeout / 4)
		}
		if maxDuration > 0 {
			bumpInterval(maxDuration / 4)
		}
		if throughputEnabled {
			bumpInterval(minWindow / 4)
		}
		if interval < time.Second {
			interval = time.Second
		}

		upstreamName := target.Upstream
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			// Sliding-window throughput state. lastSampleTime moves
			// forward each time we reach a full window, with
			// lastSampleBytes capturing the cumulative byte count at
			// that moment. delta over the next window must meet
			// minBytes or the connection is terminated.
			lastSampleTime := relayStartedAt
			lastSampleBytes := int64(0)
			for {
				select {
				case <-sentClosed:
					return
				case <-receivedClosed:
					return
				case now := <-ticker.C:
					if idleTimeout > 0 {
						last := time.Unix(0, lastActivity.Load())
						if now.Sub(last) >= idleTimeout {
							idleTimedOut.Store(true)
							s.getUpstreamCounters(upstreamName).idleTerminated.Add(1)
							s.accessLog.F("client %s idle for module %s exceeds %s, closing", ip, moduleName, idleTimeout)
							_ = upConn.Close()
							_ = downConn.Close()
							return
						}
					}
					if maxDuration > 0 && now.Sub(relayStartedAt) >= maxDuration {
						idleTimedOut.Store(true)
						s.getUpstreamCounters(upstreamName).maxDurationTerminated.Add(1)
						s.accessLog.F("client %s exceeded max relay duration %s for module %s, closing", ip, maxDuration, moduleName)
						_ = upConn.Close()
						_ = downConn.Close()
						return
					}
					if throughputEnabled && now.Sub(relayStartedAt) >= minGrace && now.Sub(lastSampleTime) >= minWindow {
						curBytes := info.SentBytes.Load() + info.ReceivedBytes.Load()
						delta := curBytes - lastSampleBytes
						if delta < minBytes {
							idleTimedOut.Store(true)
							s.getUpstreamCounters(upstreamName).throughputFloorTerminated.Add(1)
							s.accessLog.F("client %s for module %s below throughput floor (%d bytes < %d bytes in %s), closing",
								ip, moduleName, delta, minBytes, minWindow)
							_ = upConn.Close()
							_ = downConn.Close()
							return
						}
						lastSampleTime = now
						lastSampleBytes = curBytes
					}
				}
			}
		}()
	}

	go func() {
		_, err := io.Copy(upConn, downReader)
		if err != nil && !idleTimedOut.Load() {
			s.errorLog.F("copy from downstream to upstream: %v", err)
		}
		close(sentClosed)
	}()
	go func() {
		_, err := io.Copy(downConn, upReader)
		if err != nil && !idleTimedOut.Load() {
			s.errorLog.F("copy from upstream to downstream: %v", err)
		}
		close(receivedClosed)
	}()
	select {
	case <-receivedClosed:
		if err := closeRead(upConn, true); err != nil {
			s.errorLog.F("close upstream read: %v", err)
		}
		downConn.Close()
	case <-sentClosed:
		if err := closeRead(downConn, false); err != nil {
			s.errorLog.F("close downstream read: %v", err)
		}
		upConn.Close()
	}
	<-sentClosed
	<-receivedClosed

	sentBytes := info.SentBytes.Load()
	receivedBytes := info.ReceivedBytes.Load()

	s.completedConnCount.Add(1)
	s.sentBytesTotal.Add(uint64(sentBytes))
	s.recvBytesTotal.Add(uint64(receivedBytes))

	mc := s.getModuleCounters(moduleName, target.Upstream)
	mc.completed.Add(1)
	mc.sentBytes.Add(uint64(sentBytes))
	mc.recvBytes.Add(uint64(receivedBytes))

	duration := time.Since(info.ConnectedAt)
	s.accessLog.F("client %s finishes module %s (sent: %d, received: %d, duration: %s)", ip, moduleName, sentBytes, receivedBytes, duration)
	return nil
}

func rsyncProtocolVersion(line []byte) string {
	fields := strings.Fields(strings.TrimPrefix(string(line), string(RsyncdVersionPrefix)))
	if len(fields) == 0 {
		return "-"
	}
	return fields[0]
}

func tlsProtocolVersion(conn net.Conn) string {
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return ""
	}
	return tls.VersionName(tlsConn.ConnectionState().Version)
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

		status, err := s.Status()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(status)
	})

	mux.HandleFunc("/telegraf", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		timestamp := time.Now().Truncate(time.Second).UnixNano()
		count := s.GetActiveConnectionCount()
		if s.Shared != nil {
			status, err := s.Status()
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			count = int64(status.Count)
		}
		// https://docs.influxdata.com/influxdb/latest/reference/syntax/line-protocol/
		_, _ = fmt.Fprintf(w, "rsync-proxy,host=%s count=%d %d\n", hostname, count, timestamp)
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var metrics bytes.Buffer
		if s.Shared != nil {
			if err := s.AggregateMetrics(&metrics); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		} else {
			s.writePrometheusMetrics(&metrics, time.Now())
		}
		promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{DisableCompression: true, EnableOpenMetrics: false}).ServeHTTP(w, r)
		_, _ = w.Write(metrics.Bytes())
	})

	s.HTTPServer = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.ControlHandler != nil && s.ControlHandler(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	})}
	close(s.Started)
	return s.HTTPServer.Serve(s.HTTPListener)
}

func (s *Server) Listen() error {
	baseListen := s.listenFactory
	if baseListen == nil {
		baseListen = listenTCPOrUnix
	}
	listen := func(addr string) (net.Listener, error) {
		l, err := baseListen(addr)
		// A shared listener's path belongs to the serving daemon, not an individual
		// generation. Set this before starting Accept/Close goroutines.
		if s.Shared != nil {
			if u, ok := l.(*net.UnixListener); ok {
				u.SetUnlinkOnClose(false)
			}
		}
		return l, err
	}
	l1, err := listen(s.ListenAddr)
	if err != nil {
		return fmt.Errorf("create tcp listener: %w", err)
	}
	s.ListenAddr = l1.Addr().String()
	log.Printf("[INFO] Rsync proxy listening on %s", s.ListenAddr)

	var lTLS net.Listener
	if s.TLSListenAddr != "" {
		lTLS, err = listen(s.TLSListenAddr)
		if err != nil {
			_ = l1.Close()
			return fmt.Errorf("create tls listener: %w", err)
		}
		s.TLSListenAddr = lTLS.Addr().String()
		log.Printf("[INFO] Rsync TLS proxy listening on %s", s.TLSListenAddr)
		s.rawTLSListener = lTLS
		lTLS = tls.NewListener(lTLS, &tls.Config{GetCertificate: s.getTLSCertificate})
	}

	l2, err := listen(s.HTTPListenAddr)
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

// StopAccepting starts a drain without cancelling accepted sessions.
func (s *Server) StopAccepting() {
	s.stopOnce.Do(func() {
		for _, l := range []net.Listener{s.TCPListener, s.TLSListener, s.HTTPListener} {
			if l != nil {
				_ = l.Close()
			}
		}
		close(s.stopC)
	})
}

// Close stops admission and aborts all accepted sessions, including queue waits.
func (s *Server) Close() { s.StopAccepting(); s.AbortConnections() }

func (s *Server) AbortConnections() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.forceClosed = true
	for conn, cancel := range s.sessions {
		cancel()
		_ = conn.Close()
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer s.activeConnCount.Add(-1)
	s.acceptedConnCount.Add(1)
	connIndex := s.connIndex.Add(1)

	// Apply TCP keepalive on the accepted client connection so that
	// half-open connections (peer crashed, NAT entry reaped) are
	// detected within the configured period rather than waiting for
	// the OS default. No-op on unix-domain or non-TCP listeners.
	applyTCPKeepAlive(conn, s.getTCPKeepAlive())

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
			if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("%s: %w", acceptErr, err)
		}
		s.lifecycleMu.Lock()
		if s.forceClosed {
			s.lifecycleMu.Unlock()
			_ = conn.Close()
			continue
		}
		connCtx, cancel := context.WithCancel(ctx)
		s.sessions[conn] = cancel
		s.sessionsWG.Add(1)
		s.activeConnCount.Add(1)
		s.lifecycleMu.Unlock()
		go func() {
			defer s.sessionsWG.Done()
			defer func() {
				cancel()
				_ = conn.Close()
				s.lifecycleMu.Lock()
				delete(s.sessions, conn)
				s.lifecycleMu.Unlock()
			}()
			s.handleConn(connCtx, conn)
		}()
	}
}

func (s *Server) Run() error {
	errC := make(chan error, 3)
	go func() {
		err := s.runHTTPServer()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
				return
			}
			errC <- fmt.Errorf("start http server: %w", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.acceptWG.Add(1)
	go func() {
		defer s.acceptWG.Done()
		err := s.runRsyncServer(ctx, s.TCPListener, "accept rsync connection")
		if err != nil {
			errC <- err
		}
	}()
	if s.TLSListener != nil {
		s.acceptWG.Add(1)
		go func() {
			defer s.acceptWG.Done()
			err := s.runRsyncServer(ctx, s.TLSListener, "accept tls rsync connection")
			if err != nil {
				errC <- err
			}
		}()
	}

	select {
	case err := <-errC:
		s.Close()
		s.acceptWG.Wait()
		s.sessionsWG.Wait()
		return err
	case <-s.stopC:
		s.acceptWG.Wait()
		s.sessionsWG.Wait()
		return nil
	}
}
