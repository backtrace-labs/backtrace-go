package bt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"unicode/utf8"
)

func unwrapErrorChain(err error, maxDepth, maxNodes int) []errorChainLink {
	return errorChainLinks(walkErrorGraph(err, maxDepth, maxNodes))
}

func TestSplitLocation(t *testing.T) {
	cases := []struct{ in, path, line string }{
		{"\t/tmp/x.go:42 +0x1f", "/tmp/x.go", "42"},
		{"\t/tmp/x.go:42", "/tmp/x.go", "42"},
		{"\t/Users/me/C +Proj/main.go:42 +0x1f", "/Users/me/C +Proj/main.go", "42"},
		{"\t/Users/me/C +0xProj/main.go:42", "/Users/me/C +0xProj/main.go", "42"},
		{"\t/src/cgo.c:12 pc=0x7fff1234", "/src/cgo.c", "12"},
		{"\t/tmp/x.go:42 +0x1f fp=0xc000 sp=0xc008 pc=0x1234", "/tmp/x.go", "42"},
		{"\tC:/Users/dev/app/main.go:42 +0x1f", "C:/Users/dev/app/main.go", "42"},
		{"\tC:/Users/dev/app/main.go", "C:/Users/dev/app/main.go", ""},
		{"\t/tmp/x.go:12abc", "/tmp/x.go:12abc", ""},
		{"\t/tmp/x.go:", "/tmp/x.go:", ""},
	}
	for _, c := range cases {
		path, line := splitLocation(c.in)
		if path != c.path || line != c.line {
			t.Errorf("splitLocation(%q) = (%q, %q), want (%q, %q)", c.in, path, line, c.path, c.line)
		}
	}
}

