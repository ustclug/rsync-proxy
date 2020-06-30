package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ustclug/rsync-proxy/pkg/log"
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

func New() *cobra.Command {
	var reload bool

	s := server.New()

	c := &cobra.Command{
		Use: "rsync-proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			if reload {
				return sendReloadRequest(s.WebListenAddr)
			}

			err := s.LoadConfigFromFile()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			s.WriteTimeout = time.Minute
			s.ReadTimeout = time.Minute

			return s.Run(context.Background())
		},
	}
	flags := c.Flags()
	flags.StringVar(&s.ListenAddr, "listen-addr", "0.0.0.0:9527", "Address to listen on for reverse proxy")
	flags.StringVar(&s.WebListenAddr, "web.listen-addr", "127.0.0.1:9528", "Address to listen on for API")
	flags.StringVar(&s.ConfigPath, "config", "/etc/rsync-proxy/config.toml", "Path to config file")
	flags.BoolVar(&reload, "reload", false, "Inform server to reload config")
	log.AddFlags(c.Flags())

	return c
}
