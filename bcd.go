// Package bt provides integration with out-of-process tracers. Using the
// provided Tracer interface, applications may invoke tracer execution on
// demand. Panic and signal handling integrations are provided.
//
// The Tracer interface is generic and will support any out-of-process tracer
// implementing it. A default Tracer implementation, which uses the
// Backtrace I/O platform, is provided.
package bt

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// Only one tracer should be running on a process at any time; this is
	// global to all created tracers.
	traceLock chan struct{}

	// Configuration applicable to all tracer invocations.
	state globalState
)

type globalState struct {
	c GlobalConfig
	m sync.RWMutex
}

// GlobalConfig holds configuration applicable to all tracer invocations.
// UpdateConfig replaces the ENTIRE struct: populate every field (or start
// from the documented defaults) rather than passing a partial literal.
type GlobalConfig struct {
	// If the tracer's timeout expires and the tracer cannot be killed,
	// generate a run-time panic.
	//
	// Defaults to true.
	PanicOnKillFailure bool

	// Upon receipt of a signal and execution of the tracer, re-sends the
	// signal to the default Go signal handler for the signal and stops
	// listening for the signal.
	// Note: this will call signal.Reset(signal) on the received signal,
	// which undoes the effect of any signal.Notify() calls for the signal.
	//
	// Defaults to true.
	ResendSignal bool

	// Length of time to wait after completion of a tracer's
	// execution before allowing the next tracer to run.
	//
	// Defaults to 3 seconds.
	RateLimit time.Duration

	// If bt.Trace() has been configured to attempt an upload immediately,
	// wait for the Tracer to finish uploading its results (instead of
	// asynchronously uploading in a new goroutine) before returning
	// from bt.Trace().
	//
	// Defaults to true.
	SynchronousPut bool
}

func init() {
	// Tracer execution timeouts are supported, which is why we use a
	// channel here instead of sync.Mutex.
	traceLock = make(chan struct{}, 1)
	traceLock <- struct{}{}

	state = globalState{
		c: GlobalConfig{
			PanicOnKillFailure: true,
			ResendSignal:       true,
			RateLimit:          time.Second * 3,
			SynchronousPut:     true}}
}

// Update global Tracer configuration. Negative durations are treated as
// zero.
func UpdateConfig(c GlobalConfig) {
	if c.RateLimit < 0 {
		c.RateLimit = 0
	}

	state.m.Lock()
	defer state.m.Unlock()

	state.c = c
}

// logfSafe shields lock-critical paths from panicking Log implementations:
// a diagnostic callback must never leak the global trace lock or kill a
// helper goroutine. Note: containment stops panics, not a logger that
// blocks forever — custom loggers are trusted to return promptly.
func logfSafe(l Log, level LogPriority, format string, v ...interface{}) {
	defer func() { _ = recover() }()
	l.Logf(level, format, v...)
}

// ErrTracerCallback wraps a panic raised by application-supplied code on
// the tracer path: a generic Tracer/TracerSig method, or the HTTP client
// configured for snapshot uploads. Such a panic is turned into this error
// rather than being allowed to crash the host. Test with errors.Is.
var ErrTracerCallback = errors.New("bt: tracer callback panicked")

// killFailurePanic carries a deliberate PanicOnKillFailure failure through
// trace's containment recover, so that it is told apart from a callback
// panic and raised by the caller once everything held has been released.
type killFailurePanic struct{ err error }

// killTracer stops a tracer process that outlived its deadline. A variable
// so that tests can exercise the kill-failure paths.
var killTracer = func(p *os.Process) error { return p.Kill() }

// isNilTracer reports whether t is a nil interface or a typed-nil pointer.
func isNilTracer(t Tracer) bool {
	if t == nil {
		return true
	}
	rv := reflect.ValueOf(t)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return rv.IsNil()
	default:
		return false
	}
}

