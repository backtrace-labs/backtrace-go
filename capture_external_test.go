package bt_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
	"github.com/backtrace-labs/backtrace-go/btslog"
)

const testPackage = "github.com/backtrace-labs/backtrace-go_test"

type wireFrame struct {
	FuncName string `json:"funcName"`
	Library  string `json:"library"`
	Line     string `json:"line"`
	Faulted  bool   `json:"faulted"`
}

type wireThread struct {
	Name  string      `json:"name"`
	Fault bool        `json:"fault"`
	Stack []wireFrame `json:"stack"`
}

type wirePayload struct {
	MainThread string                `json:"mainThread"`
	Threads    map[string]wireThread `json:"threads"`
}

func newCaptureClient(t *testing.T, captureAll bool) (*bt.Client, func() wirePayload) {
	t.Helper()
	var (
		mu       sync.Mutex
		payloads []wirePayload
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("report body is not gzip: %v", err)
			return
		}
		body, _ := io.ReadAll(zr)
		var p wirePayload
		if json.Unmarshal(body, &p) == nil {
			mu.Lock()
			payloads = append(payloads, p)
			mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	c, err := bt.NewClient(bt.Config{
		Endpoint:             srv.URL,
		Token:                "capture-test-token",
		CaptureAllGoroutines: captureAll,
		SourceCode:           bt.SourceCodeNone,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)
	return c, func() wirePayload {
		t.Helper()
		if !c.Flush(5 * time.Second) {
			t.Fatal("Flush timed out")
		}
		mu.Lock()
		defer mu.Unlock()
		if len(payloads) != 1 {
			t.Fatalf("got %d reports, want 1 (stats %+v)", len(payloads), c.Stats())
		}
		p := payloads[0]
		payloads = nil
		return p
	}
}

func isSDKLibrary(lib string) bool {
	const sdk = "github.com/backtrace-labs/backtrace-go"
	return lib == sdk || strings.HasPrefix(lib, sdk+".") || strings.HasPrefix(lib, sdk+"/")
}

//go:noinline
func reportFromHelper(c *bt.Client, err error) (line int) {
	_, _, line, _ = runtime.Caller(0)
	c.ReportError(err, nil)
	return line + 1
}

func TestCallerGoroutineFrames(t *testing.T) {
	c, next := newCaptureClient(t, false)
	line := reportFromHelper(c, errors.New("caller"))
	p := next()

	if p.MainThread != "0" || len(p.Threads) != 1 {
		t.Fatalf("mainThread = %q, threads = %d, want only thread 0", p.MainThread, len(p.Threads))
	}
	thread := p.Threads["0"]
	if !regexp.MustCompile(`^goroutine \d+ \[running`).MatchString(thread.Name) || !thread.Fault {
		t.Errorf("thread = %q fault=%v, want a faulting goroutine header", thread.Name, thread.Fault)
	}
	if len(thread.Stack) < 2 {
		t.Fatalf("stack = %+v", thread.Stack)
	}
	want := wireFrame{FuncName: "reportFromHelper", Library: testPackage, Line: strconv.Itoa(line)}
	if thread.Stack[0] != want {
		t.Errorf("innermost frame = %+v, want %+v", thread.Stack[0], want)
	}
	if got := thread.Stack[1]; got.FuncName != "TestCallerGoroutineFrames" || got.Library != testPackage {
		t.Errorf("second frame = %+v", got)
	}
	for _, f := range thread.Stack {
		if isSDKLibrary(f.Library) || f.Library == "runtime" || f.Faulted {
			t.Errorf("unexpected frame %+v", f)
		}
	}
}

//go:noinline
func panicSite() { panic("boom") }

var noInts []int

//go:noinline
func indexSite() { noInts[len(noInts)]++ }

//go:noinline
func recoverAndReport(c *bt.Client, site func()) {
	defer func() { c.ReportPanicValue(recover(), nil) }()
	site()
}

func TestPanicReportMarksPanickingFrame(t *testing.T) {
	sites := map[string]func(){"panicSite": panicSite, "indexSite": indexSite}
	for _, captureAll := range []bool{false, true} {
		for name, site := range sites {
			c, next := newCaptureClient(t, captureAll)
			recoverAndReport(c, site)
			stack := next().Threads["0"].Stack

			faulted := -1
			for i, f := range stack {
				if f.Faulted {
					if faulted >= 0 {
						t.Errorf("captureAll=%v %s: more than one faulted frame: %+v", captureAll, name, stack)
					}
					faulted = i
				}
			}
			if faulted < 2 || stack[faulted].FuncName != name || stack[faulted].Library != testPackage {
				t.Fatalf("captureAll=%v %s: faulted frame %d in %+v", captureAll, name, faulted, stack)
			}
			if panicFrame := stack[faulted-1]; panicFrame.FuncName != "panic" || panicFrame.Library != "runtime" {
				t.Errorf("captureAll=%v %s: frame inner to the faulted one = %+v, want runtime panic", captureAll, name, panicFrame)
			}
			if deferred := stack[0]; deferred.FuncName != "func1" || deferred.Library != testPackage+".recoverAndReport" {
				t.Errorf("captureAll=%v %s: innermost frame = %+v, want the deferred recovery", captureAll, name, deferred)
			}
		}
	}
}

func TestCallerFramesMatchTextParser(t *testing.T) {
	callers, nextCallers := newCaptureClient(t, false)
	text, nextText := newCaptureClient(t, true)
	reports := []struct {
		name    string
		report  func(*bt.Client)
		creator wireFrame
	}{
		{"error", func(c *bt.Client) { reportFromHelper(c, errors.New("parity")) },
			wireFrame{FuncName: "Run", Library: "testing.(*T)"}},
		{"panic", func(c *bt.Client) { recoverAndReport(c, indexSite) },
			wireFrame{FuncName: "Run", Library: "testing.(*T)"}},
		{"spawned goroutine", spawnAndReport,
			wireFrame{FuncName: "spawnAndReport", Library: testPackage}},
	}
	for _, r := range reports {
		for _, c := range []*bt.Client{callers, text} {
			r.report(c)
		}
		fromCallers := nextCallers().Threads["0"].Stack
		fromText := nextText().Threads["0"].Stack
		if len(fromText) != len(fromCallers) || len(fromText) == 0 {
			t.Fatalf("%s: text frames %+v, callers frames %+v", r.name, fromText, fromCallers)
		}
		for i := range fromCallers {
			if fromCallers[i] != fromText[i] {
				t.Errorf("%s: frame %d: callers %+v, text %+v", r.name, i, fromCallers[i], fromText[i])
			}
		}
		if last := fromCallers[len(fromCallers)-1]; last.FuncName != r.creator.FuncName || last.Library != r.creator.Library {
			t.Errorf("%s: outermost frame = %+v, want the created-by frame %+v", r.name, last, r.creator)
		}
	}
}

//go:noinline
func spawnAndReport(c *bt.Client) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		reportFromHelper(c, errors.New("spawned"))
	}()
	<-done
}

