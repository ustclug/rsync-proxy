//go:build !linux

package cmd

import "github.com/ustclug/rsync-proxy/pkg/server"

func runDaemon(s *server.Server) error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Run()
}