// A generic out-of-process tracer interface.
//
// This is used primarily by the top-level functions of the bt package,
// like bt.Trace, to handle execution and synchronization of various
// generic tracers.
//
// Tracers are not limited to this interface and may provide additional
// utility methods; see specific tracer implementation (e.g. BTTracer)
// documentation for details.
//
// The methods in this interface are expected to be goroutine safe; multiple
// trace requests (which ultimately call into these methods) from different
// goroutines may run concurrently.
type Tracer interface {
	// Store the options provided by v.
	//
	// If the options slice is non-nil, the provided options should be
	// stored in it; otherwise, the options are added to the Tracer's
	// base set of options.
	// Returns the final options slice if the provided options slice is
	// non-nil.
	AddOptions(options []string, v ...string) []string

	// Add a key-value attribute.
	//
	// See AddOptions for rules regarding the specified options slice and
	// the return value.
	AddKV(options []string, key, val string) []string

	// Add a thread filter option using the specified tid. If any thread
	// filter options are added, all non-matching threads and goroutines
	// are expected to be excluded from the generated snapshot.
	//
	// See AddOptions for rules regarding the specified options slice and
	// the return value.
	AddThreadFilter(options []string, tid int) []string

	// Add a faulted thread option using the specified tid. Threads and
	// goroutines matching any faulted thread options are marked as faulted
	// and subject to analysis and grouping.
	//
	// See AddOptions for rules regarding the specified options slice and
	// the return value.
	AddFaultedThread(options []string, tid int) []string

	// Add a caller goroutine option using the specified goid.
	//
	// See AddOptions for rules regarding the specified options slice and
	// the return value.
	AddCallerGo(options []string, goid int) []string

	// Add a classification to the generated snapshot.
	//
	// See AddOptions for rules regarding the specified options slice and
	// the return value.
	AddClassifier(options []string, classifier string) []string

	// Returns a copy of the base set of options for the Tracer.
	Options() []string

	// Clears the base set of options for the Tracer.
	ClearOptions()

	// Returns the default TraceOptions used in bt.Trace() if an override
	// is not specified as an argument to it.
	DefaultTraceOptions() *TraceOptions

	// Accepts a final set of options and returns a Command object
	// representing a tracer that is ready to run. This will be executed
	// on the current process.
	Finalize(options []string) *exec.Cmd

	// Determines when and to what the Tracer will log.
	Log

	// String representation of a Tracer.
	fmt.Stringer

	// Returns whether the Tracer should upload its results to a remote
	// server after successful tracer execution.
	PutOnTrace() bool

	// Uploads Tracer results given by the snapshot argument, which is
	// the stdout of the Tracer process, to the configured remote server.
	//
	// As this is part of the generic Tracer interface, callers know
	// nothing about the contents of the output; thus, it is passed
	// unfiltered to the specific underlying implementation.
	Put(snapshot []byte) error
}

// Options determining actions taken during Tracer execution.
type TraceOptions struct {
	// If true, the calling thread/goroutine will be marked as faulted
	// (i.e. the cause of the error or trace request).
	//
	// This is a Linux-specific option; it results in a noop on other
	// systems.
	Faulted bool

	// If true, only the calling thread/goroutine will be traced; all others
	// will be excluded from the generated snapshot.
	//
	// This is a Linux-specific option; it results in a noop on other
	// systems.
	CallerOnly bool

	// If true and a non-nil error object is passed to bt.Trace(), a
	// classifier will be added based on the specified error's type.
	ErrClassification bool

	// If non-nil, all contained strings will be added as classifiers to
	// the generated snapshot.
	Classifications []string

	// Amount of time to wait for the tracer to finish execution.
	// If 0 is specified, Tracer.DefaultTraceOptions()'s timeout will be
	// used. If <0 is specified, no timeout will be used; the Tracer command
	// will run until it exits.
	Timeout time.Duration

	// If non-nil, any goroutines spawned during the Trace() request will
	// be added to the wait group. This facilitates waiting for things like
	// asynchronous snapshot uploads to complete before exiting the
	// application.
	SpawnedGs *sync.WaitGroup
}

// Log is the logging interface used by Tracers for execution status
// messages.
type Log interface {
	// Logs the specified message if the specified log level is enabled.
	Logf(level LogPriority, format string, v ...interface{})

	// Sets the log level to the specified bitmask of LogPriorities; all
	// priorities excluded from the mask are ignored.
	SetLogLevel(level LogPriority)
}

