//go:build !windows

package bt

import (
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
	// The trace lock must have been released: a second call still works.
	if err := Trace(tr, nil, &TraceOptions{Timeout: 5 * time.Second}); err == nil {
		t.Fatal("second Trace unexpectedly succeeded with nil Finalize")
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
