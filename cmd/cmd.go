package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
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
		reload  bool
		version bool
	)

	s := server.New()

	c := &cobra.Command{
		Use: "rsync-proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			if version {
				return printVersion(cmd.OutOrStdout())
			}

			log.SetOutput(cmd.ErrOrStderr())

			err := s.ReadConfigFromFile()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if reload {
				return SendReloadRequest(s.HTTPListenAddr, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}

			s.WriteTimeout = time.Minute
			s.ReadTimeout = time.Minute

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

	return c
}