// LogPriority is a bitmask of tracer log levels.
type LogPriority int

// Tracer log levels; combine with bitwise OR, or use LogMax for everything.
const (
	LogDebug LogPriority = 1 << iota
	LogWarning
	LogError
	LogMax = (1 << iota) - 1
)

// This is a superset of the generic Tracer interface for those that wish
// to support signal handling. The methods unique to this interface are
// not expected to be goroutine-safe.
type TracerSig interface {
	Tracer

	// Sets the desired set of signals for which to invoke the Tracer upon
	// receipt of the signal.
	SetSigset(sigs ...os.Signal)

	// Returns the desired signal set.
	Sigset() []os.Signal

	// Sets the channel through which the Tracer will respond to signals.
	SetSigchan(sc chan os.Signal)

	// Returns the channel through which the Tracer will respond to signals.
	Sigchan() chan os.Signal
}

// Create a unique error to pass to a Trace request.
type signalError struct {
	s os.Signal
}

func (s *signalError) Error() string {
	return s.s.String()
}

// Registers a signal handler to execute the specified Tracer upon receipt of
// any signal in the set specified by TracerSig.Sigset().
// If the GlobalConfiguration value ResendSignal is true, then when a signal is
// received through this handler, all handlers for that signal will be reset
// with signal.Reset(s) after tracer execution completes. The signal will then
// be resent to the default Go handler for that signal.
func Register(t TracerSig) {
	if isNilTracer(t) {
		return
	}
	ss := t.Sigset()
	if ss == nil {
		logfSafe(t, LogError, "Failed to register signal handler: empty "+
			"sigset\n")
		return
	}

	c := t.Sigchan()
	if c != nil {
		unregisterInternal(t, c)
	}

	c = make(chan os.Signal, len(ss))
	t.SetSigchan(c)

	signal.Notify(c, ss...)

	logfSafe(t, LogDebug, "Registered tracer %s (signal set: %v)\n", t, ss)

	go func(t TracerSig) {
		// Raw Logf is unsafe here: a panicking logger would kill this
		// goroutine (and with it, signal handling) — or the process.
		for s := range c {
			logfSafe(t, LogDebug, "Received %v; executing tracer\n", s)

			_ = Trace(t, &signalError{s}, nil)

			// Read the configuration at signal time so that
			// UpdateConfig calls made after Register are honored.
			state.m.RLock()
			rs := state.c.ResendSignal
			state.m.RUnlock()

			if !rs {
				continue
			}

			logfSafe(t, LogDebug, "Resending %v to default handler\n", s)

			// Re-handle the signal with the default Go behavior.
			signal.Reset(s)
			p, err := os.FindProcess(os.Getpid())
			if err != nil {
				logfSafe(t, LogError, "Failed to resend signal: "+
					"cannot find process object")
				return
			}

			_ = p.Signal(s)
		}

		logfSafe(t, LogDebug, "Signal channel closed; exiting goroutine\n")
	}(t)
}

// Stops the specified TracerSig from handling any signals it was previously
// registered to handle via bt.Register().
func Unregister(t TracerSig) {
	c := t.Sigchan()
	if c == nil {
		return
	}

	unregisterInternal(t, c)
}

func unregisterInternal(t TracerSig, c chan os.Signal) {
	logfSafe(t, LogDebug, "Stopping signal channel...\n")
	signal.Stop(c)

	logfSafe(t, LogDebug, "Closing signal channel...\n")
	close(c)

	t.SetSigchan(nil)
	logfSafe(t, LogDebug, "Tracer unregistered\n")
}

type tracerResult struct {
	stdOut []byte
	err    error
}

// testHookBeforeTracerStart, when non-nil, runs in the exec goroutine
// immediately before Cmd.Start. Test-only: it makes the start-timeout
// handoff path deterministic.
var testHookBeforeTracerStart func()

