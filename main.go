package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/ustclug/rsync-proxy/pkg/server"
)

func sendReloadRequest(addr string) error {
	client := http.Client{
		Timeout: time.Second * 10,
	}

	resp, err := client.Post(fmt.Sprintf("http://%s/reload", addr), "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out io.Writer
	if resp.StatusCode < 300 {
		out = os.Stdout
	} else {
		out = os.Stderr
	}
	_, _ = io.Copy(out, resp.Body)
	return nil
}

func main() {
	var reload bool
	s := server.New()
	flag.StringVar(&s.ListenAddr, "listen-addr", "0.0.0.0:9527", "Address to listen on for reverse proxy")
	flag.StringVar(&s.WebListenAddr, "web.listen-addr", "127.0.0.1:9528", "Address to listen on for API")
	flag.StringVar(&s.ConfigPath, "config", "/etc/rsync-proxy/config.toml", "Path to config file")
	flag.BoolVar(&reload, "reload", false, "Inform server to reload config")
	flag.Parse()

	if reload {
		err := sendReloadRequest(s.WebListenAddr)
		if err != nil {
			log.Fatalln(err)
		}
		return
	}

	err := s.LoadConfigFromFile()
	if err != nil {
		log.Fatalf("Load config: %s", err)
	}

	s.WriteTimeout = time.Minute
	s.ReadTimeout = time.Minute

	err = s.Run(context.Background())
	if err != nil {
		log.Fatalf("Error: %s", err)
	}
}