type pkgFrame uintptr

type pkgStackTrace []pkgFrame

type pkgStyleError struct {
	msg   string
	pcs   []uintptr
	cause error
}

func (e *pkgStyleError) Error() string { return e.msg }
func (e *pkgStyleError) Unwrap() error { return e.cause }

func (e *pkgStyleError) StackTrace() pkgStackTrace {
	trace := make(pkgStackTrace, len(e.pcs))
	for i, pc := range e.pcs {
		trace[i] = pkgFrame(pc)
	}
	return trace
}

//go:noinline
func newPkgStyleError(msg string, cause error) error {
	pcs := make([]uintptr, 32)
	return &pkgStyleError{msg: msg, pcs: pcs[:runtime.Callers(2, pcs)], cause: cause}
}

type callersError struct{ pcs []uintptr }

func (e *callersError) Error() string      { return "callers" }
func (e *callersError) Callers() []uintptr { return e.pcs }

type stringTraceError struct{}

func (stringTraceError) Error() string        { return "string trace" }
func (stringTraceError) StackTrace() []string { return []string{"main.main"} }
func (stringTraceError) Callers() []int       { return []int{1} }

//go:noinline
func originSite() error { return newPkgStyleError("root", nil) }

//go:noinline
func wrapSite(err error) error { return newPkgStyleError("wrapped", err) }

//go:noinline
func callersSite() error {
	pcs := make([]uintptr, 32)
	return &callersError{pcs: pcs[:runtime.Callers(1, pcs)]}
}

func TestStackCarryingErrorBecomesMainThread(t *testing.T) {
	cases := []struct {
		name   string
		err    func() error
		origin string
	}{
		{"pkg/errors style, deepest wins", func() error { return fmt.Errorf("ctx: %w", wrapSite(originSite())) }, "originSite"},
		{"Callers method", func() error { return fmt.Errorf("ctx: %w", callersSite()) }, "callersSite"},
	}
	for _, captureAll := range []bool{false, true} {
		for _, tc := range cases {
			c, next := newCaptureClient(t, captureAll)
			reportFromHelper(c, tc.err())
			p := next()

			if p.MainThread != "origin" {
				t.Fatalf("captureAll=%v %s: mainThread = %q", captureAll, tc.name, p.MainThread)
			}
			origin := p.Threads["origin"]
			if origin.Name != "error origin" || !origin.Fault || len(origin.Stack) == 0 ||
				origin.Stack[0].FuncName != tc.origin || origin.Stack[0].Library != testPackage {
				t.Errorf("captureAll=%v %s: origin thread = %+v", captureAll, tc.name, origin)
			}
			caller := p.Threads["0"]
			if caller.Fault || len(caller.Stack) == 0 || caller.Stack[0].FuncName != "reportFromHelper" {
				t.Errorf("captureAll=%v %s: reporting thread = %+v, want it kept and not faulting", captureAll, tc.name, caller)
			}
		}
	}
}

