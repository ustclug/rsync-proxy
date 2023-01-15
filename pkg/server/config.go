package server

import (
	"io"
	"os"

	"github.com/pelletier/go-toml"

	"github.com/ustclug/rsync-proxy/pkg/log"
)

type Upstream struct {
	Host    string   `toml:"host"`
	Port    int      `toml:"port"`
	Modules []string `toml:"modules"`
}

type ProxySettings struct {
	Listen     string `toml:"listen"`
	ListenHTTP string `toml:"listen_http"`
	Motd       string `toml:"motd"`
}

type Config struct {
	Proxy     ProxySettings        `toml:"proxy"`
	Upstreams map[string]*Upstream `toml:"upstreams"`
}

func (s *Server) LoadConfig(r io.Reader) error {
	log.V(3).Infof("[INFO] loading config")

	dec := toml.NewDecoder(r)
	var c Config
	err := dec.Decode(&c)
	if err != nil {
		return err
	}

	s.ListenAddr = c.Proxy.Listen
	s.HTTPListenAddr = c.Proxy.ListenHTTP
	s.Motd = c.Proxy.Motd
	s.Upstreams = c.Upstreams
	return s.complete()
}

func (s *Server) LoadConfigFromFile() error {
	f, err := os.Open(s.ConfigPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.LoadConfig(f)
}
