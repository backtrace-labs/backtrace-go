//go:build linux || freebsd

package bt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type pipes struct {
	stdin  io.Reader
	stderr io.Writer
}

const (
	defaultPutTimeout             = 30 * time.Second
	defaultMaxSnapshotBytes int64 = 1 << 30
	defaultTraceTimeout           = 120 * time.Second
)

// uploader is the connection information and options used during Put
// operations.
type uploader struct {
	endpoint string
	options  PutOptions
}

type BTTracer struct {
	// Path to the tracer to invoke.
	cmd string

	// Output directory for generated snapshots.
	outputDir string

	// Generic options to pass to the tracer.
	options []string

	// Prefix for key-value options.
	kvp string

	// Delimeter between key and value for key-value options.
	kvd string

	// Channel which receives signal notifications.
	sigs chan os.Signal

	// The set of signals the tracer will monitor.
	ss []os.Signal

	// The pipes to use for tracer I/O.
	p pipes

	// Protects tracer state modification.
	m sync.RWMutex

	// Protects the logger reference independently of m: Logf is called
	// from paths that hold m (recursively read-locking a sync.RWMutex
	// deadlocks once a writer is queued).
	logMu sync.RWMutex

	// Logs tracer execution status messages.
	logger Log

	// Default trace options to use if none are specified to bt.Trace().
	defaultTraceOptions TraceOptions

	// Protects the uploader configuration: traces are documented
	// goroutine-safe and may upload concurrently with ConfigurePut.
	putMu sync.RWMutex

	// The connection information and options used during Put operations.
	put uploader
}

type defaultLogger struct {
	mu     sync.Mutex
	logger *log.Logger
	level  LogPriority
}

func (d *defaultLogger) Logf(level LogPriority, format string, v ...interface{}) {
	d.mu.Lock()
	enabled := (d.level & level) != 0
	d.mu.Unlock()

	if !enabled {
		return
	}

	d.logger.Printf(format, v...)
}

func (d *defaultLogger) SetLogLevel(level LogPriority) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.level = level
}

type NewOptions struct {
	// If false, system goroutines (i.e. those started and used by the Go
	// runtime) are excluded.
	IncludeSystemGs bool
}

// Returns a new object implementing the bt.Tracer and bt.TracerSig interfaces
// using the Backtrace debugging platform. Currently, only Linux and FreeBSD
// are supported.
//
// Relevant default values:
//
// Tracer path: /opt/backtrace/bin/ptrace.
//
// Output directory: Current working directory of process.
//
// Signal set: ABRT, FPE, SEGV, ILL, BUS. Note: Go converts BUS, FPE, and
// SEGV arising from process execution into run-time panics, which cannot be
// handled by signal handlers. These signals are caught when sent from
// os.Process.Kill or similar.
//
// The default logger prints to stderr.
//
// DefaultTraceOptions:
//
// Faulted: true
//
// CallerOnly: false
//
// ErrClassification: true
//
// Timeout: 120s
func New(options NewOptions) *BTTracer {
	moduleOpt := "--module=go:enable,true"
	if !options.IncludeSystemGs {
		moduleOpt += ",filter,user"
	}

	return &BTTracer{
		cmd: "/opt/backtrace/bin/ptrace",
		kvp: "--kv",
		kvd: ":",
		options: []string{
			"--load=",
			moduleOpt,
			"--faulted",
			strconv.Itoa(os.Getpid())},
		ss: []os.Signal{
			syscall.SIGABRT,
			syscall.SIGFPE,
			syscall.SIGSEGV,
			syscall.SIGILL,
			syscall.SIGBUS},
		logger: &defaultLogger{
			logger: log.New(os.Stderr, "[bt] ", log.LstdFlags),
			level:  LogError},
		defaultTraceOptions: TraceOptions{
			Faulted:           true,
			CallerOnly:        false,
			ErrClassification: true,
			Timeout:           defaultTraceTimeout}}
}

const (
	defaultCoronerScheme = "https"
	defaultCoronerPort   = "6098"
)

