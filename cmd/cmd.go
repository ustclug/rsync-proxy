package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"github.com/spf13/cobra"

	"github.com/ustclug/rsync-proxy/pkg/server"
)

const DefaultUnixSocketPath = "/run/rsync-proxy/rsync-proxy.sock"

var (
	Version   = "0.0.0"
	GitCommit = "$Format:%H$"          // sha1 from git, output of $(git rev-parse HEAD)
	BuildDate = "1970-01-01T00:00:00Z" // build date in ISO8601 format, output of $(date -u +'%Y-%m-%dT%H:%M:%SZ')

	daemonSocket = DefaultUnixSocketPath
	dialer       = &net.Dialer{}
)

func makeHttpClient(addr string) *http.Client {
	addrFamily := "tcp"
	if strings.HasPrefix(addr, "/") {
		addrFamily = "unix"
	}
	return &http.Client{
		Timeout: time.Second * 10,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, addrFamily, addr)
			},
		},
	}
}

func httpGet(addr string, path string) (*http.Response, error) {
	return makeHttpClient(addr).Get("http://." + path)
}

func httpPost(addr string, path string, contentType string, body io.Reader) (*http.Response, error) {
	return makeHttpClient(addr).Post("http://."+path, contentType, body)
}

func SendReloadRequest(addr string, stdout, stderr io.Writer) error {
	resp, err := httpPost(addr, "/reload", "application/json", nil)
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
	resp, err := httpGet(addr, "/status")
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

	table := tablewriter.NewTable(
		stdout,
		tablewriter.WithRendition(tw.Rendition{
			Borders: tw.BorderNone,
			Settings: tw.Settings{
				Lines:      tw.LinesNone,
				Separators: tw.SeparatorsNone,
			},
		}),
		tablewriter.WithPadding(tw.Padding{
			Right:     "  ",
			Overwrite: true,
		}),
		tablewriter.WithHeaderAutoFormat(tw.Off),
		tablewriter.WithAlignment(tw.Alignment{
			tw.AlignRight,   // Index
			tw.AlignRight,   // RemoteAddr
			tw.AlignDefault, // Module
			tw.AlignRight,   // UpstreamAddr
			tw.AlignDefault, // ConnectedAt
			tw.AlignRight,   // ReceivedBytes
			tw.AlignRight,   // SentBytes
		}),
	)
	table.Header("Index", "Remote", "Module", "Upstream", "Connected", "Received", "Sent")
	for _, conn := range result.Connections {
		_ = table.Append([]string{
			strconv.Itoa(conn.Index),
			conn.RemoteAddr,
			conn.Module,
			conn.UpstreamAddr,
			conn.ConnectedAt.Format(time.DateTime),
			strconv.FormatInt(conn.ReceivedBytes, 10),
			strconv.FormatInt(conn.SentBytes, 10),
		})
	}
	return table.Render()
}

func SendUpstreamModulesRequest(addr string, upstream string, forceDiscover bool, stdout, stderr io.Writer) error {
	query := url.Values{}
	query.Set("name", upstream)
	if forceDiscover {
		query.Set("force_discover", strconv.FormatBool(forceDiscover))
	}

	resp, err := httpGet(addr, "/upstream-modules?"+query.Encode())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(stderr, resp.Body)
		return fmt.Errorf("failed to get upstream modules")
	}

	var result struct {
		Modules []string `json:"modules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	sort.Strings(result.Modules)
	for _, name := range result.Modules {
		_, _ = fmt.Fprintln(stdout, name)
	}
	return nil
}

func printVersion(out io.Writer, pretty bool) error {
	type Info struct {
		GitCommit string
		BuildDate string
		Version   string
		GoVersion string
		Compiler  string
		Platform  string
	}
	enc := json.NewEncoder(out)
	if pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(Info{
		GitCommit: GitCommit,
		BuildDate: BuildDate,
		Version:   Version,

		GoVersion: runtime.Version(),
		Compiler:  runtime.Compiler,
		Platform:  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	})
}

func newConnectionsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "connections",
		Short: "Show active connections",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return SendConnectionsRequest(daemonSocket, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	return c
}

func newReloadCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "reload",
		Short: "Inform server to reload config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return SendReloadRequest(daemonSocket, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	return c
}

func newUpstreamModulesCmd(s *server.Server) *cobra.Command {
	var useProxyProtocol bool
	var forceDiscover bool
	c := &cobra.Command{
		Use:   "upstream-modules <upstream>",
		Short: "Print modules for a configured upstream, or rsync URL (with port)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			upstreamModules := args[0]
			if upstreamModules == "" {
				return fmt.Errorf("empty upstream spec")
			}

			if strings.HasPrefix(upstreamModules, "rsync://") {
				parsed, err := url.Parse(upstreamModules)
				if err != nil {
					return fmt.Errorf("parse rsync url: %w", err)
				}
				rsyncHost := parsed.Host
				if parsed.Host == "" {
					// Unix socket
					rsyncHost = parsed.Path
				} else if parsed.Path != "" && parsed.Path != "/" {
					return fmt.Errorf("invalid rsync url: path is not allowed")
				}
				modules, err := s.DiscoverModulesWithProxyProtocol(rsyncHost, useProxyProtocol)
				if err != nil {
					return err
				}
				for _, name := range modules {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
				}
				return nil
			}

			return SendUpstreamModulesRequest(daemonSocket, upstreamModules, forceDiscover, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	c.Flags().BoolVar(&useProxyProtocol, "proxy-protocol", false, "Send a PROXY protocol header when discovering modules from an rsync URL")
	c.Flags().BoolVar(&forceDiscover, "force-discover", false, "Always try discover upstream modules")
	return c
}

func newVersionCmd() *cobra.Command {
	var pretty bool
	c := &cobra.Command{
		Use:   "version",
		Short: "Print version and exit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return printVersion(cmd.OutOrStdout(), pretty)
		},
	}
	c.Flags().BoolVarP(&pretty, "pretty", "p", false, "Pretty-print JSON output")
	return c
}

func New() *cobra.Command {
	var version bool

	s := server.New()
	s.WriteTimeout = time.Minute
	s.ReadTimeout = time.Minute

	c := &cobra.Command{
		Use: "rsync-proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			if version {
				return printVersion(cmd.OutOrStdout(), false)
			}

			log.SetOutput(cmd.ErrOrStderr())
			if err := s.ReadConfigFromFile(true); err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			if err := s.Listen(); err != nil {
				return fmt.Errorf("server listen: %w", err)
			}
			return s.Run()
		},
		SilenceUsage: true,
	}
	pFlags := c.PersistentFlags()
	pFlags.StringVarP(&daemonSocket, "host", "H", DefaultUnixSocketPath, "Daemon socket to connect to")
	pFlags.StringVarP(&s.ConfigPath, "config", "c", "/etc/rsync-proxy/config.toml", "Path to config file")
	pFlags.BoolVarP(&version, "version", "V", false, "Print version and exit")

	c.AddCommand(
		newConnectionsCmd(),
		newReloadCmd(),
		newUpstreamModulesCmd(s),
		newVersionCmd(),
	)

	return c
}
