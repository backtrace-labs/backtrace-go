package bt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func ProcessSession() string { return processSessionID }

const crashTestPackage = "github.com/backtrace-labs/backtrace-go_test"

const repanicCrash = `panic: runtime error: index out of range [5] with length 3 [recovered, repanicked]

goroutine 7 [running]:
example.com/app.handler.func1()
	/src/app/handler.go:12 +0x20
panic({0x1, 0x2})
	/usr/local/go/src/runtime/panic.go:787 +0x132
example.com/app.handler()
	/src/app/handler.go:15 +0x48
created by example.com/app.main in goroutine 1
	/src/app/main.go:20 +0x1c

goroutine 1 [chan receive]:
example.com/app.main()
	/src/app/main.go:22 +0x30
`

func TestParseCrash(t *testing.T) {
	cases := []struct {
		name, text, message, classifier, reportType string
		panics                                      []string
		repanicked                                  bool
		goroutine                                   string
	}{
		{
			name:       "goroutine panic",
			text:       "panic: boom\n\ngoroutine 19 [running]:\nmain.worker()\n\t/x/main.go:9 +0x2c\n",
			message:    "panic: boom",
			classifier: "panic", reportType: "panic", panics: []string{"boom"}, goroutine: "19",
		},
		{
			name:       "repanicked runtime error",
			text:       repanicCrash,
			message:    "panic: runtime error: index out of range [5] with length 3 [recovered, repanicked]",
			classifier: "runtime.Error", reportType: "panic",
			panics:     []string{"runtime error: index out of range [5] with length 3"},
			repanicked: true, goroutine: "7",
		},
		{
			name:       "chained multi-line panics",
			text:       "panic: first [recovered]\n\tpanic: second\n\tline\n\ngoroutine 1 [running]:\n",
			message:    "panic: first [recovered]\n\tpanic: second\n\tline",
			classifier: "panic", reportType: "panic", panics: []string{"first", "second\nline"}, goroutine: "1",
		},
		{
			name:       "chain ending in an unreported panic",
			text:       "panic: original [recovered, repanicked]\n\tpanic: cleanup failed\n\ngoroutine 1 [running]:\n",
			message:    "panic: original [recovered, repanicked]\n\tpanic: cleanup failed",
			classifier: "panic", reportType: "panic", panics: []string{"original", "cleanup failed"}, goroutine: "1",
		},
		{
			name:       "carriage return in a panic value",
			text:       "panic: a\r\n\tb [recovered, repanicked]\n\ngoroutine 1 [running]:\n",
			message:    "panic: a\r\n\tb [recovered, repanicked]",
			classifier: "panic", reportType: "panic", panics: []string{"a\r\nb"}, repanicked: true, goroutine: "1",
		},
		{
			name:       "pre-1.25 repanic",
			text:       "panic: x\n\ty [recovered]\n\tpanic: x\n\ty\n\ngoroutine 1 [running]:\n",
			message:    "panic: x\n\ty [recovered]\n\tpanic: x\n\ty",
			classifier: "panic", reportType: "panic", panics: []string{"x\ny", "x\ny"}, goroutine: "1",
		},
		{
			name:       "nil dereference with signal line",
			text:       "panic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV: segmentation violation code=0x2 addr=0x0 pc=0x1]\n\ngoroutine 1 [running]:\n",
			message:    "panic: runtime error: invalid memory address or nil pointer dereference",
			classifier: "runtime.Error", reportType: "panic",
			panics:    []string{"runtime error: invalid memory address or nil pointer dereference"},
			goroutine: "1",
		},
		{
			name:       "nil map",
			text:       "panic: assignment to entry in nil map\n\ngoroutine 1 [running]:\n",
			message:    "panic: assignment to entry in nil map",
			classifier: "runtime.Error", reportType: "panic", panics: []string{"assignment to entry in nil map"}, goroutine: "1",
		},
		{
			name:       "fatal error as the runtime writes it",
			text:       "\ngoroutine 20 [running]:\ninternal/runtime/maps.fatal({0x1?, 0x0?})\n\t/go/src/runtime/panic.go:1195 +0x20\n",
			message:    "fatal error",
			classifier: "fatal error", reportType: "crash",
		},
		{
			name:       "fatal error with signal line",
			text:       "[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x1]\n\nruntime stack:\nruntime.throw({0x1?, 0x2?})\n\t/go/src/runtime/panic.go:1 +0x1\n",
			message:    "fatal error\n[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x1]",
			classifier: "fatal error", reportType: "crash",
		},
		{
			name:       "signal",
			text:       "SIGSEGV: segmentation violation\nPC=0x1 m=0 sigcode=1 addr=0x0\nsignal arrived during cgo execution\n\ngoroutine 1 [syscall]:\n",
			message:    "SIGSEGV: segmentation violation",
			classifier: "SIGSEGV", reportType: "crash",
		},
		{
			name:       "panic text after the first goroutine is not a header",
			text:       "\ngoroutine 1 [running]:\nmain.f()\n\t/x.go:1 +0x1\npanic: not a header\n",
			message:    "fatal error",
			classifier: "fatal error", reportType: "crash",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := parseCrash([]byte(tc.text))
			if info.message != tc.message || info.classifier != tc.classifier || info.reportType != tc.reportType {
				t.Errorf("got message %q classifier %q report_type %q, want %q %q %q",
					info.message, info.classifier, info.reportType, tc.message, tc.classifier, tc.reportType)
			}
			if !slices.Equal(info.panics, tc.panics) {
				t.Errorf("panics = %q, want %q", info.panics, tc.panics)
			}
			if info.repanicked != tc.repanicked || info.goroutine != tc.goroutine {
				t.Errorf("repanicked %t goroutine %q, want %t %q", info.repanicked, info.goroutine, tc.repanicked, tc.goroutine)
			}
		})
	}
}

