package server

import (
	"io"
	"os"

	"github.com/pelletier/go-toml"

	"github.com/ustclug/rsync-proxy/pkg/log"
)

type Upstream struct {
	Address string   `toml:"address"`
	Modules []string `toml:"modules"`
}

type ProxySettings struct {
	Listen     string `toml:"listen"`
	ListenHTTP string `toml:"listen_http"`
	Motd       string `toml:"motd"`
	AccessLog  string `toml:"access_log"`
	ErrorLog   string `toml:"error_log"`
}

type Config struct {
	Proxy     ProxySettings        `toml:"proxy"`
	Upstreams map[string]*Upstream `toml:"upstreams"`
}

func (s *Server) ReadConfig(r io.Reader) error {
	log.V(3).Infof("[INFO] loading config")

	dec := toml.NewDecoder(r)
	var c Config
	err := dec.Decode(&c)
	if err != nil {
		return err
	}
	return s.loadConfig(&c)
}

func (s *Server) ReadConfigFromFile() error {
	f, err := os.Open(s.ConfigPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.ReadConfig(f)
}
