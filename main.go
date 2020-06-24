package main

import (
	"context"
	"log"
	"time"

	"github.com/ustclug/rsync-proxy/pkg/server"
)

func main() {
	s := server.New()
	s.WriteTimeout = time.Minute
	s.ReadTimeout = time.Minute
	s.ListenAddr = "127.0.0.1:9999"
	s.Upstreams = map[string]*server.Upstream{
		"foo": {Host: "127.0.0.1", Port: 1234, Modules: []string{"foo"}},
		"bar": {Host: "127.0.0.1", Port: 1235, Modules: []string{"bar"}},
	}
	s.DefaultUpstreamName = "foo"
	err := s.Run(context.Background())
	if err != nil {
		log.Fatalf("Error: %s", err)
	}
}