func TestPanicClassifier(t *testing.T) {
	want := map[string]string{
		"runtime error: index out of range [1] with length 1":          "runtime.Error",
		"interface conversion: interface {} is nil, not int":           "runtime.Error",
		"value method example.com/app.T.M called using nil *T pointer": "runtime.Error",
		"boom":                        "panic",
		"makechan: something else":    "panic",
		"value method without detail": "panic",
	}
	for text := range runtimePanicTexts {
		want[text] = "runtime.Error"
	}
	for value, classifier := range want {
		if got := panicClassifier(value); got != classifier {
			t.Errorf("panicClassifier(%q) = %q, want %q", value, got, classifier)
		}
	}
}

func TestSignalName(t *testing.T) {
	for line, want := range map[string]string{
		"SIGSEGV: segmentation violation": "SIGSEGV",
		"SIGQUIT: quit":                   "SIGQUIT",
		"SIG: x":                          "",
		"SIGsegv: x":                      "",
		"[signal SIGSEGV: x]":             "",
		"SIGABRT":                         "",
	} {
		if got := signalName(line); got != want {
			t.Errorf("signalName(%q) = %q, want %q", line, got, want)
		}
	}
}

type crashStringer struct{}

func (crashStringer) String() string { return "stringer\nvalue" }

type (
	crashReason string
	crashCode   int
	crashFlag   bool
	crashByte   uint8
	crashRatio  float64
	crashPhase  complex128
)

func TestPanicText(t *testing.T) {
	for _, tc := range []struct {
		value any
		text  string
	}{
		{1.5, "1.5"},
		{float32(0.1), "0.1"},
		{complex(1, -2), "(1-2i)"},
		{crashRatio(1.5), "bt.crashRatio(1.5)"},
		{crashPhase(complex(1, -2)), "bt.crashPhase(1-2i)"},
	} {
		if text, ok := panicText(tc.value); !ok || text != tc.text {
			t.Errorf("panicText(%#v) = %q, %t, want %q", tc.value, text, ok, tc.text)
		}
	}
	for _, v := range []any{struct{ A int }{1}, new(int), []int{1}, map[string]int{}, &testPanicMethodError{}} {
		if text, ok := panicText(v); ok {
			t.Errorf("panicText(%T) = %q, want no mark", v, text)
		}
	}
	if id := currentGoroutineID(); !allDigits(id) {
		t.Errorf("currentGoroutineID = %q", id)
	}
}

var runtimeHeaderValues = map[string]any{
	"string":       "two\nlines",
	"crlf":         "crlf\r\nline",
	"error":        errors.New("error\nvalue"),
	"stringer":     crashStringer{},
	"int":          -7,
	"uintptr":      uintptr(255),
	"bool":         true,
	"named string": crashReason("named\nvalue"),
	"named int":    crashCode(-7),
	"named bool":   crashFlag(true),
	"named uint8":  crashByte(200),
}

func TestPanicTextHelper(t *testing.T) {
	name := os.Getenv("BACKTRACE_GO_TEST_PANIC_VALUE")
	if name == "" {
		t.Skip("started by TestPanicTextMatchesRuntimeHeader")
	}
	go func() {
		defer func() { panic(recover()) }()
		panic(runtimeHeaderValues[name])
	}()
	time.Sleep(time.Minute)
}