// Executes the specified Tracer on the current process.
//
// If e is non-nil, it will be used to augment the trace according to the
// TraceOptions.
// If traceOptions is non-nil, it will be used instead of the Tracer's
// DefaultTraceOptions(). See TraceOptions for details on the various options.
//
// This is goroutine-safe; multiple goroutines may share the same Tracer and
// execute Trace() concurrently. Only one tracer will be allowed to run at
// any point; others will wait to acquire resources (locks) or timeout (if
// timeouts are not disabled). Trace execution will be rate-limited according
// to the GlobalConfig settings.
//
// This may also be called in a new goroutine via go Trace(...). In that case,
// ensure TraceOptions.CallerOnly is false (you will likely also want to set
// TraceOptions.Faulted to false); otherwise, only the newly spawned goroutine
// will be traced.
//
// Output of specific Tracer execution depends on the implementation; most
// Tracers will have options for specifying output paths.
func Trace(t Tracer, e error, traceOptions *TraceOptions) error {
	killFailure, err := trace(t, e, traceOptions)
	if killFailure != nil {
		// The documented PanicOnKillFailure behavior, raised after trace
		// has released everything it held.
		panic(killFailure)
	}
	return err
}

// trace is Trace without the PanicOnKillFailure panic: a tracer that could
// not be killed is returned as killFailure for the caller to raise. trace
// itself never panics.
func trace(t Tracer, e error, traceOptions *TraceOptions) (killFailure, err error) {
	// Containment boundary: any panic from a generic Tracer callback
	// (DefaultTraceOptions, Options, Add*, Finalize, PutOnTrace, Put) is
	// converted to an error return instead of crashing the host. Locks
	// held at panic time are released by the conditionally-registered
	// defers below (they run before this one).
	defer func() {
		if r := recover(); r != nil {
			if kf, ok := r.(killFailurePanic); ok {
				killFailure, err = kf.err, kf.err
				return
			}
			err = fmt.Errorf("%w (%T)", ErrTracerCallback, r)
		}
	}()

	if isNilTracer(t) {
		return nil, errors.New("bt: nil Tracer")
	}

	if traceOptions == nil {
		def := t.DefaultTraceOptions()
		if def == nil {
			return nil, errors.New("bt: Tracer.DefaultTraceOptions returned nil")
		}
		// Copy so a later mutation of the tracer's defaults cannot race
		// this invocation, and detach the caller-owned slice.
		resolved := *def
		resolved.Classifications = append([]string(nil), def.Classifications...)
		traceOptions = &resolved
	}

	// If no timeouts are specified, the timeout channel will block
	// forever (i.e. it will return only after the tracer exits).
	// We create the timer first to account for the work below, but
	// we won't wrap setup in a timeout as it's unlikely to be
	// a bottleneck.
	var timeout <-chan time.Time

	if traceOptions.Timeout == 0 {
		def := t.DefaultTraceOptions()
		if def == nil {
			return nil, errors.New("bt: Tracer.DefaultTraceOptions returned nil")
		}
		to := def.Timeout
		timeout = time.After(to)
		t.Logf(LogDebug, "Tracer timeout: %v\n", to)
	} else if traceOptions.Timeout > 0 {
		timeout = time.After(traceOptions.Timeout)
		t.Logf(LogDebug, "Tracer timeout: %v\n", traceOptions.Timeout)
	}

	// We create a new options slice to avoid modifying the base
	// set of tracer options just for this particular trace
	// invocation.
	options := t.Options()

	// If the caller has requested a trace with thread-specific options,
	// then add the relevant thread specifications to the options list.
	if traceOptions.CallerOnly || traceOptions.Faulted {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if tid, err := gettid(); err == nil {
			t.Logf(LogDebug, "Retrieved tid: %v\n", tid)

			if traceOptions.CallerOnly {
				options = t.AddThreadFilter(options, tid)
			}

			if traceOptions.Faulted {
				options = t.AddFaultedThread(options, tid)
			}
		} else {
			t.Logf(LogWarning, "Failed to retrieve tid: %v\n", err)
		}
	}

	// Report caller's goid
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(strings.TrimPrefix(string(buf[:n]), "goroutine "))
	if len(fields) > 0 {
		if goid, err := strconv.Atoi(fields[0]); err == nil {
			t.Logf(LogDebug, "Retrieved goid: %v\n", goid)
			options = t.AddCallerGo(options, goid)
		} else {
			t.Logf(LogWarning, "Failed to retrieve goid: %v\n", err)
		}
	} else {
		t.Logf(LogWarning, "Failed to retrieve goid: empty stack header\n")
	}

	if e != nil {
		options = t.AddKV(options, "error", safeErrorString(e))
		if traceOptions.ErrClassification {
			options = t.AddClassifier(options,
				reflect.TypeOf(e).String())
		}
	}

	for _, c := range traceOptions.Classifications {
		options = t.AddClassifier(options, c)
	}

	state.m.RLock()
	kfPanic := state.c.PanicOnKillFailure
	rl := state.c.RateLimit
	synchronousPut := state.c.SynchronousPut
	state.m.RUnlock()

	select {
	case <-timeout:
		err = errors.New("Tracer lock acquisition timed out")
		t.Logf(LogError, "%v\n", err)

		return
	case <-traceLock:
		break
	}

	// We now hold the trace lock.
	// Allow another tracer to execute (i.e. by re-populating the
	// traceLock channel) as long as the current tracer has exited. When a
	// timeout fires before the subprocess start resolves, lock release is
	// handed to a cleanup goroutine instead (see below), so a late child
	// can never run concurrently with the next trace.
	unlockHandedOff := false
	defer func() {
		if !unlockHandedOff {
			go traceUnlockRL(t, rl)
		}
	}()

	done := make(chan tracerResult, 1)
	started := make(chan struct{})
	tracer := t.Finalize(options)
	if tracer == nil {
		// Stub tracers (e.g. on macOS) have no command to run; fail
		// gracefully instead of dereferencing nil in the goroutine.
		err = errors.New("tracer unavailable on this platform")
		t.Logf(LogWarning, "%v\n", err)
		return
	}

	if traceOptions.SpawnedGs != nil {
		traceOptions.SpawnedGs.Add(1)
	}

	go func() {
		if traceOptions.SpawnedGs != nil {
			defer traceOptions.SpawnedGs.Done()
		}

		logfSafe(t, LogDebug, "Starting tracer %v\n", tracer)

		var res tracerResult
		var stdOut bytes.Buffer

		tracer.Stdout = &stdOut

		if testHookBeforeTracerStart != nil {
			testHookBeforeTracerStart()
		}
		if startErr := tracer.Start(); startErr != nil {
			res.err = startErr
			done <- res
			return
		}
		close(started)

		res.err = tracer.Wait()
		res.stdOut = stdOut.Bytes()
		done <- res

		logfSafe(t, LogDebug, "Tracer finished execution\n")
	}()

	t.Logf(LogDebug, "Waiting for tracer completion...\n")

	var res tracerResult

	select {
	case <-timeout:
		// Only kill once the subprocess is known to have started;
		// tracer.Process is nil before that (kill would panic). A
		// result that is already available wins over the timeout.
		select {
		case res = <-done:
			// The tracer failed to start or finished just as the
			// timeout fired; fall through to result handling.
			break
		case <-started:
			// One more non-blocking check: the tracer may have
			// completed successfully between the two signals.
			select {
			case res = <-done:
				break
			default:
				// A process that exited on its own just as
				// the timeout fired is not a kill failure.
				if err = killTracer(tracer.Process); err != nil &&
					!errors.Is(err, os.ErrProcessDone) {
					t.Logf(LogError,
						"Failed to kill tracer upon timeout: %v\n",
						err)

					if kfPanic {
						t.Logf(LogWarning,
							"PanicOnKillFailure set; "+
								"panicking\n")
						// Sentinel-wrapped so the containment
						// recover hands it to the caller, which
						// raises it once the lock is released,
						// instead of reporting a callback panic.
						panic(killFailurePanic{err})
					}
				}

				err = errors.New("Tracer execution timed out")
				t.Logf(LogError, "%v; process killed\n", err)

				return
			}
		default:
			// Cmd.Start itself has not resolved by the deadline.
			// Return to the caller now; a cleanup goroutine deals
			// with the late child and only then releases the trace
			// lock, so no other trace can run alongside it.
			unlockHandedOff = true
			go func() {
				select {
				case <-time.After(30 * time.Second):
					// Cmd.Start itself is wedged (e.g. a
					// pathologically slow exec). Give up and
					// release the lock rather than disabling
					// tracing forever.
					logfSafe(t, LogError,
						"Tracer Start did not resolve; releasing trace lock\n")
				case <-started:
					if killErr := killTracer(tracer.Process); killErr != nil &&
						!errors.Is(killErr, os.ErrProcessDone) {
						logfSafe(t, LogError,
							"Failed to kill late tracer: %v\n", killErr)
						if kfPanic {
							// Honors PanicOnKillFailure; in a
							// goroutine this aborts the process,
							// consistent with the option's intent.
							panic(killErr)
						}
					}
					// Bound the reap wait: an unkillable child must
					// not hold the trace lock forever.
					select {
					case <-done:
					case <-time.After(30 * time.Second):
						logfSafe(t, LogError,
							"Late tracer not reaped after kill; releasing trace lock\n")
					}
				case <-done:
				}
				traceUnlockRL(t, rl)
			}()

			err = errors.New("Tracer start timed out")
			logfSafe(t, LogError, "%v\n", err)

			return
		}
	case res = <-done:
		break
	}

	// Tracer execution has completed by this point.
	if res.err != nil {
		t.Logf(LogError, "Tracer failed to run: %v\n",
			res.err)
		err = res.err

		return
	}

	if !t.PutOnTrace() {
		t.Logf(LogDebug, "Trace request complete\n")

		return
	}

	putFn := func() error {
		logfSafe(t, LogDebug, "Uploading snapshot...")

		if err := t.Put(res.stdOut); err != nil {
			logfSafe(t, LogError, "Failed to upload snapshot: %s",
				err)

			return err
		}

		logfSafe(t, LogDebug, "Successfully uploaded snapshot\n")

		return nil
	}

	if synchronousPut {
		err = putFn()
	} else {
		t.Logf(LogDebug, "Starting asynchronous put...\n")

		if traceOptions.SpawnedGs != nil {
			traceOptions.SpawnedGs.Add(1)
		}

		go func() {
			if traceOptions.SpawnedGs != nil {
				defer traceOptions.SpawnedGs.Done()
			}
			// A panicking Put/Log implementation must not kill the
			// process from this goroutine.
			defer func() { _ = recover() }()

			_ = putFn()
		}()
	}

	t.Logf(LogDebug, "Trace request complete\n")

	return
}