// buildPutURL validates the upload endpoint strictly and assembles the
// final URL. Only absolute http(s) URLs without userinfo, fragment, or
// opaque form are accepted; a missing scheme or port receives the coronerd
// defaults (IPv6-safe).
func buildPutURL(endpoint, token string) (string, error) {
	if endpoint == "" {
		return "", errors.New("endpoint must be non-empty")
	}
	if token == "" {
		return "", errors.New("token must be non-empty")
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		// Do not wrap err: *url.Error quotes the raw URL, and net/url
		// inner errors can embed quoted input fragments too. The
		// endpoint should not carry credentials (the token is a
		// separate argument), but redact defensively anyway.
		msg := "unparsable URL"
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			if inner := ue.Err.Error(); !strings.Contains(inner, `"`) {
				msg = inner
			}
		}
		return "", fmt.Errorf("invalid endpoint (%s): %s", msg, redactURL(endpoint))
	}

	// Endpoints without the scheme prefix (or at the very least a '//`
	// prefix) are interpreted as remote server paths. Handle the
	// (unlikely) case of an unspecified scheme — but only for bare
	// hosts: a path component in the shifted host would silently move
	// the upload target.
	if u.Host == "" && u.Scheme == "" && u.Opaque == "" && u.Path != "" {
		host := strings.TrimSuffix(u.Path, "/")
		if strings.Contains(host, "/") {
			return "", errors.New("endpoint must be an absolute HTTP(S) URL " +
				"or a bare host, got a scheme-less path")
		}
		u.Host = host
		u.Path = ""
	}
	if u.Scheme == "" {
		u.Scheme = defaultCoronerScheme
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported endpoint scheme %q", u.Scheme)
	}
	if u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("endpoint must be an absolute HTTP(S) URL " +
			"without userinfo or fragment")
	}

	// Apply the default port IPv6-safely: Hostname() strips any brackets
	// and JoinHostPort restores them as needed. Detect the missing-port
	// case structurally and range-check explicit ports.
	if host, port, portErr := net.SplitHostPort(u.Host); portErr != nil {
		var addrErr *net.AddrError
		if !errors.As(portErr, &addrErr) || !strings.Contains(addrErr.Err, "missing port") {
			return "", fmt.Errorf("invalid endpoint host/port: %w", portErr)
		}
		u.Host = net.JoinHostPort(u.Hostname(), defaultCoronerPort)
	} else {
		if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid endpoint port %q", port)
		}
		_ = host
	}

	u.Path = "post"
	u.RawPath = ""
	u.RawQuery = url.Values{"token": {token}}.Encode()
	return u.String(), nil
}

// ConfigurePut configures the uploading of a generated snapshot file to a
// remote Backtrace coronerd object store.
//
// Uploads use simple one-shot semantics and won't retry on failures. For
// more robust snapshot uploading and directory monitoring, consider using
// the coroner daemon.
//
// endpoint: the URL of the server; a valid HTTP(S) endpoint per url.Parse.
// The default scheme and port are https and 6098, used if left unspecified.
//
// token: the hash associated with the coronerd project to which this
// application belongs.
//
// options: modifies behavior of the Put action; see PutOptions.
func (t *BTTracer) ConfigurePut(endpoint, token string, options PutOptions) error {
	putURL, err := buildPutURL(endpoint, token)
	if err != nil {
		return err
	}
	if options.Timeout < 0 {
		return errors.New("upload timeout must be positive")
	}
	if options.Timeout == 0 {
		options.Timeout = defaultPutTimeout
	}
	if options.MaxSnapshotBytes < 0 {
		return errors.New("snapshot size limit must be positive")
	}
	if options.MaxSnapshotBytes == 0 {
		options.MaxSnapshotBytes = defaultMaxSnapshotBytes
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &options.Client
	}

	t.putMu.Lock()
	t.put = uploader{endpoint: putURL, options: options}
	t.putMu.Unlock()

	// Diagnostics carry the redacted URL only: the query embeds the token.
	t.Logf(LogDebug, "Put enabled (endpoint: %s, unlink: %v)\n",
		redactURL(putURL), options.Unlink)

	return nil
}

// See bt.Tracer.PutOnTrace().
func (t *BTTracer) PutOnTrace() bool {
	t.putMu.RLock()
	defer t.putMu.RUnlock()

	return t.put.options.OnTrace
}

// See bt.Tracer.Put().
func (t *BTTracer) Put(snapshot []byte) error {
	end := bytes.IndexByte(snapshot, 0)
	if end == -1 {
		end = len(snapshot)
	}
	path := strings.TrimSpace(string(snapshot[:end]))

	return t.putSnapshotFile(path)
}

// Synchronously uploads snapshots contained in the specified directory.
// It is safe to spawn a goroutine to run BTTracer.PutDir().
//
// ConfigurePut should have returned successfully before calling
// BTTracer.PutDir().
//
// Only files with the '.btt' suffix will be uploaded.
//
// The first error encountered terminates the directory walk, thus
// skipping snapshots which would have been processed later in the walk.
func (t *BTTracer) PutDir(path string) error {
	t.Logf(LogDebug, "Uploading snapshots from %s...\n", path)
	return filepath.Walk(path, putDirWalk(t))
}

