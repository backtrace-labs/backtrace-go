package bt

import (
	"log"
	"os"
	"sync/atomic"
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

	// busy, when set, is a per-client re-entrancy guard: a Logger that
	// calls back into the SDK (a logging-adapter pattern) would otherwise
	// recurse Printf -> Report -> drop -> logf -> Printf without bound,
	// ending in an uncatchable stack overflow. While a diagnostic line is
	// being written, nested (and concurrent) diagnostics for the same
	// client are suppressed.
	busy *atomic.Int32
}

func (d diag) logf(format string, v ...interface{}) {
	if !d.debug {
		return
	}
	l := d.logger
	if l == nil {
		l = defaultDiagLogger
	}

	if d.busy != nil {
		if !d.busy.CompareAndSwap(0, 1) {
			return
		}
		defer d.busy.Store(0)
	}

	// A diagnostic callback must never be able to terminate the
	// application or the SDK worker. Do not recursively attempt to log
	// this panic.
	defer func() { _ = recover() }()
	l.Printf(format, v...)
}