func traceUnlockRL(t Tracer, rl time.Duration) {
	if rl > 0 {
		<-time.After(rl)
	}
	// Release the lock BEFORE logging: a diagnostic callback that panics
	// or blocks must never leave the process-global trace lock consumed.
	traceLock <- struct{}{}
	logfSafe(t, LogDebug, "trace lock released\n")
}

// Create a unique error type to use during panic recovery.
type panicError struct {
	v interface{}
}

func (p *panicError) Error() string {
	return safeSprint(p.v)
}

// Establishes a panic handler that will execute the specified Tracer in
// response. If repanic is true, this will repanic again after Tracer execution
// completes (with the original value returned by recover()).
// This must be used with Go's defer, panic, and recover pattern; see
// https://blog.golang.org/defer-panic-and-recover.
//
// A failure inside the trace (including a panicking Tracer implementation)
// never replaces the original panic: the recovered value is always what is
// re-raised when repanic is true. When repanic is false and the tracer could
// not be killed, the PanicOnKillFailure panic is raised as it is by Trace.
func Recover(t Tracer, repanic bool, options *TraceOptions) {
	recovered := recover()
	if recovered == nil {
		return
	}

	err, ok := recovered.(error)
	if !ok {
		// The runtime type of the error object drives classification;
		// *bt.panicError is more descriptive than *errors.errorString.
		err = &panicError{recovered}
	}
	// trace never panics, so a failing tracer cannot escape here and mask
	// the application's panic.
	killFailure, _ := trace(t, err, options)

	if repanic {
		panic(recovered)
	}
	if killFailure != nil {
		panic(killFailure)
	}
}
