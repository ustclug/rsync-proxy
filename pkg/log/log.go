package log

import (
	"fmt"
	"io"
	"log"
	"os"

	"github.com/spf13/pflag"
)

type Verbose bool

var (
	verbosity int

	stderr = log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile)
	stdout = log.New(os.Stdout, "", log.Ldate|log.Ltime|log.Lshortfile)
)

func (v Verbose) Infof(format string, a ...interface{}) {
	if v {
		_ = stdout.Output(2, fmt.Sprintf(format, a...))
	}
}

func (v Verbose) Errorf(format string, a ...interface{}) {
	if v {
		_ = stderr.Output(2, fmt.Sprintf(format, a...))
	}
}

func SetOutput(out, errOut io.Writer) {
	stdout.SetOutput(out)
	stderr.SetOutput(errOut)
}

func V(lvl int) Verbose {
	return lvl <= verbosity
}

func AddFlags(flagSet *pflag.FlagSet) {
	flagSet.IntVarP(&verbosity, "verbose", "v", 2, "Number for the log level verbosity. The higher the more verbose.")
}

func Fatalf(format string, a ...interface{}) {
	stderr.Fatalf(format, a...)
}

func Fatalln(a ...interface{}) {
	stderr.Fatalln(a...)
}

func Errorf(format string, a ...interface{}) {
	stderr.Printf(format, a...)
}