func TestPanicTextMatchesRuntimeHeader(t *testing.T) {
	skipWithoutSubprocess(t)
	for name, v := range runtimeHeaderValues {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPanicTextHelper$")
			cmd.Env = append(os.Environ(), "BACKTRACE_GO_TEST_PANIC_VALUE="+name)
			out, _ := cmd.CombinedOutput()
			info := parseCrash(out)
			want, ok := panicText(v)
			if n := len(info.panics); !ok || n == 0 || info.panics[n-1] != want {
				t.Errorf("runtime printed %q, panicText gives %q, %t", info.message, want, ok)
			}
		})
	}
}

func TestBuildThreadsSkipsRuntimeStack(t *testing.T) {
	stack := "goroutine 1 [running]:\n" +
		"main.f()\n" +
		"\t/x.go:1 +0x1\n" +
		"\n" +
		"runtime stack:\n" +
		"runtime.throw({0x1, 0x2})\n" +
		"\t/go/src/runtime/panic.go:1 +0x1\n" +
		"\n" +
		"goroutine 2 [runnable]:\n" +
		"main.g()\n" +
		"\t/x.go:2 +0x1\n"
	threads, _, _ := buildThreads([]byte(stack), noSource())
	if got := threads["0"].Stacks; len(got) != 1 || got[0].FuncName != "f" {
		t.Errorf("system stack frames leaked into thread 0: %+v", got)
	}
	if got := threads["1"].Stacks; len(got) != 1 || got[0].FuncName != "g" {
		t.Errorf("thread after the system stack = %+v", got)
	}
}

func TestMonitorEnv(t *testing.T) {
	env := monitorEnv([]string{
		"PATH=/bin", "Path=C:\\Windows", "HOME=/home/u", "TMPDIR=/tmp", "SystemRoot=C:\\Windows",
		"BACKTRACE_ENDPOINT=https://x", "backtrace_extra=1", "HTTPS_PROXY=http://proxy",
		"no_proxy=localhost", "SSL_CERT_FILE=/ca.pem", "REQUEST_METHOD=GET", "HTTP_PROXY=http://from-request",
		"GOGC=10", "GOMEMLIMIT=1", "GODEBUG=x=1", "AWS_SECRET_ACCESS_KEY=s", "DATABASE_URL=postgres://",
		"BACKTRACE_GO_CRASH_MONITOR=stale", "=C:=C:\\",
		"USERPROFILE=C:\\Users\\u", "Tmp=C:\\Temp", "TEMP=C:\\Temp", "LocalAppData=C:\\L", "APPDATA=C:\\R",
		"USERNAME=u", "TEMPLATE=x",
	}, "3")
	want := []string{
		"BACKTRACE_GO_CRASH_MONITOR=3",
		"PATH=/bin", "Path=C:\\Windows", "HOME=/home/u", "TMPDIR=/tmp", "SystemRoot=C:\\Windows",
		"BACKTRACE_ENDPOINT=https://x", "backtrace_extra=1", "HTTPS_PROXY=http://proxy",
		"no_proxy=localhost", "SSL_CERT_FILE=/ca.pem", "REQUEST_METHOD=GET", "HTTP_PROXY=http://from-request",
		"USERPROFILE=C:\\Users\\u", "Tmp=C:\\Temp", "TEMP=C:\\Temp", "LocalAppData=C:\\L", "APPDATA=C:\\R",
	}
	if !slices.Equal(env, want) {
		t.Errorf("monitorEnv =\n%q\nwant\n%q", env, want)
	}
}

func TestControlLog(t *testing.T) {
	now := time.Now()
	l := &controlLog{hello: monitorHello{PID: 1}}
	l.add(`{"pid":42,"session":"s-1","start":1000,"endpoint":"https://e","token":"t"}`, now)
	l.add("{not json", now)
	if got := l.parent(); got != (monitorHello{PID: 42, Session: "s-1", Start: 1000, Endpoint: "https://e", Token: "t"}) {
		t.Errorf("parent = %+v", got)
	}

	l.add(panicMark("1", "old"), now.Add(-31*time.Second))
	l.add(panicMark("1", "recent"), now.Add(-29*time.Second))
	if l.marked("1", "old", now) {
		t.Error("a mark older than 30s matched")
	}
	if !l.marked("1", "recent", now) {
		t.Error("a mark from the last 30s did not match")
	}
	if l.marked("2", "recent", now) {
		t.Error("a mark from another goroutine matched")
	}
	if l.marked("1", "never", now) {
		t.Error("an unmarked panic matched")
	}

	for i := 0; i < 2*maxMarks; i++ {
		l.add(panicMark("1", fmt.Sprint(i)), now)
	}
	if len(l.marks) != maxMarks {
		t.Errorf("marks = %d, want %d", len(l.marks), maxMarks)
	}
	if !l.marked("1", fmt.Sprint(2*maxMarks-1), now) || l.marked("1", "0", now) {
		t.Error("the newest marks must be kept")
	}

	l = &controlLog{}
	l.read(strings.NewReader(`{"pid":7}` + "\n" + panicMark("1", "p") + "\n"))
	if l.parent().PID != 7 || !l.marked("1", "p", time.Now()) {
		t.Errorf("read: parent %+v marks %v", l.parent(), l.marks)
	}
}