func putDirWalk(t *BTTracer) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		if err != nil {
			t.Logf(LogError, "Failed to walk put directory: %v\n",
				err)
			return err
		}

		if info.IsDir() {
			return nil
		}

		if !strings.HasSuffix(info.Name(), ".btt") {
			t.Logf(LogDebug, "Ignoring file %s: suffix '.btt' "+
				"is required\n", info.Name())
			return nil
		}

		return t.putSnapshotFile(path)
	}
}

func (t *BTTracer) putSnapshotFile(path string) error {
	t.Logf(LogDebug, "Attempting to upload snapshot %s...\n", path)

	t.putMu.RLock()
	u := t.put
	t.putMu.RUnlock()
	if u.endpoint == "" || u.options.HTTPClient == nil {
		return errors.New("snapshot upload is not configured")
	}

	body, err := os.Open(path)
	if err != nil {
		return err
	}
	defer body.Close()

	// Snapshot files must be bounded regular files: FIFOs or devices
	// would block or stream unbounded data.
	info, err := body.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("snapshot is not a regular file")
	}
	if info.Size() > u.options.MaxSnapshotBytes {
		return fmt.Errorf("snapshot exceeds %d-byte limit", u.options.MaxSnapshotBytes)
	}

	ctx, cancel := context.WithTimeout(context.Background(), u.options.Timeout)
	defer cancel()
	// Bound the body at read time too (the file may grow after stat) and
	// declare the length so the request is not chunked-unbounded.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.endpoint,
		io.LimitReader(body, info.Size()))
	if err != nil {
		return fmt.Errorf("building upload request for %s: %s",
			redactURL(u.endpoint), sanitizeHTTPError(err, u.endpoint))
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := u.options.HTTPClient.Do(req)
	if err != nil {
		// Never surface an error retaining the credential-bearing URL.
		return fmt.Errorf("upload to %s failed: %s",
			redactURL(u.endpoint), sanitizeHTTPError(err, u.endpoint))
	}
	defer func() {
		// Drain (bounded) so the keep-alive connection can be reused
		// across PutDir loops.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload to %s failed: %s", redactURL(u.endpoint), resp.Status)
	}

	if u.options.Unlink {
		t.Logf(LogDebug, "Unlinking snapshot...\n")

		if err := os.Remove(path); err != nil {
			t.Logf(LogWarning,
				"Failed to unlink snapshot: %v\n",
				err)

			// This does not mean the put itself failed,
			// so we don't return this error here.
		} else {
			t.Logf(LogDebug, "Unlinked snapshot\n")
		}
	}

	t.Logf(LogDebug, "Uploaded snapshot\n")

	return nil
}

// Sets the executable path for the tracer.
func (t *BTTracer) SetTracerPath(path string) {
	t.m.Lock()
	defer t.m.Unlock()

	t.cmd = path
}

// Sets the output path for generated snapshots. The directory will be
// created with the specified permission bits if it does not already
// exist.
//
// If perm is 0, a default of 0755 will be used.
func (t *BTTracer) SetOutputPath(path string, perm os.FileMode) error {
	if perm == 0 {
		perm = 0755
	}

	if err := os.MkdirAll(path, perm); err != nil {
		t.Logf(LogError, "Failed to create output directory: %v\n", err)
		return err
	}

	t.m.Lock()
	defer t.m.Unlock()

	t.outputDir = path

	return nil
}

// Sets the input and output pipes for the tracer.
// Stdout is not redirected; it is instead passed to the
// tracer's Put command.
func (t *BTTracer) SetPipes(stdin io.Reader, stderr io.Writer) {
	t.m.Lock()
	defer t.m.Unlock()

	t.p.stdin = stdin
	t.p.stderr = stderr
}

// Sets the logger for the tracer.
func (t *BTTracer) SetLogger(logger Log) {
	t.logMu.Lock()
	defer t.logMu.Unlock()

	t.logger = logger
}

// See bt.Tracer.AddOptions().
func (t *BTTracer) AddOptions(options []string, v ...string) []string {
	if options != nil {
		return append(options, v...)
	}

	t.m.Lock()
	defer t.m.Unlock()

	t.options = append(t.options, v...)
	return nil
}

// Append to an option with given prefix
func AppendOptionWithPrefix(options []string, prefix string, v string) []string {
	for i, opt := range options {
		if strings.HasPrefix(opt, prefix) {
			new_opt := opt + "," + v
			options[i] = new_opt
			return options
		}
	}
	return append(options, prefix+v)
}

