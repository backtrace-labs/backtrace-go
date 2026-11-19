//go:build !windows

package bt

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// hostileTracer panics from exactly one method, selected by panicOn.
type hostileTracer struct {
	panicOn string
	dto     TraceOptions
	nilOpts bool
}

func (h *hostileTracer) boom(method string) {
	if h.panicOn == method {
		panic("hostile tracer: " + method)
	}
}

func (h *hostileTracer) AddOptions(options []string, v ...string) []string {
	h.boom("AddOptions")
	return options
}

func (h *hostileTracer) AddKV(options []string, key, val string) []string {
	h.boom("AddKV")
	return options
}

func (h *hostileTracer) AddThreadFilter(options []string, tid int) []string {
	h.boom("AddThreadFilter")
	return options
}

func (h *hostileTracer) AddFaultedThread(options []string, tid int) []string {
	h.boom("AddFaultedThread")
	return options
}

func (h *hostileTracer) AddCallerGo(options []string, goid int) []string {
	h.boom("AddCallerGo")
	return options
}

func (h *hostileTracer) AddClassifier(options []string, class string) []string {
	h.boom("AddClassifier")
	return options
}

func (h *hostileTracer) Options() []string {
	h.boom("Options")
	return nil
}

func (h *hostileTracer) ClearOptions() { h.boom("ClearOptions") }

func (h *hostileTracer) DefaultTraceOptions() *TraceOptions {
	h.boom("DefaultTraceOptions")
	if h.nilOpts {
		return nil
	}
	return &h.dto
}

func (h *hostileTracer) Finalize(options []string) *exec.Cmd {
	h.boom("Finalize")
	return exec.Command("true")
}

func (h *hostileTracer) Logf(level LogPriority, format string, v ...interface{}) {
	h.boom("Logf")
}

func (h *hostileTracer) SetLogLevel(level LogPriority) { h.boom("SetLogLevel") }

func (h *hostileTracer) String() string {
	h.boom("String")
	return "hostileTracer"
}

func (h *hostileTracer) PutOnTrace() bool {
	h.boom("PutOnTrace")
	return true
}

func (h *hostileTracer) Put(snapshot []byte) error {
	h.boom("Put")
	return nil
}

// panickyError panics from Error(), exercising the safeErrorString path.
type panickyError struct{}

func (panickyError) Error() string { panic("hostile error") }

// TestTraceContainsHostileTracer pins the containment contract: a panic
// from any generic Tracer method that Trace calls becomes an error wrapping
// ErrTracerCallback, never a process crash, and the global trace lock is
// released: a well-behaved Trace right after it still runs.
func TestTraceContainsHostileTracer(t *testing.T) {
	defer traceTestConfig()()

	explicit := &TraceOptions{
		Timeout:           5 * time.Second,
		Faulted:           true,
		CallerOnly:        true,
		ErrClassification: true,
		Classifications:   []string{"c"},
	}
	// The thread options are added only where the thread id is known.
	threadOptions := runtime.GOOS == "linux"

	cases := []struct {
		method  string
		options *TraceOptions
		reached bool
	}{
		// Read only when the caller passes no options.
		{"DefaultTraceOptions", nil, true},
		{"Options", explicit, true},
		{"AddThreadFilter", explicit, threadOptions},
		{"AddFaultedThread", explicit, threadOptions},
		{"AddCallerGo", explicit, true},
		{"AddKV", explicit, true},
		{"AddClassifier", explicit, true},
		{"Finalize", explicit, true},
		{"Logf", explicit, true},
		{"PutOnTrace", explicit, true},
		{"Put", explicit, true},
	}
	healthy := &fakeTracer{
		makeCmd: func() *exec.Cmd { return exec.Command("true") },
		dto:     TraceOptions{Timeout: 5 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			tr := &hostileTracer{
				panicOn: c.method,
				dto:     TraceOptions{Timeout: 5 * time.Second},
			}
			done := make(chan error, 1)
			go func() { done <- Trace(tr, errors.New("boom"), c.options) }()
			select {
			case err := <-done:
				switch {
				case c.reached && !errors.Is(err, ErrTracerCallback):
					t.Errorf("a panic in %s gave %v, want an error wrapping ErrTracerCallback", c.method, err)
				case !c.reached && err != nil:
					t.Errorf("%s is not called on this platform, yet Trace failed: %v", c.method, err)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("Trace hung after a %s panic", c.method)
			}

			// A leaked trace lock does not hang the next Trace, it makes
			// it fail with a lock timeout: so ask for a successful one.
			if err := Trace(healthy, nil, &TraceOptions{Timeout: 5 * time.Second}); err != nil {
				t.Errorf("Trace after a %s panic = %v, want nil (trace lock leaked?)", c.method, err)
			}
		})
	}
}

