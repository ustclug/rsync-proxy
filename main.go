package main

import (
	"github.com/ustclug/rsync-proxy/cmd"
	"github.com/ustclug/rsync-proxy/pkg/log"
)

func main() {
	c := cmd.New()
	err := c.Execute()
	if err != nil {
		log.Fatalln("Error:", err)
	}
}