func helloFor(t *testing.T, rs *recordingServer, start time.Time) string {
	t.Helper()
	data, err := json.Marshal(monitorHello{
		PID: 4242, Session: "parent-session", Start: start.UnixMilli(),
		Endpoint: rs.srv.URL, Token: "crash-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func crashConfig() Config {
	return Config{
		Timeout:    5 * time.Second,
		MaxRetries: -1,
		Attributes: map[string]any{"deployment": "blue", "hostname": nil},
	}
}

func TestSuperviseCrashesSendsReport(t *testing.T) {
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	start := time.Now().Add(-90 * time.Second)

	code := superviseCrashes(crashConfig(), strings.NewReader(repanicCrash), strings.NewReader(helloFor(t, rs, start)))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if rs.count() != 1 {
		t.Fatalf("reports = %d, want 1", rs.count())
	}
	p := rs.last()
	attrs := attrsOf(t, p)
	for k, want := range map[string]any{
		"error.type":          "Crash",
		"error.message":       "panic: runtime error: index out of range [5] with length 3 [recovered, repanicked]",
		"report_type":         "panic",
		"process.id":          float64(4242),
		"application.session": "parent-session",
		"deployment":          "blue",
		"uname.sysname":       runtime.GOOS,
	} {
		if attrs[k] != want {
			t.Errorf("attribute %s = %v, want %v", k, attrs[k], want)
		}
	}
	if age, _ := attrs["process.age"].(float64); age < 89 || age > 120 {
		t.Errorf("process.age = %v, want the parent's age (about 90)", attrs["process.age"])
	}
	for _, k := range []string{"hostname", "runtime.goroutines", "memory.heap.alloc", "gc.count", "runtime.gogc"} {
		if v, ok := attrs[k]; ok {
			t.Errorf("attribute %s = %v must not be sent", k, v)
		}
	}
	if got := p["classifiers"]; !reflect.DeepEqual(got, []any{"runtime.Error"}) {
		t.Errorf("classifiers = %v", got)
	}
	if p["mainThread"] != "0" {
		t.Fatalf("mainThread = %v, want 0", p["mainThread"])
	}
	threads := p["threads"].(map[string]any)
	main := threads["0"].(map[string]any)
	if main["name"] != "goroutine 7 [running]" || main["fault"] != true {
		t.Errorf("main thread = %v", main)
	}
	var frames []string
	for _, f := range main["stack"].([]any) {
		frame := f.(map[string]any)
		name := frame["library"].(string) + "." + frame["funcName"].(string)
		if frame["faulted"] == true {
			name += " (faulted)"
		}
		frames = append(frames, name)
	}
	wantFrames := []string{"example.com/app.handler.func1", "runtime.panic", "example.com/app.handler (faulted)", "example.com/app.main"}
	if !slices.Equal(frames, wantFrames) {
		t.Errorf("frames = %q, want %q", frames, wantFrames)
	}
	if other, ok := threads["1"].(map[string]any); !ok || other["fault"] != false {
		t.Errorf("second goroutine = %v", threads["1"])
	}
}

func TestSuperviseCrashesSkipsDeliveredPanic(t *testing.T) {
	const value = "runtime error: index out of range [5] with length 3"
	unrecovered := strings.Replace(repanicCrash, " [recovered, repanicked]", "", 1)
	chained := "panic: original [recovered, repanicked]\n\tpanic: cleanup failed\n\ngoroutine 1 [running]:\nmain.f()\n\t/x.go:1 +0x1\n"
	for _, tc := range []struct {
		name, crash, goroutine, value string
		sent                          bool
	}{
		{name: "delivered repanic", crash: repanicCrash, goroutine: "7", value: value},
		{name: "mark from another goroutine", crash: repanicCrash, goroutine: "8", value: value, sent: true},
		{name: "unrecovered panic with the marked text", crash: unrecovered, goroutine: "7", value: value, sent: true},
		{name: "chain ending in an unmarked panic", crash: chained, goroutine: "1", value: "original", sent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := newRecordingServer()
			t.Cleanup(rs.srv.Close)
			control := helloFor(t, rs, time.Now()) + panicMark(tc.goroutine, tc.value) + "\n"
			if code := superviseCrashes(crashConfig(), strings.NewReader(tc.crash), strings.NewReader(control)); code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			rs.mu.Lock()
			requests := rs.requests
			rs.mu.Unlock()
			want := 0
			if tc.sent {
				want = 1
			}
			if requests != want {
				t.Errorf("requests = %d, want %d", requests, want)
			}
		})
	}
}

func TestSuperviseCrashesCleanExit(t *testing.T) {
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	if code := superviseCrashes(crashConfig(), strings.NewReader(""), strings.NewReader(helloFor(t, rs, time.Now()))); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.requests != 0 {
		t.Errorf("requests = %d, want 0", rs.requests)
	}
}

type countingReader struct {
	r    io.Reader
	read int
	eof  bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	if err == io.EOF {
		c.eof = true
	}
	return n, err
}

func TestSuperviseCrashesDrainsLongOutput(t *testing.T) {
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	cfg := crashConfig()
	cfg.MaxStackBytes = 4096
	filler := strings.Repeat("goroutine 9 [chan receive]:\nexample.com/app.idle()\n\t/src/app/idle.go:1 +0x1\n\n", 2000)
	crash := &countingReader{r: strings.NewReader(repanicCrash + "\n" + filler)}
	if code := superviseCrashes(cfg, crash, strings.NewReader(helloFor(t, rs, time.Now()))); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !crash.eof || crash.read != len(repanicCrash)+1+len(filler) {
		t.Errorf("read %d bytes (eof %t), want all %d", crash.read, crash.eof, len(repanicCrash)+1+len(filler))
	}
	if rs.count() != 1 {
		t.Fatalf("reports = %d, want 1", rs.count())
	}
	if threads := rs.last()["threads"].(map[string]any); len(threads) > 4096/40 {
		t.Errorf("threads = %d, want only those within the first 4096 bytes", len(threads))
	}
}

func TestSuperviseCrashesWithoutEndpoint(t *testing.T) {
	t.Setenv(envEndpoint, "")
	logs := &lineLogger{}
	cfg := crashConfig()
	cfg.Logger = logs
	if code := superviseCrashes(cfg, strings.NewReader(repanicCrash), strings.NewReader("")); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if lines := logs.all(); !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "crash not sent") }) {
		t.Errorf("log = %q, want a crash-not-sent warning", lines)
	}
}

