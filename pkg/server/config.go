package server

import (
	"io"
	"log"
	"os"

	"github.com/pelletier/go-toml"
)

type Upstream struct {
	Address          string   `toml:"address"`
	Modules          []string `toml:"modules"`
	DiscoverModules  bool     `toml:"discover_modules"`
	UseProxyProtocol bool     `toml:"use_proxy_protocol"`
	MaxActiveConns   int      `toml:"max_active_connections"`
	MaxQueuedConns   int      `toml:"max_queued_connections"`
	// PerIPMaxActiveConns overrides the proxy-wide
	// per_ip_max_active_connections setting for this upstream. A
	// value of 0 (the default, i.e. field omitted) means the
	// upstream inherits the proxy-wide value.
	PerIPMaxActiveConns int `toml:"per_ip_max_active_connections"`
}

type ProxySettings struct {
	Listen      string `toml:"listen"`
	ListenTLS   string `toml:"listen_tls"`
	ListenHTTP  string `toml:"listen_http"`
	Motd        string `toml:"motd"`
	AccessLog   string `toml:"access_log"`
	ErrorLog    string `toml:"error_log"`
	TLSCertFile string `toml:"tls_cert_file"`
	TLSKeyFile  string `toml:"tls_key_file"`
	// RelayIdleTimeoutSecs is the idle timeout (in seconds) applied
	// during the bidirectional relay phase of a connection. If no data
	// flows in either direction for this duration, the connection is
	// terminated. 0 (the default) disables the timeout.
	//
	// This mirrors the semantics of the rsyncd "timeout" setting (see
	// rsyncd.conf(5)), which is an I/O timeout. A common choice for
	// public mirrors is 600.
	RelayIdleTimeoutSecs int `toml:"relay_idle_timeout"`
	// RelayMaxDurationSecs is the maximum total wall-clock duration
	// (in seconds) of the bidirectional relay phase. When exceeded
	// the proxy closes the connection regardless of activity. 0 (the
	// default) disables this hard cap. rsync clients will typically
	// reconnect and resume on the next run.
	RelayMaxDurationSecs int `toml:"relay_max_duration"`
	// TCPKeepAliveSecs enables TCP keepalive on accepted client
	// connections and on dialed upstream connections. The value is
	// the keepalive period in seconds; 0 (the default) leaves the
	// OS-default keepalive behavior in place (typically: disabled or
	// ~2 hours). Enabling this helps detect half-open connections
	// (peer crashed, NAT reaped) within minutes rather than hours.
	TCPKeepAliveSecs int `toml:"tcp_keepalive"`
	// PerIPMaxActiveConns is the proxy-wide default for the per-IP
	// per-upstream concurrency cap applied during the relay phase.
	// 0 (the default) disables the limit. Each upstream may
	// override this via [upstreams.X].per_ip_max_active_connections.
	PerIPMaxActiveConns int `toml:"per_ip_max_active_connections"`
	// DialTimeoutSecs caps how long the proxy waits when dialing an
	// upstream rsync server. A value of 0 (the default) leaves the
	// OS-default TCP connect behavior in place (~75s on Linux). When
	// an upstream is unreachable this surfaces a fast failure to
	// the client and increments rsync_proxy_upstream_dial_errors_total
	// without tying up the listener for the full kernel SYN-retry
	// budget.
	DialTimeoutSecs int `toml:"dial_timeout"`
	// MinThroughputBytes, MinThroughputWindowSecs and
	// MinThroughputGraceSecs together implement a throughput floor
	// during the relay phase. Within any window of
	// min_throughput_window seconds the connection must transfer at
	// least min_throughput_bytes (counting both directions); if it
	// does not, the proxy closes the connection. The grace period
	// suppresses the check for the first min_throughput_grace seconds
	// after the relay starts to avoid killing a slow-start session.
	// Setting min_throughput_bytes or min_throughput_window to 0 (the
	// default) disables the floor. min_throughput_grace defaults to
	// the value of min_throughput_window when 0.
	MinThroughputBytes      int64 `toml:"min_throughput_bytes"`
	MinThroughputWindowSecs int   `toml:"min_throughput_window"`
	MinThroughputGraceSecs  int   `toml:"min_throughput_grace"`
}

type Config struct {
	Proxy     ProxySettings        `toml:"proxy"`
	Upstreams map[string]*Upstream `toml:"upstreams"`
}

func (s *Server) ReadConfig(r io.Reader, openLog bool) error {
	log.Print("[INFO] loading config")

	dec := toml.NewDecoder(r)
	var c Config
	err := dec.Decode(&c)
	if err != nil {
		return err
	}
	return s.loadConfig(&c, openLog)
}

func (s *Server) ReadConfigFromFile(openLog bool) error {
	f, err := os.Open(s.ConfigPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.ReadConfig(f, openLog)
}
