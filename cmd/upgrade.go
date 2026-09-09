package cmd

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/pelletier/go-toml"
	"github.com/spf13/cobra"

	"github.com/ustclug/rsync-proxy/pkg/server"
)

func sendControl(addr, path string, timeout time.Duration, out io.Writer) error {
	client := makeHttpClient(addr)
	client.Timeout = timeout + 5*time.Second
	resp, err := client.Post("http://."+path, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(out, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("control request failed: %s", resp.Status)
	}
	return nil
}

func newUpgradeCmd(s *server.Server) *cobra.Command {
	var timeout time.Duration
	c := &cobra.Command{Use: "upgrade", Short: "Start the installed binary and drain the previous generation", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if timeout <= 0 {
				return fmt.Errorf("timeout must be positive")
			}
			addr, err := controlAddress(c, s)
			if err != nil {
				return err
			}
			return sendControl(addr, "/upgrade?timeout="+url.QueryEscape(timeout.String()), timeout, c.OutOrStdout())
		}}
	c.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "New generation readiness timeout")
	return c
}

func newReopenLogsCmd(s *server.Server) *cobra.Command {
	return &cobra.Command{Use: "reopen-logs", Short: "Reopen log files in all live generations", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			addr, err := controlAddress(c, s)
			if err != nil {
				return err
			}
			return sendControl(addr, "/reopen-logs", 30*time.Second, c.OutOrStdout())
		}}
}

// Lifecycle commands honor --config without opening logs, discovering modules,
// or otherwise initializing a second server. Explicit --host takes precedence.
func controlAddress(c *cobra.Command, s *server.Server) (string, error) {
	if c.Flags().Changed("host") || c.InheritedFlags().Changed("host") {
		return daemonSocket, nil
	}
	f, err := os.Open(s.ConfigPath)
	if os.IsNotExist(err) {
		return daemonSocket, nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	var config server.Config
	if err = toml.NewDecoder(f).Decode(&config); err != nil {
		return "", err
	}
	if config.Proxy.ListenHTTP != "" {
		return config.Proxy.ListenHTTP, nil
	}
	return daemonSocket, nil
}
