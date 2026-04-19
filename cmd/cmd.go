package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ustclug/rsync-proxy/pkg/server"
)

var (
	Version   = "0.0.0"
	GitCommit = "$Format:%H$"          // sha1 from git, output of $(git rev-parse HEAD)
	BuildDate = "1970-01-01T00:00:00Z" // build date in ISO8601 format, output of $(date -u +'%Y-%m-%dT%H:%M:%SZ')
)

func SendReloadRequest(addr string, stdout, stderr io.Writer) error {
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
		out = stdout
	} else {
		out = stderr
		err = fmt.Errorf("failed to reload")
	}
	_, _ = io.Copy(out, resp.Body)
	return err
}

func SendConnectionsRequest(addr string, stdout, stderr io.Writer) error {
	resp, err := http.Get(fmt.Sprintf("http://%s/status", addr))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(stderr, resp.Body)
		return fmt.Errorf("failed to get connections")
	}

	var result struct {
		Connections []struct {
			Index         int       `json:"index"`
			RemoteAddr    string    `json:"remote"`
			Module        string    `json:"module"`
			UpstreamAddr  string    `json:"upstream"`
			ConnectedAt   time.Time `json:"connected"`
			ReceivedBytes int64     `json:"receivedBytes"`
			SentBytes     int64     `json:"sentBytes"`
		} `json:"connections"`
		Count int `json:"count"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if result.Count == 0 {
		_, _ = fmt.Fprintln(stdout, "No active connections")
		return nil
	}

	_, _ = fmt.Fprintln(stdout, "=== Active Connections ===")
	for _, conn := range result.Connections {
		_, _ = fmt.Fprintf(stdout, "Index: %d, Addr: %s, Module: %s, Upstream: %s, Connected: %s, Recv: %d bytes, Send: %d bytes\n",
			conn.Index,
			conn.RemoteAddr,
			conn.Module,
			conn.UpstreamAddr,
			conn.ConnectedAt.Format("2006-01-02 15:04:05"),
			conn.ReceivedBytes,
			conn.SentBytes)
	}
	_, _ = fmt.Fprintln(stdout, "==========================")
	return nil
}

func printVersion(stdout io.Writer) error {
	type Info struct {
		GitCommit string
		BuildDate string
		Version   string
		GoVersion string
		Compiler  string
		Platform  string
	}
	enc := json.NewEncoder(stdout)
	return enc.Encode(Info{
		GitCommit: GitCommit,
		BuildDate: BuildDate,
		Version:   Version,

		GoVersion: runtime.Version(),
		Compiler:  runtime.Compiler,
		Platform:  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	})
}

func New() *cobra.Command {
	var (
		reload          bool
		version         bool
		connections     bool
		upstreamModules string
	)

	s := server.New()

	c := &cobra.Command{
		Use: "rsync-proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			if version {
				return printVersion(cmd.OutOrStdout())
			}

			log.SetOutput(cmd.ErrOrStderr())
			s.WriteTimeout = time.Minute
			s.ReadTimeout = time.Minute

			if upstreamModules != "" && strings.HasPrefix(upstreamModules, "rsync://") {
				parsed, err := url.Parse(upstreamModules)
				if err != nil {
					return fmt.Errorf("parse rsync url: %w", err)
				}
				if parsed.Host == "" {
					return fmt.Errorf("invalid rsync url: missing host")
				}
				if parsed.Path != "" && parsed.Path != "/" {
					return fmt.Errorf("invalid rsync url: path is not allowed")
				}
				modules, err := s.DiscoverModules(parsed.Host)
				if err != nil {
					return err
				}
				for _, name := range modules {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
				}
				return nil
			}

			// For helper commands, we don't want to open log file as rw, to allow other users to use.
			openLog := !reload && !connections && upstreamModules == ""
			err := s.ReadConfigFromFile(openLog)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if reload {
				return SendReloadRequest(s.HTTPListenAddr, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			if connections {
				return SendConnectionsRequest(s.HTTPListenAddr, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			if upstreamModules != "" {
				modules, err := s.ListUpstreamModules(upstreamModules)
				if err != nil {
					return err
				}
				for _, name := range modules {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
				}
				return nil
			}

			err = s.Listen()
			if err != nil {
				return err
			}
			return s.Run()
		},
	}
	flags := c.Flags()
	flags.StringVarP(&s.ConfigPath, "config", "c", "/etc/rsync-proxy/config.toml", "Path to config file")
	flags.BoolVar(&reload, "reload", false, "Inform server to reload config")
	flags.BoolVarP(&version, "version", "V", false, "Print version and exit")
	flags.BoolVar(&connections, "connections", false, "Show active connections")
	flags.StringVar(&upstreamModules, "upstream-modules", "", "Print modules for a configured upstream, or rsync URL (with port)")
	c.MarkFlagsMutuallyExclusive("reload", "connections", "upstream-modules")

	return c
}
