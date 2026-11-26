//go:build !windows

package bt

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
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
	if err == nil || !strings.Contains(err.Error(), "unavailable") ||
		!errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("err = %v, want 'tracer unavailable' wrapping ErrUnsupportedPlatform", err)
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

// recordingTracer records the argv of the last Finalize call. Like BTTracer,
// it adds to its base options when an Add method gets a nil slice.
type recordingTracer struct {
	mu      sync.Mutex
	base    []string
	argv    []string
	dto     *TraceOptions
	makeCmd func() *exec.Cmd
	put     func(snapshot []byte) error
	logf    func(msg string)
	sigs    []os.Signal
	sigchan chan os.Signal
}

func (r *recordingTracer) AddOptions(options []string, v ...string) []string {
	if options != nil {
		return append(options, v...)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.base = append(r.base, v...)
	return nil
}

func (r *recordingTracer) AddKV(options []string, key, val string) []string {
	return r.AddOptions(options, "--kv", key+":"+val)
}

func (r *recordingTracer) AddThreadFilter(options []string, tid int) []string {
	return r.AddOptions(options, "--thread", strconv.Itoa(tid))
}

func (r *recordingTracer) AddFaultedThread(options []string, tid int) []string {
	return r.AddOptions(options, "--fault-thread", strconv.Itoa(tid))
}

func (r *recordingTracer) AddCallerGo(options []string, goid int) []string {
	return r.AddOptions(options, "--caller-go", strconv.Itoa(goid))
}

func (r *recordingTracer) AddClassifier(options []string, classifier string) []string {
	return r.AddOptions(options, "--classifier", classifier)
}

func (r *recordingTracer) Options() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.base...)
}

func (r *recordingTracer) ClearOptions() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.base = nil
}

func (r *recordingTracer) DefaultTraceOptions() *TraceOptions {
	if r.dto == nil {
		return nil
	}
	opts := *r.dto
	return &opts
}

func (r *recordingTracer) Finalize(options []string) *exec.Cmd {
	r.mu.Lock()
	r.argv = append([]string(nil), options...)
	r.mu.Unlock()
	if r.makeCmd != nil {
		return r.makeCmd()
	}
	return exec.Command("true")
}

func (r *recordingTracer) lastArgv() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.argv
}

func (r *recordingTracer) Logf(level LogPriority, format string, v ...any) {
	if r.logf != nil {
		r.logf(fmt.Sprintf(format, v...))
	}
}

func (r *recordingTracer) SetLogLevel(level LogPriority) {}
func (r *recordingTracer) String() string                { return "recordingTracer" }
func (r *recordingTracer) PutOnTrace() bool              { return r.put != nil }
func (r *recordingTracer) Put(snapshot []byte) error     { return r.put(snapshot) }
func (r *recordingTracer) SetSigset(sigs ...os.Signal)   { r.sigs = sigs }
func (r *recordingTracer) Sigset() []os.Signal           { return r.sigs }
func (r *recordingTracer) SetSigchan(sc chan os.Signal)  { r.sigchan = sc }
func (r *recordingTracer) Sigchan() chan os.Signal       { return r.sigchan }

func optionValues(argv []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			out = append(out, argv[i+1])
		}
	}
	return out
}

func waitTraceLockFree(t *testing.T) {
	t.Helper()
	select {
	case <-traceLock:
		traceLock <- struct{}{}
	case <-time.After(10 * time.Second):
		t.Fatal("trace lock still held after 10s")
	}
}

func TestTraceWaitersDoNotPinThreads(t *testing.T) {
	defer traceTestConfig()()
	waitTraceLockFree(t)

	<-traceLock
	defer func() { traceLock <- struct{}{} }()

	tr := &recordingTracer{}
	const waiters = 200
	before := pprof.Lookup("threadcreate").Count()

	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			errs <- Trace(tr, errors.New("storm"), &TraceOptions{
				Faulted: true, CallerOnly: true, Timeout: time.Second,
			})
		}()
	}
	for i := 0; i < waiters; i++ {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "lock acquisition timed out") {
			t.Fatalf("waiter %d: err = %v, want lock acquisition timeout", i, err)
		}
	}
	if created := pprof.Lookup("threadcreate").Count() - before; created > waiters/4 {
		t.Errorf("%d goroutines waiting for the trace lock created %d OS threads", waiters, created)
	}
}

