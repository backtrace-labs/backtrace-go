//go:build !windows

package bt

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeTracer implements the Tracer interface with a configurable command,
// letting Trace's timeout/kill/start-failure paths run without a real
// ptrace binary.
type fakeTracer struct {
	makeCmd func() *exec.Cmd
	dto     TraceOptions
}

func (f *fakeTracer) AddOptions(options []string, v ...string) []string {
	if options != nil {
		return append(options, v...)
	}
	return nil
}
func (f *fakeTracer) AddKV(options []string, key, val string) []string        { return options }
func (f *fakeTracer) AddThreadFilter(options []string, tid int) []string      { return options }
func (f *fakeTracer) AddFaultedThread(options []string, tid int) []string     { return options }
func (f *fakeTracer) AddCallerGo(options []string, goid int) []string         { return options }
func (f *fakeTracer) AddClassifier(options []string, class string) []string   { return options }
func (f *fakeTracer) Options() []string                                       { return nil }
func (f *fakeTracer) ClearOptions()                                           {}
func (f *fakeTracer) DefaultTraceOptions() *TraceOptions                      { return &f.dto }
func (f *fakeTracer) Finalize(options []string) *exec.Cmd                     { return f.makeCmd() }
func (f *fakeTracer) Logf(level LogPriority, format string, v ...interface{}) {}
func (f *fakeTracer) SetLogLevel(level LogPriority)                           {}
func (f *fakeTracer) String() string                                          { return "fakeTracer" }
func (f *fakeTracer) PutOnTrace() bool                                        { return false }
func (f *fakeTracer) Put(snapshot []byte) error                               { return nil }

// traceTestConfig makes trace tests fast and panic-free; the returned func
// restores the documented defaults.
func traceTestConfig() func() {
	UpdateConfig(GlobalConfig{
		PanicOnKillFailure: false,
		ResendSignal:       true,
		RateLimit:          time.Millisecond,
		SynchronousPut:     true,
	})
	return func() {
		UpdateConfig(GlobalConfig{
			PanicOnKillFailure: true,
			ResendSignal:       true,
			RateLimit:          time.Second * 3,
			SynchronousPut:     true,
		})
	}
}

