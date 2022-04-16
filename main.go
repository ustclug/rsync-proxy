package main

import (
	"github.com/ustclug/rsync-proxy/cmd"
	"github.com/ustclug/rsync-proxy/pkg/log"
)

func main() {
	c := cmd.New()
	log.AddFlags(c.Flags())
	_ = c.Execute()
}