// TestTraceNilAndTypedNilTracer pins that a missing tracer is an ordinary
// error. ErrTracerCallback is reserved for a callback that panicked.
func TestTraceNilAndTypedNilTracer(t *testing.T) {
	defer traceTestConfig()()

	err := Trace(nil, nil, nil)
	if err == nil {
		t.Error("nil Tracer accepted")
	}
	if errors.Is(err, ErrTracerCallback) {
		t.Errorf("a nil Tracer is reported as a callback panic: %v", err)
	}
	var typedNil *hostileTracer
	err = Trace(typedNil, nil, nil)
	if err == nil {
		t.Error("typed-nil Tracer accepted")
	}
	if errors.Is(err, ErrTracerCallback) {
		t.Errorf("a typed-nil Tracer is reported as a callback panic: %v", err)
	}
}

func TestTraceNilDefaultTraceOptions(t *testing.T) {
	defer traceTestConfig()()

	tr := &hostileTracer{nilOpts: true}
	err := Trace(tr, nil, nil)
	if err == nil {
		t.Error("nil DefaultTraceOptions accepted")
	}
	if errors.Is(err, ErrTracerCallback) {
		t.Errorf("nil DefaultTraceOptions is reported as a callback panic: %v", err)
	}
}

// TestTracePanickingErrorValue pins that e.Error() is called behind
// containment (safeErrorString).
func TestTracePanickingErrorValue(t *testing.T) {
	defer traceTestConfig()()

	tr := &fakeTracer{
		makeCmd: func() *exec.Cmd { return exec.Command("true") },
		dto:     TraceOptions{Timeout: 5 * time.Second},
	}
	if err := Trace(tr, panickyError{}, &TraceOptions{
		Timeout:           5 * time.Second,
		ErrClassification: true,
	}); err != nil {
		t.Errorf("Trace with a panicking error value = %v, want nil", err)
	}
}

// TestRecoverPreservesOriginalPanic pins that a tracer failure never
// replaces (or swallows) the application's panic.
func TestRecoverPreservesOriginalPanic(t *testing.T) {
	defer traceTestConfig()()

	tr := &hostileTracer{panicOn: "Finalize", dto: TraceOptions{Timeout: time.Second}}

	var recovered interface{}
	func() {
		defer func() { recovered = recover() }()
		func() {
			defer Recover(tr, true, &TraceOptions{Timeout: time.Second})
			panic("original application panic")
		}()
	}()

	if recovered != "original application panic" {
		t.Fatalf("recovered = %#v, want the original panic value", recovered)
	}
}

func TestRecoverSwallowsWhenNotRepanicking(t *testing.T) {
	defer traceTestConfig()()

	tr := &hostileTracer{panicOn: "Put", dto: TraceOptions{Timeout: time.Second}}
	func() {
		defer Recover(tr, false, &TraceOptions{Timeout: time.Second})
		panic("swallowed")
	}()
	// Reaching here means the panic was swallowed and no tracer panic
	// escaped.
}