func TestIsElidedMarker(t *testing.T) {
	for line, want := range map[string]bool{
		"...additional frames elided...": true,
		"...204 frames elided...":        true,
		"...1 frames elided...":          true,
		"... frames elided...":           false,
		"...x frames elided...":          false,
		"main.main()":                    false,
	} {
		if got := isElidedMarker(line); got != want {
			t.Errorf("isElidedMarker(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestBuildThreadsLiveElidedMarker(t *testing.T) {
	stack := "goroutine 1 [running]:\n" +
		"main.recurse(...)\n" +
		"\t/tmp/x.go:5 +0x1\n" +
		"...204 frames elided...\n" +
		"main.recurse(0x1)\n" +
		"\t/tmp/x.go:5 +0x2\n" +
		"main.main()\n" +
		"\t/tmp/x.go:9 +0x3\n"

	threads, _, _ := buildThreads([]byte(stack), noSource())
	got := threads["0"].Stacks
	want := []StackFrame{
		{FuncName: "recurse", Library: "main", Line: "5"},
		{FuncName: "recurse", Library: "main", Line: "5"},
		{FuncName: "main", Library: "main", Line: "9"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("frames = %+v, want %+v", got, want)
	}
}

func TestShowFrameMatchesRuntimeFilter(t *testing.T) {
	cases := []struct {
		name  string
		first bool
		want  bool
	}{
		{"main.main", false, true},
		{"runtime.gopanic", false, true},
		{"runtime.gopanic", true, false},
		{"runtime.goexit", false, false},
		{"runtime.main", false, false},
		{"runtime.sigpanic", false, false},
		{"runtime.Goexit", false, true},
		{"runtime.(*Func).Name", false, true},
		{"runtime.(*mheap).alloc", false, false},
		{"runtime.(*mspan).Exported", false, false},
		{"runtime.runFinalizers", false, true},
		{"runtime/debug.Stack", false, true},
		{"nodots", false, false},
	}
	for _, c := range cases {
		if got := showFrame(c.name, c.first); got != c.want {
			t.Errorf("showFrame(%q, first=%v) = %v, want %v", c.name, c.first, got, c.want)
		}
	}
}

func TestMarkPanicFrame(t *testing.T) {
	frame := func(lib, fn string) StackFrame { return StackFrame{Library: lib, FuncName: fn} }
	cases := []struct {
		name   string
		frames []StackFrame
		want   int
	}{
		{"user panic", []StackFrame{frame("app.handler", "func1"), frame("runtime", "panic"), frame("app", "work"), frame("app", "main")}, 2},
		{"runtime error", []StackFrame{frame("app.handler", "func1"), frame("runtime", "panic"), frame("runtime", "panicmem"), frame("runtime.(*T)", "m"), frame("app", "work")}, 4},
		{"nested panic", []StackFrame{frame("app", "outer"), frame("runtime", "panic"), frame("app.cleanup", "func1"), frame("runtime", "panic"), frame("app", "work")}, 2},
		{"no panic", []StackFrame{frame("app", "work"), frame("app", "main")}, -1},
		{"panic outermost", []StackFrame{frame("app", "func1"), frame("runtime", "panic")}, -1},
	}
	for _, c := range cases {
		markPanicFrame(c.frames)
		got := -1
		for i, f := range c.frames {
			if f.Faulted {
				if got >= 0 {
					t.Errorf("%s: several faulted frames", c.name)
				}
				got = i
			}
		}
		if got != c.want {
			t.Errorf("%s: faulted frame = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestSplitGoroutineText(t *testing.T) {
	cases := []struct{ name, text, wantName, wantCreator string }{
		{
			"main goroutine",
			"goroutine 1 [running]:\nmain.main()\n\t/a/main.go:5 +0x1d\n",
			"goroutine 1 [running]", "",
		},
		{
			"spawned goroutine with ancestors",
			"goroutine 7 [running]:\nmain.work()\n\t/a/main.go:9 +0x1\ncreated by main.main in goroutine 1\n\t/a/main.go:5 +0x2\n" +
				"[originating from goroutine 1]:\nmain.main(...)\n\t/a/main.go:5\ncreated by main.other\n\t/a/main.go:7\n",
			"goroutine 7 [running]", "goroutine 7 [running]:\ncreated by main.main in goroutine 1\n\t/a/main.go:5 +0x2",
		},
	}
	for _, c := range cases {
		name, creator := splitGoroutineText(c.text)
		if name != c.wantName || string(creator) != c.wantCreator {
			t.Errorf("%s: got %q, %q, want %q, %q", c.name, name, creator, c.wantName, c.wantCreator)
		}
	}
}

type tracePC uintptr

type pkgErrorsShape struct{ pcs []uintptr }

func (e *pkgErrorsShape) Error() string { return "pkg" }

func (e *pkgErrorsShape) StackTrace() []tracePC {
	out := make([]tracePC, len(e.pcs))
	for i, pc := range e.pcs {
		out[i] = tracePC(pc)
	}
	return out
}

type callersShape struct{ pcs []uintptr }

func (e callersShape) Error() string      { return "callers" }
func (e callersShape) Callers() []uintptr { return e.pcs }

type wrongTraceShape struct{}

func (wrongTraceShape) Error() string             { return "wrong" }
func (wrongTraceShape) StackTrace(int) []uintptr  { return []uintptr{1} }
func (wrongTraceShape) Callers() ([]uintptr, int) { return []uintptr{1}, 0 }

type panickingTraceShape struct{}

func (panickingTraceShape) Error() string         { return "panicking" }
func (panickingTraceShape) StackTrace() []uintptr { panic("StackTrace panic") }

func TestErrorStackPCs(t *testing.T) {
	pcs := []uintptr{11, 22, 33}
	if got := errorStackPCs(&pkgErrorsShape{pcs: pcs}); fmt.Sprint(got) != fmt.Sprint(pcs) {
		t.Errorf("pkg/errors shape = %v, want %v", got, pcs)
	}
	shared := append([]uintptr(nil), pcs...)
	got := errorStackPCs(callersShape{pcs: shared})
	if fmt.Sprint(got) != fmt.Sprint(pcs) {
		t.Errorf("Callers shape = %v, want %v", got, pcs)
	}
	shared[0] = 99
	if got[0] != 11 {
		t.Error("Callers result shares the error's slice")
	}
	long := make([]uintptr, maxCallerFrames+10)
	if got := errorStackPCs(callersShape{pcs: long}); len(got) != maxCallerFrames {
		t.Errorf("long stack kept %d PCs, want %d", len(got), maxCallerFrames)
	}
	for name, err := range map[string]error{
		"wrong signatures":   wrongTraceShape{},
		"panicking method":   panickingTraceShape{},
		"typed nil":          (*pkgErrorsShape)(nil),
		"no stack":           errors.New("plain"),
		"fmt wrapper itself": fmt.Errorf("x: %w", &pkgErrorsShape{pcs: pcs}),
	} {
		if got := errorStackPCs(err); got != nil {
			t.Errorf("%s: errorStackPCs = %v, want nil", name, got)
		}
	}
}

func TestErrorOriginPCsPicksDeepest(t *testing.T) {
	inner := &pkgErrorsShape{pcs: []uintptr{1}}
	outer := &testWrapErr{msg: "outer", inner: callersShape{pcs: []uintptr{2}}}
	joined := errors.Join(
		fmt.Errorf("a: %w", outer),
		fmt.Errorf("b: %w", fmt.Errorf("c: %w", fmt.Errorf("d: %w", inner))),
	)
	nodes := walkErrorGraph(joined, DefaultMaxErrorDepth, DefaultMaxErrorNodes)
	if got := errorOriginPCs(nodes); fmt.Sprint(got) != "[1]" {
		t.Errorf("origin = %v, want the deepest stack [1]", got)
	}
	if got := errorOriginPCs(walkErrorGraph(joined, -1, DefaultMaxErrorNodes)); got != nil {
		t.Errorf("negative MaxErrorDepth origin = %v, want nil", got)
	}
}

type valueWrap struct {
	n     int
	inner error
}

func (v valueWrap) Error() string { return fmt.Sprintf("wrap %d", v.n) }
func (v valueWrap) Unwrap() error { return v.inner }

type valueErr struct{ msg string }

func (v valueErr) Error() string { return v.msg }

type mapErr map[string]error

func (m mapErr) Error() string { return "map" }
func (m mapErr) Unwrap() error { return m["next"] }

func TestErrorChainValueWrappers(t *testing.T) {
	chain := unwrapErrorChain(valueWrap{1, valueWrap{2, errors.New("boom")}}, DefaultMaxErrorDepth, DefaultMaxErrorNodes)
	if len(chain) != 3 || chain[2].Message != "boom" {
		t.Errorf("value wrappers chain = %+v, want 3 nodes ending in boom", chain)
	}
	if chain := unwrapErrorChain(errors.Join(valueErr{"dup"}, valueErr{"dup"}), DefaultMaxErrorDepth, DefaultMaxErrorNodes); len(chain) != 3 {
		t.Errorf("join of equal values = %d nodes, want 3", len(chain))
	}
	cyclic := mapErr{}
	cyclic["next"] = cyclic
	if chain := unwrapErrorChain(cyclic, DefaultMaxErrorDepth, DefaultMaxErrorNodes); len(chain) != 1 {
		t.Errorf("cyclic map error = %d nodes, want 1", len(chain))
	}
	if chain := unwrapErrorChain(endlessErr{}, 5, DefaultMaxErrorNodes); len(chain) != 6 {
		t.Errorf("endless value chain = %d nodes, want 6 (depth cap 5)", len(chain))
	}
	if chain := unwrapErrorChain(endlessErr{}, DefaultMaxErrorDepth, 4); len(chain) != 4 {
		t.Errorf("endless value chain = %d nodes, want the node cap of 4", len(chain))
	}
}

type endlessErr struct{}

func (endlessErr) Error() string { return "endless" }
func (endlessErr) Unwrap() error { return endlessErr{} }

func TestErrorChainMessagesCapped(t *testing.T) {
	long := strings.Repeat("é", 700)
	chain := unwrapErrorChain(fmt.Errorf("outer: %w", errors.New(long)), DefaultMaxErrorDepth, DefaultMaxErrorNodes)
	if len(chain) != 2 {
		t.Fatalf("chain = %d nodes", len(chain))
	}
	for i, link := range chain {
		if len(link.Message) > maxErrorMessageBytes || !utf8.ValidString(link.Message) {
			t.Errorf("node %d message = %d bytes (valid %v)", i, len(link.Message), utf8.ValidString(link.Message))
		}
	}
	if !strings.HasPrefix(long, chain[1].Message) || len(chain[1].Message) < maxErrorMessageBytes-1 {
		t.Errorf("node 1 message is not the longest valid prefix: %d bytes", len(chain[1].Message))
	}
}

func TestRuntimeAttributesFromMetrics(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(37))
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(1 << 40))

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	attrs := map[string]any{}
	runtimeAttributes(attrs)
	runtime.ReadMemStats(&after)

	if attrs["runtime.gogc"] != int64(37) {
		t.Errorf("runtime.gogc = %#v, want 37", attrs["runtime.gogc"])
	}
	if attrs["runtime.gomemlimit"] != uint64(1<<40) {
		t.Errorf("runtime.gomemlimit = %#v, want %d", attrs["runtime.gomemlimit"], uint64(1<<40))
	}
	alloc, _ := attrs["memory.heap.alloc"].(uint64)
	sys, _ := attrs["memory.heap.sys"].(uint64)
	objects, _ := attrs["memory.heap.objects"].(uint64)
	if alloc == 0 || objects == 0 || sys < alloc {
		t.Errorf("heap alloc=%d sys=%d objects=%d", alloc, sys, objects)
	}
	// Stack and GC work spans come out of HeapSys. Only the sum never shrinks.
	if limit := after.HeapSys + after.StackInuse + after.GCSys; sys > limit {
		t.Errorf("memory.heap.sys = %d, above MemStats HeapSys+StackInuse+GCSys %d after the read", sys, limit)
	}
	if gc, _ := attrs["gc.count"].(uint64); gc < uint64(before.NumGC) || gc > uint64(after.NumGC) {
		t.Errorf("gc.count = %d, MemStats.NumGC before %d and after %d", gc, before.NumGC, after.NumGC)
	}

	debug.SetGCPercent(-1)
	runtimeAttributes(attrs)
	if attrs["runtime.gogc"] != "off" {
		t.Errorf("runtime.gogc with GC off = %#v, want off", attrs["runtime.gogc"])
	}
}

type statOnlyFiles struct {
	t    *testing.T
	info os.FileInfo
}

func (f statOnlyFiles) Stat(string) (os.FileInfo, error) { return f.info, nil }

func (f statOnlyFiles) OpenFile(name string, _ int, _ os.FileMode) (*os.File, error) {
	f.t.Errorf("%s opened although Stat reported %v", name, f.info.Mode())
	return nil, errors.New("unexpected open")
}

func TestOpenRegularFileChecksBeforeOpening(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := openRegularFile(statOnlyFiles{t, info}, "device"); !errors.Is(err, errNotRegularFile) {
		t.Errorf("err = %v, want errNotRegularFile", err)
	}
}

func TestOpenRegularFileRejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, info, err := openRegularFile(hostFiles{}, path)
	if err != nil || info.Size() != 4 {
		t.Fatalf("regular file: info=%v err=%v", info, err)
	}
	f.Close()
	if _, _, err := openRegularFile(hostFiles{}, dir); !errors.Is(err, errNotRegularFile) {
		t.Errorf("directory: err = %v, want errNotRegularFile", err)
	}
	if _, _, err := openRegularFile(hostFiles{}, filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file opened")
	}
}

func TestSourceRootsServeEveryStackOfOneBuilder(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"first.go", "second.go"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	sources := newSourceBuilder(sourceOptions{
		mode: SourceCodeContext, contextLines: 1, tabWidth: 8,
		roots: []string{dir},
	})
	defer sources.close()
	for _, path := range paths {
		stack := "goroutine 1 [running]:\nmain.a()\n\t" + path + ":2 +0x1\n"
		parseStackText([]byte(stack), sources)
	}
	texts := map[string]string{}
	for _, sc := range sources.result() {
		texts[sc.Path] = sc.Text
	}
	for _, path := range paths {
		if texts[path] == "" {
			t.Errorf("no source text for %s, a later stack of the same report", filepath.Base(path))
		}
	}
}