func TestTraceDefaultTimeout(t *testing.T) {
	defer traceTestConfig()()
	waitTraceLockFree(t)

	cases := []struct {
		name     string
		defaults *TraceOptions
		opts     *TraceOptions
		want     string
	}{
		{"zero default, nil options", &TraceOptions{}, nil, "2m0s"},
		{"zero default, zero per call", &TraceOptions{}, &TraceOptions{}, "2m0s"},
		{"negative default, nil options", &TraceOptions{Timeout: -1}, nil, ""},
		{"negative default, zero per call", &TraceOptions{Timeout: -1}, &TraceOptions{}, ""},
		{"positive default, zero per call", &TraceOptions{Timeout: time.Minute}, &TraceOptions{}, "1m0s"},
		{"negative per call", &TraceOptions{Timeout: time.Minute}, &TraceOptions{Timeout: -1}, ""},
		{"positive per call", &TraceOptions{}, &TraceOptions{Timeout: 5 * time.Second}, "5s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ""
			tr := &recordingTracer{dto: c.defaults, logf: func(msg string) {
				if d, ok := strings.CutPrefix(msg, "Tracer timeout: "); ok {
					got = strings.TrimSpace(d)
				}
			}}
			if err := Trace(tr, nil, c.opts); err != nil {
				t.Fatalf("Trace = %v", err)
			}
			if got != c.want {
				t.Errorf("timeout = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTraceNegativeDefaultTimeoutWaitsForLock(t *testing.T) {
	defer traceTestConfig()()
	waitTraceLockFree(t)
	tr := &recordingTracer{dto: &TraceOptions{Timeout: -1}}

	<-traceLock
	result := make(chan error, 1)
	go func() { result <- Trace(tr, nil, &TraceOptions{Faulted: true}) }()
	select {
	case err := <-result:
		traceLock <- struct{}{}
		t.Fatalf("Trace returned %v while another trace held the lock", err)
	case <-time.After(150 * time.Millisecond):
	}
	traceLock <- struct{}{}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Trace = %v, want nil after the lock was released", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Trace did not finish after the lock was released")
	}
}

func TestTraceReleasesLockBeforeUpload(t *testing.T) {
	defer traceTestConfig()()
	waitTraceLockFree(t)

	inPut := make(chan struct{})
	releasePut := make(chan struct{})
	var puts atomic.Int32
	tr := &recordingTracer{put: func([]byte) error {
		if puts.Add(1) == 1 {
			close(inPut)
			<-releasePut
		}
		return nil
	}}

	first := make(chan error, 1)
	go func() { first <- Trace(tr, nil, &TraceOptions{Timeout: 5 * time.Second}) }()
	select {
	case <-inPut:
	case <-time.After(10 * time.Second):
		t.Fatal("first trace never reached its upload")
	}

	err := Trace(tr, nil, &TraceOptions{Timeout: 2 * time.Second})
	close(releasePut)
	if err != nil {
		t.Fatalf("Trace during another trace's upload = %v, want nil", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("first Trace = %v", err)
	}
	if got := puts.Load(); got != 2 {
		t.Errorf("Put calls = %d, want 2", got)
	}
}

func TestTraceRateLimitOnlyAfterTracerRan(t *testing.T) {
	defer traceTestConfig()()
	UpdateConfig(GlobalConfig{RateLimit: 500 * time.Millisecond, SynchronousPut: true})
	waitTraceLockFree(t)
	t.Cleanup(func() { waitTraceLockFree(t) })

	noProcess := map[string]func() *exec.Cmd{
		"nil Finalize":  func() *exec.Cmd { return nil },
		"start failure": func() *exec.Cmd { return exec.Command("/nonexistent/tracer-binary") },
	}
	for name, makeCmd := range noProcess {
		t.Run(name, func(t *testing.T) {
			tr := &recordingTracer{makeCmd: makeCmd}
			if err := Trace(tr, nil, &TraceOptions{Timeout: time.Second}); err == nil {
				t.Fatal("Trace = nil, want an error")
			}
			start := time.Now()
			err := Trace(tr, nil, &TraceOptions{Timeout: 250 * time.Millisecond})
			if err != nil && strings.Contains(err.Error(), "lock acquisition") {
				t.Fatalf("second Trace = %v after %v: rate limit applied without a tracer process",
					err, time.Since(start))
			}
		})
	}

	t.Run("tracer ran", func(t *testing.T) {
		tr := &recordingTracer{}
		if err := Trace(tr, nil, &TraceOptions{Timeout: time.Second}); err != nil {
			t.Fatalf("Trace = %v", err)
		}
		err := Trace(tr, nil, &TraceOptions{Timeout: 100 * time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "lock acquisition timed out") {
			t.Fatalf("Trace within the rate limit = %v, want lock acquisition timeout", err)
		}
		if err := Trace(tr, nil, &TraceOptions{Timeout: 5 * time.Second}); err != nil {
			t.Fatalf("Trace after the rate limit = %v", err)
		}
	})
}

func TestTraceArgsAreBounded(t *testing.T) {
	defer traceTestConfig()()
	waitTraceLockFree(t)

	tr := &recordingTracer{}
	text := "x\x00y" + strings.Repeat("a", maxTraceArgBytes-len("error:")) + "é" + strings.Repeat("b", 8000)
	err := Trace(tr, errors.New(text), &TraceOptions{
		Classifications: []string{strings.Repeat("c", maxTraceArgBytes-1) + "é", "", "\x00", "ok"},
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Trace = %v", err)
	}
	argv := tr.lastArgv()

	kv := optionValues(argv, "--kv")
	if len(kv) != 1 {
		t.Fatalf("kv = %d values, want 1", len(kv))
	}
	value := strings.TrimPrefix(kv[0], "error:")
	if len(value) > maxTraceArgBytes || !utf8.ValidString(value) || strings.ContainsRune(value, 0) ||
		!strings.HasPrefix(value, "xyaaa") {
		t.Errorf("error value: %d bytes, valid UTF-8 %v, prefix %q", len(value), utf8.ValidString(value), value[:5])
	}

	classifiers := optionValues(argv, "--classifier")
	if len(classifiers) != 2 || classifiers[0] != strings.Repeat("c", maxTraceArgBytes-1) || classifiers[1] != "ok" {
		t.Errorf("classifiers = %d values (%d bytes first), want the capped one and \"ok\"",
			len(classifiers), len(classifiers[0]))
	}
}

func TestTraceKeepsEmptyBaseOptions(t *testing.T) {
	defer traceTestConfig()()
	waitTraceLockFree(t)

	tr := &recordingTracer{}
	for _, msg := range []string{"first", "second"} {
		if err := Trace(tr, errors.New(msg), &TraceOptions{
			ErrClassification: true, Classifications: []string{msg}, Timeout: 5 * time.Second,
		}); err != nil {
			t.Fatalf("Trace(%s) = %v", msg, err)
		}
	}

	if base := tr.Options(); len(base) != 0 {
		t.Errorf("base options = %q, want none", base)
	}
	argv := tr.lastArgv()
	if got := optionValues(argv, "--kv"); !slices.Equal(got, []string{"error:second"}) {
		t.Errorf("kv = %q, want only the second error", got)
	}
	if got := optionValues(argv, "--classifier"); !slices.Equal(got, []string{"*errors.errorString", "second"}) {
		t.Errorf("classifiers = %q", got)
	}
	if got := optionValues(argv, "--caller-go"); len(got) != 1 {
		t.Errorf("caller goroutine options = %q, want one", got)
	}
}

func TestTraceKillHoldsLockUntilReaped(t *testing.T) {
	defer traceTestConfig()()
	waitTraceLockFree(t)
	t.Cleanup(func() { waitTraceLockFree(t) })

	holdOpen, unblock, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer holdOpen.Close()
	t.Cleanup(func() { unblock.Close() })

	tr := &recordingTracer{makeCmd: func() *exec.Cmd {
		cmd := exec.Command("sh", "-c", "cat <&3 & exec sleep 60")
		cmd.ExtraFiles = []*os.File{holdOpen}
		return cmd
	}}
	err = Trace(tr, nil, &TraceOptions{Timeout: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "execution timed out") {
		t.Fatalf("Trace = %v, want an execution timeout", err)
	}

	tr.makeCmd = nil
	err = Trace(tr, nil, &TraceOptions{Timeout: 300 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "lock acquisition timed out") {
		t.Fatalf("Trace while the killed tracer's output is open = %v, want lock acquisition timeout", err)
	}
	unblock.Close()
	if err := Trace(tr, nil, &TraceOptions{Timeout: 10 * time.Second}); err != nil {
		t.Fatalf("Trace after the killed tracer was reaped = %v", err)
	}
}

var (
	_ int         = LogDebug | LogWarning | LogError | LogMax
	_ LogPriority = LogDebug | LogWarning | LogError | LogMax
)

type panickingLogTracer struct {
	recordingTracer
}

func (p *panickingLogTracer) Logf(LogPriority, string, ...any) {
	panic("Logf exploded")
}

func TestTraceContainsPanickingLogf(t *testing.T) {
	defer traceTestConfig()()
	waitTraceLockFree(t)

	tr := &panickingLogTracer{recordingTracer{makeCmd: func() *exec.Cmd { return nil }}}
	opts := &TraceOptions{Faulted: true, CallerOnly: true, Timeout: 5 * time.Second}
	if err := Trace(tr, nil, opts); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("Trace with nil Finalize = %v, want ErrUnsupportedPlatform", err)
	}
}

func TestTraceLateStartFailureSkipsRateLimit(t *testing.T) {
	defer traceTestConfig()()
	UpdateConfig(GlobalConfig{RateLimit: 2 * time.Second, SynchronousPut: true})
	waitTraceLockFree(t)
	t.Cleanup(func() { waitTraceLockFree(t) })

	release := make(chan struct{})
	testHookBeforeTracerStart = func() { <-release }
	defer func() { testHookBeforeTracerStart = nil }()

	tr := &recordingTracer{makeCmd: func() *exec.Cmd { return exec.Command("/nonexistent/tracer-binary") }}
	err := Trace(tr, nil, &TraceOptions{Timeout: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "start timed out") {
		t.Fatalf("Trace = %v, want start-timeout error", err)
	}
	close(release)

	err = Trace(tr, nil, &TraceOptions{Timeout: time.Second})
	if err == nil || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Trace after the late start failure = %v, want the start error without waiting out the rate limit", err)
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
// carrying the kill error itself, not an ErrTracerCallback error.
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