func TestTraceTimeoutKillsProcessWithoutPanic(t *testing.T) {
	defer traceTestConfig()()

	tr := &fakeTracer{
		makeCmd: func() *exec.Cmd { return exec.Command("sleep", "60") },
		dto:     TraceOptions{Timeout: 5 * time.Second},
	}

	start := time.Now()
	err := Trace(tr, nil, &TraceOptions{Timeout: 100 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout took %v; kill did not happen promptly", elapsed)
	}
}

func TestTraceStartFailureIsReportedNotPanicked(t *testing.T) {
	defer traceTestConfig()()

	tr := &fakeTracer{
		makeCmd: func() *exec.Cmd { return exec.Command("/nonexistent/tracer-binary") },
		dto:     TraceOptions{Timeout: 5 * time.Second},
	}

	// The historical bug: the timeout path called tracer.Process.Kill()
	// while Process was still nil -> nil-pointer panic. A very short
	// timeout races the start failure on purpose.
	err := Trace(tr, nil, &TraceOptions{Timeout: time.Millisecond})
	if err == nil {
		t.Fatal("expected an error from a tracer that cannot start")
	}
}

// TestTraceNilFinalizeFailsGracefully pins the darwin-stub path: a Tracer
// whose Finalize returns nil (no command to run) must produce an error, not
// a nil-pointer crash in the exec goroutine.
func TestTraceNilFinalizeFailsGracefully(t *testing.T) {
	defer traceTestConfig()()

	tr := &fakeTracer{
		makeCmd: func() *exec.Cmd { return nil },
		dto:     TraceOptions{Timeout: 5 * time.Second},
	}

	err := Trace(tr, nil, &TraceOptions{Timeout: 5 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("err = %v, want 'tracer unavailable' error", err)
	}
	// The trace lock must have been released: a second call must fail the
	// SAME way (a lock-acquisition timeout would prove a leaked lock).
	if err := Trace(tr, nil, &TraceOptions{Timeout: 5 * time.Second}); err == nil ||
		!strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("second Trace = %v, want 'tracer unavailable' (trace lock leaked?)", err)
	}
}

// TestTraceStartTimeoutHandoff pins the handoff path deterministically: the
// timeout fires while Cmd.Start is still blocked, Trace returns a start-
// timeout error, and the cleanup goroutine eventually releases the trace
// lock so later traces still run.
func TestTraceStartTimeoutHandoff(t *testing.T) {
	defer traceTestConfig()()

	release := make(chan struct{})
	testHookBeforeTracerStart = func() { <-release }
	defer func() { testHookBeforeTracerStart = nil }()

	tr := &fakeTracer{
		makeCmd: func() *exec.Cmd { return exec.Command("true") },
		dto:     TraceOptions{Timeout: 5 * time.Second},
	}

	err := Trace(tr, nil, &TraceOptions{Timeout: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "start timed out") {
		t.Fatalf("err = %v, want start-timeout error", err)
	}

	// Unblock the late Start; the cleanup goroutine must release the lock.
	// The hook stays set (clearing it here would race with the blocked
	// goroutine's earlier read); the closed channel makes it a no-op for
	// subsequent traces, and the deferred clear is ordered by the lock
	// handover.
	close(release)

	lockFree := make(chan error, 1)
	go func() {
		lockFree <- Trace(tr, nil, &TraceOptions{Timeout: 5 * time.Second})
	}()
	select {
	case err := <-lockFree:
		if err != nil {
			t.Fatalf("post-handoff Trace = %v, want nil (lock released, tracer runs)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("trace lock leaked by the start-timeout handoff")
	}
}

func TestTraceSuccess(t *testing.T) {
	defer traceTestConfig()()

	tr := &fakeTracer{
		makeCmd: func() *exec.Cmd { return exec.Command("true") },
		dto:     TraceOptions{Timeout: 5 * time.Second},
	}

	if err := Trace(tr, nil, &TraceOptions{Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("Trace = %v, want nil", err)
	}
}

// failingKill makes the tracer kill report a failure (after really killing
// the process, so nothing is left behind) until the test ends.
func failingKill(t *testing.T) error {
	t.Helper()
	failure := errors.New("kill refused (test)")
	original := killTracer
	killTracer = func(p *os.Process) error {
		_ = p.Kill()
		return failure
	}
	t.Cleanup(func() { killTracer = original })
	return failure
}

// panicOnKillFailure switches the documented default back on for a test
// that started from traceTestConfig.
func panicOnKillFailure() {
	UpdateConfig(GlobalConfig{
		PanicOnKillFailure: true,
		ResendSignal:       true,
		RateLimit:          time.Millisecond,
		SynchronousPut:     true,
	})
}

func sleepingTracer() *fakeTracer {
	return &fakeTracer{
		makeCmd: func() *exec.Cmd { return exec.Command("sleep", "60") },
		dto:     TraceOptions{Timeout: 5 * time.Second},
	}
}

func healthyTrace(t *testing.T) {
	t.Helper()
	tr := &fakeTracer{
		makeCmd: func() *exec.Cmd { return exec.Command("true") },
		dto:     TraceOptions{Timeout: 5 * time.Second},
	}
	if err := Trace(tr, nil, &TraceOptions{Timeout: 5 * time.Second}); err != nil {
		t.Errorf("Trace afterwards = %v, want nil (trace lock leaked?)", err)
	}
}

// TestKillFailurePanicsFromTrace pins GlobalConfig.PanicOnKillFailure: a
// tracer that outlives its deadline and cannot be killed raises a panic
// carrying the kill error itself, not an ErrTracerCallback error, and only
// after the trace lock has been given back.
func TestKillFailurePanicsFromTrace(t *testing.T) {
	defer traceTestConfig()()
	panicOnKillFailure()
	failure := failingKill(t)

	recovered := recoveredFrom(func() {
		_ = Trace(sleepingTracer(), nil, &TraceOptions{Timeout: 100 * time.Millisecond})
	})
	if recovered != failure {
		t.Fatalf("Trace panicked with %#v, want the kill error", recovered)
	}
	healthyTrace(t)
}

// TestKillFailureIsAnErrorWhenNotPanicking is the same failure with the
// option off: an error return, no panic.
func TestKillFailureIsAnErrorWhenNotPanicking(t *testing.T) {
	defer traceTestConfig()()
	failingKill(t)

	var err error
	recovered := recoveredFrom(func() {
		err = Trace(sleepingTracer(), nil, &TraceOptions{Timeout: 100 * time.Millisecond})
	})
	if recovered != nil {
		t.Fatalf("Trace panicked with %#v although PanicOnKillFailure is off", recovered)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want the timeout error", err)
	}
	healthyTrace(t)
}

// TestRecoverRaisesKillFailure pins what Recover does with it. With repanic
// the application's own panic is what continues; without, nothing of the
// application's is left to raise, so the PanicOnKillFailure panic is, as
// it is from Trace. Swallowing it would turn the option off silently.
func TestRecoverRaisesKillFailure(t *testing.T) {
	defer traceTestConfig()()
	panicOnKillFailure()
	failure := failingKill(t)
	options := &TraceOptions{Timeout: 100 * time.Millisecond}

	recovered := recoveredFrom(func() {
		defer Recover(sleepingTracer(), false, options)
		panic("application panic")
	})
	if recovered != failure {
		t.Errorf("Recover without repanic raised %#v, want the kill error", recovered)
	}
	healthyTrace(t)

	recovered = recoveredFrom(func() {
		defer Recover(sleepingTracer(), true, options)
		panic("application panic")
	})
	if recovered != "application panic" {
		t.Errorf("Recover with repanic raised %#v, want the application's panic", recovered)
	}
	healthyTrace(t)
}
