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
	UseProxyProtocol bool     `toml:"use_proxy_protocol"`
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
	log.Print("[INFO] loading config")

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
