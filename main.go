package main

import (
	"github.com/ustclug/rsync-proxy/cmd"
)

func main() {
	_ = cmd.New().Execute()
}
