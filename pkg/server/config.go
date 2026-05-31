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