func (t *BTTracer) AppendOptionWithPrefix(options []string, prefix string, v string) []string {
	if options != nil {
		return AppendOptionWithPrefix(options, prefix, v)
	}

	t.m.Lock()
	defer t.m.Unlock()

	t.options = AppendOptionWithPrefix(t.options, prefix, v)
	return nil
}

// See bt.Tracer.AddKV().
func (t *BTTracer) AddKV(options []string, key, val string) []string {
	return t.AddOptions(options, t.kvp, key+t.kvd+val)
}

// See bt.Tracer.AddThreadFilter().
func (t *BTTracer) AddThreadFilter(options []string, tid int) []string {
	return t.AddOptions(options, "--thread", strconv.Itoa(tid))
}

// See bt.Tracer.AddFaultedThread().
func (t *BTTracer) AddFaultedThread(options []string, tid int) []string {
	return t.AddOptions(options, "--fault-thread", strconv.Itoa(tid))
}

// See bt.Tracer.AddCallerGo().
func (t *BTTracer) AddCallerGo(options []string, goid int) []string {
	moduleOpt := "goid," + strconv.Itoa(goid)
	return t.AppendOptionWithPrefix(options, "--module=go:", moduleOpt)
}

// See bt.Tracer.AddClassifier().
func (t *BTTracer) AddClassifier(options []string, classifier string) []string {
	return t.AddOptions(options, "--classifier", classifier)
}

// See bt.Tracer.Options().
func (t *BTTracer) Options() []string {
	t.m.RLock()
	defer t.m.RUnlock()

	return append([]string(nil), t.options...)
}

// See bt.Tracer.ClearOptions().
func (t *BTTracer) ClearOptions() {
	t.m.Lock()
	defer t.m.Unlock()

	t.options = nil
}

// See bt.Tracer.DefaultTraceOptions(). Returns a pointer to a COPY: mutate
// defaults through SetDefaultTraceOptions, not through the returned value.
func (t *BTTracer) DefaultTraceOptions() *TraceOptions {
	t.m.RLock()
	defer t.m.RUnlock()

	opts := t.defaultTraceOptions
	return &opts
}

// SetDefaultTraceOptions replaces the defaults used by bt.Trace when no
// per-call options are supplied. A zero Timeout keeps the built-in default
// (a zero default would make every Trace time out instantly); use a
// negative Timeout to disable the deadline.
func (t *BTTracer) SetDefaultTraceOptions(opts TraceOptions) {
	if opts.Timeout == 0 {
		opts.Timeout = defaultTraceTimeout
	}

	t.m.Lock()
	defer t.m.Unlock()

	t.defaultTraceOptions = opts
}

// See bt.Tracer.Finalize().
func (t *BTTracer) Finalize(options []string) *exec.Cmd {
	// Snapshot under the lock, then build and log without holding it:
	// Logf must never run while m is held (recursive RLock).
	t.m.RLock()
	cmd := t.cmd
	dir := t.outputDir
	stdin := t.p.stdin
	stderr := t.p.stderr
	t.m.RUnlock()

	tracer := exec.Command(cmd, options...)
	tracer.Dir = dir
	tracer.Stdin = stdin
	tracer.Stderr = stderr

	t.Logf(LogDebug, "Command: %v\n", tracer)

	return tracer
}

func (t *BTTracer) Logf(level LogPriority, format string, v ...interface{}) {
	t.logMu.RLock()
	logger := t.logger
	t.logMu.RUnlock()

	if logger != nil {
		// Called outside any BTTracer lock: format arguments may
		// re-enter the tracer (e.g. %s on the tracer itself). A
		// panicking logger is contained.
		defer func() { _ = recover() }()
		logger.Logf(level, format, v...)
	}
}

func (t *BTTracer) SetLogLevel(level LogPriority) {
	t.logMu.RLock()
	logger := t.logger
	t.logMu.RUnlock()

	if logger != nil {
		logger.SetLogLevel(level)
	}
}

func (t *BTTracer) String() string {
	t.m.RLock()
	defer t.m.RUnlock()

	return fmt.Sprintf("Command: %s, Options: %v", t.cmd, t.options)
}

// See bt.TracerSig.SetSigset().
func (t *BTTracer) SetSigset(sigs ...os.Signal) {
	t.ss = append([]os.Signal(nil), sigs...)
}

// See bt.TracerSig.Sigset().
func (t *BTTracer) Sigset() []os.Signal {
	return append([]os.Signal(nil), t.ss...)
}

// See bt.TracerSig.SetSigchan().
func (t *BTTracer) SetSigchan(sc chan os.Signal) {
	t.sigs = sc
}

// See bt.TracerSig.Sigchan().
func (t *BTTracer) Sigchan() chan os.Signal {
	return t.sigs
}