func TestStackCarryingPanicValue(t *testing.T) {
	c, next := newCaptureClient(t, false)
	recoverAndReport(c, func() { panic(originSite()) })
	p := next()
	if p.MainThread != "origin" || p.Threads["origin"].Stack[0].FuncName != "originSite" {
		t.Fatalf("mainThread = %q, origin = %+v", p.MainThread, p.Threads["origin"])
	}
	caller := p.Threads["0"]
	faulted := 0
	for _, f := range caller.Stack {
		if f.Faulted {
			faulted++
		}
	}
	if caller.Fault || faulted != 1 {
		t.Errorf("reporting thread fault=%v with %d faulted frames: %+v", caller.Fault, faulted, caller.Stack)
	}
}

func TestErrorWithoutUsableStackKeepsCallerThread(t *testing.T) {
	c, next := newCaptureClient(t, false)
	reportFromHelper(c, fmt.Errorf("ctx: %w", stringTraceError{}))
	if p := next(); p.MainThread != "0" || len(p.Threads) != 1 {
		t.Errorf("mainThread = %q, threads = %d, want only the reporting goroutine", p.MainThread, len(p.Threads))
	}
}

//go:noinline
func logFromHelper(l *slog.Logger) (line int) {
	_, _, line, _ = runtime.Caller(0)
	l.Error("logged", "err", errors.New("boom"))
	return line + 1
}

type wrappingHandler struct{ slog.Handler }

func (w wrappingHandler) Handle(ctx context.Context, r slog.Record) error {
	return w.Handler.Handle(ctx, r)
}

//go:noinline
func otherFunctionPC() uintptr {
	var pcs [1]uintptr
	runtime.Callers(1, pcs[:])
	return pcs[0]
}

func hasSlogFrame(stack []wireFrame) bool {
	for _, f := range stack {
		if strings.HasPrefix(f.Library, "log/slog") {
			return true
		}
	}
	return false
}

func TestSlogReportStartsAtLoggingCall(t *testing.T) {
	for name, wrap := range map[string]func(slog.Handler) slog.Handler{
		"btslog handler":        func(h slog.Handler) slog.Handler { return h },
		"inside a user handler": func(h slog.Handler) slog.Handler { return wrappingHandler{h} },
	} {
		t.Run(name, func(t *testing.T) {
			c, next := newCaptureClient(t, false)
			line := logFromHelper(slog.New(wrap(btslog.NewHandler(nil, btslog.Options{Client: c}))))
			p := next()
			stack := p.Threads[p.MainThread].Stack
			if len(stack) < 2 {
				t.Fatalf("stack = %+v", stack)
			}
			want := wireFrame{FuncName: "logFromHelper", Library: testPackage, Line: strconv.Itoa(line)}
			if stack[0] != want {
				t.Errorf("innermost frame = %+v, want %+v", stack[0], want)
			}
			if stack[1].Library != testPackage+".TestSlogReportStartsAtLoggingCall" {
				t.Errorf("second frame = %+v, want the subtest function", stack[1])
			}
			if hasSlogFrame(stack) {
				t.Errorf("stack has log/slog frames: %+v", stack)
			}
		})
	}
}

func TestSlogCallSiteOnlyTrimsAMatchingStack(t *testing.T) {
	c, next := newCaptureClient(t, false)
	h := btslog.NewHandler(nil, btslog.Options{Client: c})
	for name, pc := range map[string]uintptr{"no PC": 0, "PC not on the stack": otherFunctionPC()} {
		if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelError, name, pc)); err != nil {
			t.Fatal(err)
		}
		p := next()
		stack := p.Threads[p.MainThread].Stack
		if len(stack) == 0 || stack[0].FuncName != "TestSlogCallSiteOnlyTrimsAMatchingStack" {
			t.Errorf("%s: stack = %+v, want it to start at the calling test", name, stack)
		}
	}
}

func TestSlogCallSiteKeepsFullDumpStacks(t *testing.T) {
	c, next := newCaptureClient(t, true)
	logFromHelper(slog.New(btslog.NewHandler(nil, btslog.Options{Client: c})))
	p := next()
	if stack := p.Threads[p.MainThread].Stack; !hasSlogFrame(stack) {
		t.Errorf("CaptureAllGoroutines stack = %+v, want the log/slog frames kept", stack)
	}
}
