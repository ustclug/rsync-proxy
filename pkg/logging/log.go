package logging

import (
	"log"
	"os"
)

func init() {
	// Don't repeat timestamp if logging to systemd journal (v231+)
	if _, ok := os.LookupEnv("JOURNAL_STREAM"); ok {
		log.SetFlags(log.Flags() &^ (log.Ldate | log.Ltime))
	}
}
