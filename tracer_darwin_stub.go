//go:build darwin
// +build darwin

package bt

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

//nolint:all
type pipes struct {
	stdin  io.Reader
	stderr io.Writer
}

//nolint:all
type uploader struct {
	endpoint string
	options  PutOptions
}

type BTTracer struct {
	// Path to the tracer to invoke.
	cmd string

	// Output directory for generated snapshots.
	outputDir string //nolint:all

	// Generic options to pass to the tracer.
	options []string

	// Prefix for key-value options.
	kvp string //nolint:all

	// Delimeter between key and value for key-value options.
	kvd string //nolint:all

	// Channel which receives signal notifications.
	sigs chan os.Signal

	// The set of signals the tracer will monitor.
	ss []os.Signal //nolint:all

	// The pipes to use for tracer I/O.
	p pipes //nolint:all

	// Protects tracer state modification.
	m sync.RWMutex

	// Logs tracer execution status messages.
	logger Log //nolint:all

	// Default trace options to use if none are specified to bt.Trace().
	defaultTraceOptions TraceOptions

	// The connection information and options used during Put operations.
	put uploader
}

//nolint:all
type defaultLogger struct {
	logger *log.Logger
	level  LogPriority
}

//nolint:all
func (d *defaultLogger) Logf(level LogPriority, format string, v ...interface{}) {
}

//nolint:all
func (d *defaultLogger) SetLogLevel(level LogPriority) {
}

type NewOptions struct {
	// If false, system goroutines (i.e. those started and used by the Go
	// runtime) are excluded.
	IncludeSystemGs bool
}

// Returns a new object implementing the bt.Tracer and bt.TracerSig interfaces
// using the Backtrace debugging platform.
func New(options NewOptions) *BTTracer {
	return &BTTracer{}
}

type PutOptions struct {
	// If set to true, tracer results (i.e. generated snapshot files)
	// will be unlinked from the filesystem after successful puts.
	Unlink bool

	// The http.Client to use for uploading. The default will be used
	// if left unspecified.
	Client http.Client

	// If set to true, tracer results will be uploaded after each
	// successful Trace request.
	OnTrace bool
}

func (t *BTTracer) ConfigurePut(endpoint, token string, options PutOptions) error {
	return nil
}

// See bt.Tracer.PutOnTrace().
func (t *BTTracer) PutOnTrace() bool {
	return t.put.options.OnTrace
}

// See bt.Tracer.Put().
func (t *BTTracer) Put(snapshot []byte) error {
	return nil
}

// Synchronously uploads snapshots contained in the specified directory.
func (t *BTTracer) PutDir(path string) error {
	return nil
}

//nolint:all
func putDirWalk(t *BTTracer) filepath.WalkFunc {
	return nil
}

//nolint:all
func (t *BTTracer) putSnapshotFile(path string) error {
	return nil
}

// Sets the executable path for the tracer.
func (t *BTTracer) SetTracerPath(path string) {
}

// Sets the output path for generated snapshots.
func (t *BTTracer) SetOutputPath(path string, perm os.FileMode) error {
	return nil
}

// Sets the input and output pipes for the tracer.
// Stdout is not redirected; it is instead passed to the
// tracer's Put command.
func (t *BTTracer) SetPipes(stdin io.Reader, stderr io.Writer) {
}

// Sets the logger for the tracer.
func (t *BTTracer) SetLogger(logger Log) {
}

// See bt.Tracer.AddOptions().
func (t *BTTracer) AddOptions(options []string, v ...string) []string {
	return nil
}

// See bt.Tracer.AddKV().
func (t *BTTracer) AddKV(options []string, key, val string) []string {
	return options
}

// See bt.Tracer.AddThreadFilter().
func (t *BTTracer) AddThreadFilter(options []string, tid int) []string {
	return options
}

// See bt.Tracer.AddFaultedThread().
func (t *BTTracer) AddFaultedThread(options []string, tid int) []string {
	return options
}

// See bt.Tracer.AddCallerGo().
func (t *BTTracer) AddCallerGo(options []string, goid int) []string {
	return options
}

// See bt.Tracer.AddClassifier().
func (t *BTTracer) AddClassifier(options []string, classifier string) []string {
	return options
}

// See bt.Tracer.Options().
func (t *BTTracer) Options() []string {
	return t.options
}

// See bt.Tracer.ClearOptions().
func (t *BTTracer) ClearOptions() {
}

// See bt.Tracer.DefaultTraceOptions().
func (t *BTTracer) DefaultTraceOptions() *TraceOptions {
	return &t.defaultTraceOptions
}

// See bt.Tracer.Finalize().
func (t *BTTracer) Finalize(options []string) *exec.Cmd {
	return nil
}

func (t *BTTracer) Logf(level LogPriority, format string, v ...interface{}) {
}

func (t *BTTracer) SetLogLevel(level LogPriority) {
}

func (t *BTTracer) String() string {
	t.m.RLock()
	defer t.m.RUnlock()

	return fmt.Sprintf("Command: %s, Options: %v", t.cmd, t.options)
}

// See bt.TracerSig.SetSigset().
func (t *BTTracer) SetSigset(sigs ...os.Signal) {
}

// See bt.TracerSig.Sigset().
func (t *BTTracer) Sigset() []os.Signal {
	return []os.Signal(nil)
}

// See bt.TracerSig.SetSigchan().
func (t *BTTracer) SetSigchan(sc chan os.Signal) {
}

// See bt.TracerSig.Sigchan().
func (t *BTTracer) Sigchan() chan os.Signal {
	return t.sigs
}
