package bt

import (
	"log"
	"os"
)

// Logger is the minimal logging interface used for SDK diagnostics.
// The standard library *log.Logger satisfies it.
type Logger interface {
	Printf(format string, v ...interface{})
}

// defaultDiagLogger writes SDK diagnostics to stderr with a stable prefix.
var defaultDiagLogger Logger = log.New(os.Stderr, "[backtrace] ", log.LstdFlags)

// diag is an internal logging helper. Diagnostics are emitted only when
// debug mode is enabled; the reporting API never panics and never writes to
// stdout/stderr unless debugging was requested. (The bcd tracing
// integration has its own logging and panic semantics; see GlobalConfig.)
type diag struct {
	logger Logger
	debug  bool
}

func (d diag) logf(format string, v ...interface{}) {
	if !d.debug {
		return
	}
	l := d.logger
	if l == nil {
		l = defaultDiagLogger
	}
	l.Printf(format, v...)
}