func TestSuperviseCrashesWithLargeMaxRetries(t *testing.T) {
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	cfg := crashConfig()
	cfg.MaxRetries, cfg.Timeout = 1_000_000_000, 30*time.Second
	if code := superviseCrashes(cfg, strings.NewReader(repanicCrash), strings.NewReader(helloFor(t, rs, time.Now()))); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if rs.count() != 1 {
		t.Errorf("reports = %d, want 1", rs.count())
	}
}

func TestCrashDeliveryTimeout(t *testing.T) {
	if got, want := crashDeliveryTimeout(Config{}.normalize()), 3*DefaultTimeout+maxRetryWait+DefaultShutdownTimeout; got != want {
		t.Errorf("default timeout = %v, want %v", got, want)
	}
	capped := 61*30*time.Second + maxRetryWait + time.Second
	for _, tc := range []struct {
		cfg  Config
		want time.Duration
	}{
		{Config{MaxRetries: 1_000_000_000, Timeout: 30 * time.Second, ShutdownTimeout: time.Second}, capped},
		{Config{MaxRetries: math.MaxInt, Timeout: 30 * time.Second, ShutdownTimeout: time.Second}, capped},
		{Config{MaxRetries: 2, Timeout: math.MaxInt64 / 2, ShutdownTimeout: time.Second}, math.MaxInt64},
		{Config{MaxRetries: -1, Timeout: time.Second, ShutdownTimeout: math.MaxInt64}, math.MaxInt64},
	} {
		if got := crashDeliveryTimeout(tc.cfg); got != tc.want {
			t.Errorf("MaxRetries %d, Timeout %v, ShutdownTimeout %v: got %v, want %v",
				tc.cfg.MaxRetries, tc.cfg.Timeout, tc.cfg.ShutdownTimeout, got, tc.want)
		}
	}
}

func TestCheckMonitorHost(t *testing.T) {
	if err := checkMonitorHost("linux", 1); err == nil || !strings.Contains(err.Error(), "PID 1") {
		t.Errorf("linux PID 1: err = %v, want the PID 1 error", err)
	}
	for _, tc := range []struct {
		goos string
		pid  int
	}{{"linux", 2}, {"darwin", 1}, {"windows", 1}, {"freebsd", 1}} {
		if err := checkMonitorHost(tc.goos, tc.pid); err != nil {
			t.Errorf("%s PID %d: %v", tc.goos, tc.pid, err)
		}
	}
}

