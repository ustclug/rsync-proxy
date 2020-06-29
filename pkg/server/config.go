package server

import (
	"io"
	"log"
	"os"

	"github.com/pelletier/go-toml"
)

type Upstream struct {
	Host    string
	Port    int
	Modules []string
}

type Config struct {
	Upstreams           map[string]*Upstream `toml:"upstreams"`
	DefaultUpstreamName string               `toml:"default_upstream"`
}

func (s *Server) LoadConfig(r io.Reader) error {
	log.Printf("[INFO] loading config")

	dec := toml.NewDecoder(r)
	var c Config
	err := dec.Decode(&c)
	if err != nil {
		return err
	}

	s.Upstreams = c.Upstreams
	s.DefaultUpstreamName = c.DefaultUpstreamName
	return s.complete()
}

func (s *Server) LoadConfigFromFile() error {
	f, err := os.Open(s.ConfigPath)
	if err != nil {
		return err
	}
	defer f.Close()
	err = s.LoadConfig(f)
	if err != nil {
		return err
	}
	return nil
}