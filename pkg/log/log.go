package log

import (
	"flag"
	"fmt"
	"log"
	"os"
)

type Verbose bool

var (
	verbosity int

	stderr = log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile)
)

func (v Verbose) Infof(format string, a ...interface{}) {
	if v {
		_ = stderr.Output(2, fmt.Sprintf(format, a...))
	}
}

func V(lvl int) Verbose {
	return lvl <= verbosity
}

func AddFlags(flagSet *flag.FlagSet) {
	flagSet.IntVar(&verbosity, "v", 2, "Number for the log level verbosity. The higher the more verbose.")
}

func Fatalf(format string, a ...interface{}) {
	stderr.Fatalf(format, a...)
}

func Fatalln(a ...interface{}) {
	stderr.Fatalln(a...)
}