func TestAwaitMonitor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	if _, err := w.Write([]byte{'\n'}); err != nil {
		t.Fatal(err)
	}
	if err := awaitMonitor(r, make(chan error), time.Minute); err != nil {
		t.Errorf("ready monitor: %v", err)
	}

	for exit, want := range map[error]string{
		errors.New("exit status 7"): "exited before reaching MonitorCrashes: exit status 7",
		nil:                         "exited before reaching MonitorCrashes: exit status 0",
	} {
		exited := make(chan error, 1)
		exited <- exit
		if err := awaitMonitor(strings.NewReader(""), exited, time.Minute); err == nil || err.Error() != "monitor process "+want {
			t.Errorf("exited monitor: err = %v, want %q", err, want)
		}
	}

	stalled, stalledW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stalled.Close(); stalledW.Close() })
	if err := awaitMonitor(stalled, make(chan error), 10*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not reach MonitorCrashes within 10ms") {
		t.Errorf("stalled monitor: err = %v", err)
	}
}

func TestControlSendGivesUpOnStalledMonitor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	prev := controlWriteTimeout
	controlWriteTimeout = 10 * time.Millisecond
	t.Cleanup(func() { controlWriteTimeout = prev })
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&crashMonitor{control: w}).send(make([]byte, 8<<20))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("send blocked on a monitor that does not read")
	}
}

func TestMonitorCrashesRejectsInvalidConfig(t *testing.T) {
	if err := MonitorCrashes(Config{Endpoint: "ftp://example.com"}); err == nil {
		t.Fatal("MonitorCrashes accepted an invalid endpoint")
	}
	if activeMonitor.Load() != nil {
		t.Error("a monitor was started for an invalid config")
	}
}

func fakeMonitor(t *testing.T) *bufio.Reader {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := activeMonitor.Swap(&crashMonitor{control: w})
	t.Cleanup(func() {
		activeMonitor.Store(prev)
		w.Close()
		r.Close()
	})
	return bufio.NewReader(r)
}

func repanicThrough(report func(map[string]any), value any) (recovered any) {
	defer func() { recovered = recover() }()
	defer report(nil)
	panic(value)
}

func TestReportPanicMarksDeliveredPanics(t *testing.T) {
	marks := fakeMonitor(t)
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.Logger = &lineLogger{}
	})

	goroutine := currentGoroutineID()
	if got := repanicThrough(c.ReportPanic, "delivered\npanic"); got != "delivered\npanic" {
		t.Fatalf("re-panicked with %v", got)
	}
	if line, err := marks.ReadString('\n'); err != nil || line != panicMark(goroutine, "delivered\npanic")+"\n" {
		t.Errorf("mark = %q (%v), want the delivered panic's mark", line, err)
	}

	SetDefaultClient(c)
	t.Cleanup(func() { SetDefaultClient(nil) })
	value := errors.New("legacy delivered")
	if got := repanicThrough(ReportPanic, value); got != value {
		t.Fatalf("re-panicked with %v", got)
	}
	if line, err := marks.ReadString('\n'); err != nil || line != panicMark(goroutine, "legacy delivered")+"\n" {
		t.Errorf("mark = %q (%v), want the legacy panic's mark", line, err)
	}

	rs.mu.Lock()
	rs.statuses = []int{http.StatusForbidden, http.StatusForbidden}
	rs.mu.Unlock()
	repanicThrough(c.ReportPanic, "rejected")
	repanicThrough(ReportPanic, "rejected too")
	activeMonitor.Load().send([]byte("sentinel\n"))
	if line, _ := marks.ReadString('\n'); line != "sentinel\n" {
		t.Errorf("rejected reports wrote mark %q", line)
	}
	if c.Stats().Delivered != 2 || c.Stats().ServerReject != 2 {
		t.Errorf("stats = %+v, want 2 delivered and 2 rejected", c.Stats())
	}
}

type crashRun struct {
	output     string
	exitCode   int
	crasherPID int
	monitorPID int
	session    string
}

func skipWithoutSubprocess(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "js", "wasip1", "plan9", "ios", "android":
		t.Skipf("no crash monitor test on %s", runtime.GOOS)
	}
}

func runCrasher(t *testing.T, mode string, rs *recordingServer) crashRun {
	t.Helper()
	run := execCrasher(t, mode, rs)
	if run.crasherPID == 0 {
		t.Fatalf("crasher did not start a monitor (exit %d); output:\n%s", run.exitCode, run.output)
	}
	return run
}

