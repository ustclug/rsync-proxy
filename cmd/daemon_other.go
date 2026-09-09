//go:build !linux

package cmd

import (
	"fmt"
	"github.com/ustclug/rsync-proxy/pkg/server"
)

func runDaemon(s *server.Server) error {
	if err := s.ReadConfigFromFile(true); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Run()
}