// Returns only after the monitor exits too. The monitor holds the crasher's stderr pipe.
func execCrasher(t *testing.T, mode string, rs *recordingServer) crashRun {
	t.Helper()
	skipWithoutSubprocess(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashMonitorHelper$")
	cmd.Env = append(os.Environ(),
		"BACKTRACE_GO_TEST_CRASH="+mode,
		"CRASH_TEST_ENDPOINT="+rs.srv.URL,
		"BACKTRACE_ENDPOINT=",
		"BACKTRACE_TOKEN=",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 30 * time.Second
	err := cmd.Run()

	run := crashRun{output: out.String()}
	for _, line := range strings.Split(run.output, "\n") {
		switch {
		case strings.HasPrefix(line, "monitor-pid="):
			fmt.Sscanf(line, "monitor-pid=%d", &run.monitorPID)
		case strings.HasPrefix(line, "crasher-pid="):
			fmt.Sscanf(line, "crasher-pid=%d session=%s", &run.crasherPID, &run.session)
		}
	}
	if strings.Contains(run.output, "WARNING: DATA RACE") {
		t.Errorf("data race in the crasher or the monitor; output:\n%s", run.output)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		if p, findErr := os.FindProcess(run.monitorPID); run.monitorPID > 0 && findErr == nil {
			_ = p.Kill()
		}
		t.Fatalf("crash monitor still running 30s after the crasher exited; output:\n%s", run.output)
	}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("crasher: %v; output:\n%s", err, run.output)
	}
	return run
}

func crashFrames(t *testing.T, p map[string]any) []map[string]any {
	t.Helper()
	threads, _ := p["threads"].(map[string]any)
	main, _ := threads[fmt.Sprint(p["mainThread"])].(map[string]any)
	stack, _ := main["stack"].([]any)
	frames := make([]map[string]any, 0, len(stack))
	for _, f := range stack {
		frames = append(frames, f.(map[string]any))
	}
	return frames
}

func hasFrame(frames []map[string]any, library, function string) bool {
	return slices.ContainsFunc(frames, func(f map[string]any) bool {
		return f["library"] == library && f["funcName"] == function
	})
}

func singleCrashReport(t *testing.T, rs *recordingServer, run crashRun) (map[string]any, map[string]any) {
	t.Helper()
	if rs.count() != 1 {
		t.Fatalf("reports = %d, want 1; output:\n%s", rs.count(), run.output)
	}
	p := rs.last()
	attrs := attrsOf(t, p)
	if attrs["error.type"] != "Crash" || attrs["process.id"] != float64(run.crasherPID) ||
		attrs["application.session"] != run.session || attrs["crash.test"] == nil {
		t.Errorf("error.type %v, process.id %v (crasher %d), session %v (crasher %s), crash.test %v",
			attrs["error.type"], attrs["process.id"], run.crasherPID, attrs["application.session"], run.session, attrs["crash.test"])
	}
	if attrs["crash.app.env"] != false {
		t.Errorf("crash.app.env = %v: the monitor saw an application environment variable", attrs["crash.app.env"])
	}
	if _, ok := attrs["runtime.goroutines"]; ok {
		t.Error("crash report carries the monitor's runtime.goroutines")
	}
	return p, attrs
}

func TestMonitorCrashesReportsUnrecoveredPanic(t *testing.T) {
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	run := runCrasher(t, "goroutine", rs)
	if run.exitCode != 2 {
		t.Errorf("crasher exit code = %d, want 2", run.exitCode)
	}
	p, attrs := singleCrashReport(t, rs, run)
	if attrs["error.message"] != "panic: crash-test goroutine panic" || attrs["report_type"] != "panic" {
		t.Errorf("error.message %q, report_type %v", attrs["error.message"], attrs["report_type"])
	}
	if !reflect.DeepEqual(p["classifiers"], []any{"panic"}) {
		t.Errorf("classifiers = %v, want [panic]", p["classifiers"])
	}
	frames := crashFrames(t, p)
	if len(frames) == 0 || frames[0]["library"] != crashTestPackage || frames[0]["funcName"] != "crashInGoroutine" {
		t.Errorf("innermost frame = %v, want the panicking function", frames)
	}
	if !hasFrame(frames, crashTestPackage, "TestCrashMonitorHelper") {
		t.Errorf("frames %v lack the created-by frame", frames)
	}
}

func TestMonitorCrashesReportsFatalError(t *testing.T) {
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	run := runCrasher(t, "fatal", rs)
	if run.exitCode != 2 {
		t.Errorf("crasher exit code = %d, want 2", run.exitCode)
	}
	p, attrs := singleCrashReport(t, rs, run)
	if attrs["error.message"] != "fatal error" || attrs["report_type"] != "crash" {
		t.Errorf("error.message %q, report_type %v", attrs["error.message"], attrs["report_type"])
	}
	if !reflect.DeepEqual(p["classifiers"], []any{"fatal error"}) {
		t.Errorf("classifiers = %v, want [fatal error]", p["classifiers"])
	}
	frames := crashFrames(t, p)
	if len(frames) == 0 || frames[0]["library"] != "internal/sync" || frames[0]["funcName"] != "fatal" {
		t.Errorf("innermost frame = %v, want internal/sync.fatal", frames)
	}
	if !hasFrame(frames, crashTestPackage, "unlockUnlocked") {
		t.Errorf("frames %v lack the unlocking function", frames)
	}
}

func TestMonitorCrashesSkipsDeliveredPanics(t *testing.T) {
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	run := runCrasher(t, "reported", rs)
	if run.exitCode != 2 {
		t.Errorf("crasher exit code = %d, want 2", run.exitCode)
	}
	if rs.count() != 1 {
		t.Fatalf("reports = %v, want only the ReportPanic report; output:\n%s", rs.messages(), run.output)
	}
	attrs := attrsOf(t, rs.last())
	if attrs["error.type"] != "Unhandled exception" || attrs["error.message"] != "reported\nbefore the crash" {
		t.Errorf("error.type %v, error.message %q", attrs["error.type"], attrs["error.message"])
	}
}

func TestMonitorCrashesReportsUndeliveredPanic(t *testing.T) {
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	rs.mu.Lock()
	rs.statuses = []int{http.StatusForbidden}
	rs.mu.Unlock()
	run := runCrasher(t, "reported", rs)
	if rs.count() != 2 {
		t.Fatalf("reports = %v, want the rejected panic report and the crash; output:\n%s", rs.messages(), run.output)
	}
	rs.mu.Lock()
	payloads := slices.Clone(rs.payloads)
	rs.mu.Unlock()
	if attrsOf(t, payloads[0])["error.type"] != "Unhandled exception" {
		t.Errorf("first report = %v, want the in-process panic report", attrsOf(t, payloads[0]))
	}
	crash := payloads[1]
	attrs := attrsOf(t, crash)
	msg, _ := attrs["error.message"].(string)
	if attrs["error.type"] != "Crash" || !strings.HasPrefix(msg, "panic: reported\n\tbefore the crash [recovered") {
		t.Errorf("error.type %v, error.message %q", attrs["error.type"], msg)
	}
	frames := crashFrames(t, crash)
	faulted := slices.IndexFunc(frames, func(f map[string]any) bool { return f["faulted"] == true })
	if faulted < 0 || frames[faulted]["funcName"] != "reportAndRepanic" {
		t.Errorf("faulted frame in %v, want reportAndRepanic", frames)
	}
}

func TestMonitorCrashesReportsLaterPanicInChain(t *testing.T) {
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	run := runCrasher(t, "chained", rs)
	if run.exitCode != 2 {
		t.Errorf("crasher exit code = %d, want 2", run.exitCode)
	}
	if rs.count() != 2 {
		t.Fatalf("reports = %v, want the delivered panic report and the crash; output:\n%s", rs.messages(), run.output)
	}
	rs.mu.Lock()
	payloads := slices.Clone(rs.payloads)
	rs.mu.Unlock()
	if attrs := attrsOf(t, payloads[0]); attrs["error.type"] != "Unhandled exception" || attrs["error.message"] != "original" {
		t.Errorf("first report = %v, want the in-process panic report", attrs)
	}
	attrs := attrsOf(t, payloads[1])
	want := "panic: original [recovered, repanicked]\n\tpanic: cleanup failed"
	if attrs["error.type"] != "Crash" || attrs["error.message"] != want {
		t.Errorf("error.type %v, error.message %q, want Crash %q", attrs["error.type"], attrs["error.message"], want)
	}
}

func TestMonitorCrashesFailsWhenMonitorExitsEarly(t *testing.T) {
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	run := execCrasher(t, "monitorexit", rs)
	want := "monitor-error: bt: crash monitor not started: monitor process exited before reaching MonitorCrashes: exit status 7"
	if run.exitCode != 3 || !strings.Contains(run.output, want) {
		t.Errorf("exit code %d, output:\n%s\nwant exit 3 and %q", run.exitCode, run.output, want)
	}
	if rs.count() != 0 {
		t.Errorf("reports = %d, want 0", rs.count())
	}
}

func TestMonitorCrashesIgnoresCleanExit(t *testing.T) {
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	run := runCrasher(t, "clean", rs)
	if run.exitCode != 0 {
		t.Errorf("crasher exit code = %d, want 0; output:\n%s", run.exitCode, run.output)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.requests != 0 {
		t.Errorf("requests = %d, want 0", rs.requests)
	}
}
