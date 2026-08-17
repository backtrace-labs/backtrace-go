package bt

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const stackFixture = `goroutine 1 [running]:
github.com/backtrace-labs/backtrace-go.TestMain(0x1400011a960)
	/Users/test-user/Documents/Work/backtrace-go/main_test.go:41 +0x28
main.GetStack()
    /tmp/sandbox889685435/prog.go:30 +0x5f
main.main()
    /tmp/sandbox889685435/prog.go:15 +0x2f

goroutine 6 [runnable]:
main.testFunc()
    /tmp/sandbox889685435/prog.go:22
created by main.main in goroutine 1
    /tmp/sandbox889685435/prog.go:13 +0x1e

goroutine 8 [running]:
main.test.main(0x9)
        /Users/root/Library/Application Support/JetBrains/GoLand2024.1/scratches/scratch_19.go:74 +0xa0c

goroutine 9 [running]:
testing.(*T).Run(0x14000110680, {0x1012116ab, 0x9}, 0x1012ff428)
        /Users/some_file.go:12 +0xa0c
panic({0x100b3f360?, 0x100bb0860?})
	/usr/local/go/src/runtime/something.go:770 +0xf0
created by testing.(*T).Run in goroutine 1
	/usr/local/go/src/testing/foobar.go:1742 +0x668
`

func noSource() sourceOptions {
	return sourceOptions{mode: SourceCodeNone, contextLines: 8, tabWidth: 8}
}

func TestBuildThreadsFixture(t *testing.T) {
	threads, sources, mainThread := buildThreads([]byte(stackFixture), noSource())

	if mainThread != "0" {
		t.Errorf("mainThread = %q, want 0", mainThread)
	}
	if len(sources) != 0 {
		t.Errorf("SourceCodeNone produced %d source entries", len(sources))
	}

	want := map[string]Thread{
		"0": {
			Name:  "goroutine 1 [running]",
			Fault: true,
			Stacks: []StackFrame{
				{FuncName: "GetStack", Library: "main", Line: "30"},
				{FuncName: "main", Library: "main", Line: "15"},
			},
		},
		"1": {
			Name: "goroutine 6 [runnable]",
			Stacks: []StackFrame{
				{FuncName: "testFunc", Library: "main", Line: "22"},
				{FuncName: "main", Library: "main", Line: "13"},
			},
		},
		"2": {
			Name: "goroutine 8 [running]",
			Stacks: []StackFrame{
				{FuncName: "main", Library: "main.test", Line: "74"},
			},
		},
		"3": {
			Name: "goroutine 9 [running]",
			Stacks: []StackFrame{
				{FuncName: "Run", Library: "testing.(*T)", Line: "12"},
				{FuncName: "panic", Library: "runtime", Line: "770"},
				{FuncName: "Run", Library: "testing.(*T)", Line: "1742"},
			},
		},
	}
	if !reflect.DeepEqual(want, threads) {
		t.Errorf("threads mismatch:\n got: %#v\nwant: %#v", threads, want)
	}
}

func TestBuildThreadsWindowsPaths(t *testing.T) {
	stack := "goroutine 1 [running]:\n" +
		"main.main()\n" +
		"\tC:/Users/dev/app/main.go:42 +0x1f\n"

	threads, _, _ := buildThreads([]byte(stack), noSource())
	frame := threads["0"].Stacks[0]
	if frame.Line != "42" {
		t.Errorf("line = %q, want 42 (split at drive-letter colon?)", frame.Line)
	}
}

func TestBuildThreadsWindowsSourcePath(t *testing.T) {
	stack := "goroutine 1 [running]:\n" +
		"main.main()\n" +
		"\tC:/Users/dev/app/main.go:42 +0x1f\n"

	// File mode exercises the path bookkeeping even when unreadable.
	threads, sources, _ := buildThreads([]byte(stack), sourceOptions{mode: SourceCodeFile, contextLines: 8, tabWidth: 8})
	id := threads["0"].Stacks[0].SourceCodeID
	if sources[id].Path != "C:/Users/dev/app/main.go" {
		t.Errorf("source path = %q", sources[id].Path)
	}
}

func TestBuildThreadsNoDotFunctionLineDoesNotPanic(t *testing.T) {
	stack := "goroutine 1 [running]:\n" +
		"mysteryfunction(0x1)\n" +
		"\t/tmp/x.go:5 +0x1\n"

	threads, _, _ := buildThreads([]byte(stack), noSource())
	frame := threads["0"].Stacks[0]
	if frame.FuncName != "mysteryfunction" || frame.Library != "" {
		t.Errorf("frame = %+v", frame)
	}
}

func TestBuildThreadsElidedFramesMarker(t *testing.T) {
	stack := "goroutine 1 [running]:\n" +
		"main.a()\n" +
		"\t/tmp/x.go:1 +0x1\n" +
		"...additional frames elided...\n" +
		"created by main.b in goroutine 2\n" +
		"\t/tmp/x.go:9 +0x2\n"

	threads, _, _ := buildThreads([]byte(stack), noSource())
	got := threads["0"].Stacks
	if len(got) != 2 {
		t.Fatalf("frames = %d, want 2 (elided marker desynced parser?): %+v", len(got), got)
	}
	if got[1].FuncName != "b" || got[1].Line != "9" {
		t.Errorf("frame after elided marker = %+v", got[1])
	}
}

func TestBuildThreadsGenerics(t *testing.T) {
	stack := "goroutine 1 [running]:\n" +
		"example.com/pkg.Map[go.shape.int,go.shape.string](0x1, 0x2)\n" +
		"\t/tmp/gen.go:10 +0x1\n"

	threads, _, _ := buildThreads([]byte(stack), noSource())
	frame := threads["0"].Stacks[0]
	if frame.FuncName != "Map[go.shape.int,go.shape.string]" {
		t.Errorf("FuncName = %q", frame.FuncName)
	}
	if frame.Library != "example.com/pkg" {
		t.Errorf("Library = %q", frame.Library)
	}
}

func TestBuildThreadsGenericReceiver(t *testing.T) {
	stack := "goroutine 1 [running]:\n" +
		"example.com/pkg.(*Cache[go.shape.string]).Get(0x1, 0x2)\n" +
		"\t/tmp/cache.go:33 +0x1\n"

	threads, _, _ := buildThreads([]byte(stack), noSource())
	frame := threads["0"].Stacks[0]
	if frame.FuncName != "Get" {
		t.Errorf("FuncName = %q, want Get", frame.FuncName)
	}
	if frame.Library != "example.com/pkg.(*Cache[go.shape.string])" {
		t.Errorf("Library = %q", frame.Library)
	}
}

func TestBuildThreadsAncestorSectionsIgnored(t *testing.T) {
	// GODEBUG=tracebackancestors output appends ancestor sections after a
	// goroutine's own frames; they are history, not live frames.
	stack := "goroutine 5 [running]:\n" +
		"main.worker()\n" +
		"\t/tmp/x.go:10 +0x1\n" +
		"[originating from goroutine 1]:\n" +
		"main.spawner()\n" +
		"\t/tmp/x.go:99 +0x2\n" +
		"\n" +
		"goroutine 6 [runnable]:\n" +
		"main.other()\n" +
		"\t/tmp/x.go:20 +0x3\n"

	threads, _, _ := buildThreads([]byte(stack), noSource())
	if len(threads) != 2 {
		t.Fatalf("threads = %d, want 2", len(threads))
	}
	if got := threads["0"].Stacks; len(got) != 1 || got[0].FuncName != "worker" {
		t.Errorf("ancestor frames leaked into thread 0: %+v", got)
	}
	if got := threads["1"].Stacks; len(got) != 1 || got[0].FuncName != "other" {
		t.Errorf("thread after ancestor section wrong: %+v", got)
	}
}

func TestBuildThreadsDropsSDKOnlyThreads(t *testing.T) {
	// A goroutine whose every frame is SDK-internal (e.g. the SDK's own
	// send worker) is noise and is omitted; the app goroutine stays.
	stack := "goroutine 1 [running]:\n" +
		"main.caller()\n" +
		"\t/tmp/app.go:30 +0x2\n" +
		"\n" +
		"goroutine 2 [select]:\n" +
		"github.com/backtrace-labs/backtrace-go.(*Client).worker(0x1)\n" +
		"\t/sdk/client.go:320 +0x1\n" +
		"created by github.com/backtrace-labs/backtrace-go.startClient in goroutine 1\n" +
		"\t/sdk/client.go:90 +0x2\n"

	threads, _, _ := buildThreads([]byte(stack), noSource())
	if len(threads) != 1 {
		t.Fatalf("threads = %d, want 1 (SDK-only goroutine kept?): %+v", len(threads), threads)
	}
	if _, ok := threads["0"]; !ok {
		t.Error("app thread missing")
	}
}

func TestBuildThreadsSkipsSDKFrames(t *testing.T) {
	stack := "goroutine 1 [running]:\n" +
		"github.com/backtrace-labs/backtrace-go.Report(0x1, 0x2)\n" +
		"\t/sdk/main.go:200 +0x1\n" +
		"main.caller()\n" +
		"\t/tmp/app.go:30 +0x2\n"

	threads, _, _ := buildThreads([]byte(stack), noSource())
	got := threads["0"].Stacks
	if len(got) != 1 || got[0].FuncName != "caller" {
		t.Errorf("SDK frames not filtered: %+v", got)
	}
}

func TestBuildThreadsFrameWithoutLocation(t *testing.T) {
	stack := "goroutine 17 [syscall]:\n" +
		"goroutine running on other thread; stack unavailable\n" +
		"\n" +
		"goroutine 2 [runnable]:\n" +
		"main.ok()\n" +
		"\t/tmp/x.go:3 +0x1\n"

	threads, _, _ := buildThreads([]byte(stack), noSource())
	if len(threads) != 2 {
		t.Fatalf("threads = %d, want 2", len(threads))
	}
	if len(threads["0"].Stacks) != 0 {
		t.Errorf("dangling function line produced a frame: %+v", threads["0"].Stacks)
	}
	if len(threads["1"].Stacks) != 1 {
		t.Errorf("second thread frames = %+v", threads["1"].Stacks)
	}
}

func TestSourceContextExtraction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.go")
	var sb strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	stack := "goroutine 1 [running]:\n" +
		"main.main()\n" +
		"\t" + path + ":15 +0x1f\n"

	threads, sources, _ := buildThreads([]byte(stack), sourceOptions{
		mode:         SourceCodeContext,
		contextLines: 3,
		tabWidth:     4,
	})

	id := threads["0"].Stacks[0].SourceCodeID
	sc, ok := sources[id]
	if !ok {
		t.Fatalf("no source entry for id %q", id)
	}
	if sc.StartLine != 12 {
		t.Errorf("StartLine = %d, want 12", sc.StartLine)
	}
	wantText := "line 12\nline 13\nline 14\nline 15\nline 16\nline 17\nline 18"
	if sc.Text != wantText {
		t.Errorf("Text = %q, want %q", sc.Text, wantText)
	}
	if sc.TabWidth != 4 {
		t.Errorf("TabWidth = %d, want 4", sc.TabWidth)
	}
}

func TestSourceContextClampsAtFileBounds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.go")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stack := "goroutine 1 [running]:\n" +
		"main.main()\n" +
		"\t" + path + ":1 +0x1f\n"

	threads, sources, _ := buildThreads([]byte(stack), sourceOptions{
		mode:         SourceCodeContext,
		contextLines: 10,
		tabWidth:     8,
	})
	sc := sources[threads["0"].Stacks[0].SourceCodeID]
	if sc.StartLine != 1 {
		t.Errorf("StartLine = %d, want 1", sc.StartLine)
	}
	if !strings.HasPrefix(sc.Text, "one\ntwo\nthree") {
		t.Errorf("Text = %q", sc.Text)
	}
}

func TestSourceFileModeEmbedsWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "whole.go")
	content := "alpha\nbeta\ngamma"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	stack := "goroutine 1 [running]:\n" +
		"main.main()\n" +
		"\t" + path + ":2 +0x1f\n"

	threads, sources, _ := buildThreads([]byte(stack), sourceOptions{
		mode:         SourceCodeFile,
		contextLines: 3,
		tabWidth:     8,
	})
	sc := sources[threads["0"].Stacks[0].SourceCodeID]
	if sc.Text != content {
		t.Errorf("Text = %q, want whole file", sc.Text)
	}
	if sc.StartLine != 1 {
		t.Errorf("StartLine = %d, want 1", sc.StartLine)
	}
}

func TestSourceSnippetsDeduplicated(t *testing.T) {
	stack := "goroutine 1 [running]:\n" +
		"main.a()\n" +
		"\t/tmp/same.go:5 +0x1\n" +
		"main.b()\n" +
		"\t/tmp/same.go:5 +0x2\n" +
		"main.c()\n" +
		"\t/tmp/same.go:9 +0x3\n"

	threads, sources, _ := buildThreads([]byte(stack), sourceOptions{
		mode:         SourceCodeContext,
		contextLines: 2,
		tabWidth:     8,
	})
	frames := threads["0"].Stacks
	if frames[0].SourceCodeID != frames[1].SourceCodeID {
		t.Error("same (path,line) not deduplicated")
	}
	if frames[0].SourceCodeID == frames[2].SourceCodeID {
		t.Error("different lines share a snippet in context mode")
	}
	if len(sources) != 2 {
		t.Errorf("source entries = %d, want 2", len(sources))
	}
}

func TestParseThreadsFromStackLegacyWrapper(t *testing.T) {
	threads, _ := ParseThreadsFromStack([]byte(stackFixture))
	if len(threads) != 4 {
		t.Fatalf("threads = %d, want 4", len(threads))
	}
	if !threads["0"].Fault {
		t.Error("thread 0 not marked faulting")
	}
}

func TestSourceMetadataMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.go")
	if err := os.WriteFile(path, []byte("secret line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stack := "goroutine 1 [running]:\n" +
		"main.main()\n" +
		"\t" + path + ":1 +0x1f\n"

	threads, sources, _ := buildThreads([]byte(stack), sourceOptions{
		mode: SourceCodeMetadata, contextLines: 8, tabWidth: 8,
	})
	sc := sources[threads["0"].Stacks[0].SourceCodeID]
	if sc.Path != path {
		t.Errorf("metadata path = %q", sc.Path)
	}
	if sc.Text != "" {
		t.Errorf("metadata mode leaked source text: %q", sc.Text)
	}
}

func TestSourceRootsAllowlist(t *testing.T) {
	allowedDir := t.TempDir()
	deniedDir := t.TempDir()
	allowed := filepath.Join(allowedDir, "in.go")
	denied := filepath.Join(deniedDir, "out.go")
	for _, p := range []string{allowed, denied} {
		if err := os.WriteFile(p, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	stack := "goroutine 1 [running]:\n" +
		"main.a()\n" +
		"\t" + allowed + ":2 +0x1\n" +
		"main.b()\n" +
		"\t" + denied + ":2 +0x2\n"

	threads, sources, _ := buildThreads([]byte(stack), sourceOptions{
		mode: SourceCodeContext, contextLines: 2, tabWidth: 8,
		roots: []string{allowedDir},
	})
	frames := threads["0"].Stacks
	if sources[frames[0].SourceCodeID].Text == "" {
		t.Error("allowed root produced no source text")
	}
	if got := sources[frames[1].SourceCodeID].Text; got != "" {
		t.Errorf("file outside SourceRoots was read: %q", got)
	}
}

func TestSourceBudgets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.go")
	content := strings.Repeat("padding line\n", 100)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	stack := "goroutine 1 [running]:\n" +
		"main.a()\n" +
		"\t" + path + ":50 +0x1\n"

	// Per-file budget smaller than the file: degrade to path-only.
	_, sources, _ := buildThreads([]byte(stack), sourceOptions{
		mode: SourceCodeContext, contextLines: 3, tabWidth: 8,
		maxFileBytes: 64,
	})
	for _, sc := range sources {
		if sc.Text != "" {
			t.Errorf("per-file budget ignored: %d bytes embedded", len(sc.Text))
		}
	}

	// Total budget of 1 byte: snippet cannot be embedded.
	_, sources, _ = buildThreads([]byte(stack), sourceOptions{
		mode: SourceCodeContext, contextLines: 3, tabWidth: 8,
		maxTotal: 1,
	})
	for _, sc := range sources {
		if sc.Text != "" {
			t.Errorf("total source budget ignored: %d bytes embedded", len(sc.Text))
		}
	}
}

func TestIsSDKFrameBoundary(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"github.com/backtrace-labs/backtrace-go.Report", true},
		{"github.com/backtrace-labs/backtrace-go", true},
		{"github.com/backtrace-labs/backtrace-go/bthttp.(*Handler).Handle", true},
		{"github.com/backtrace-labs/backtrace-go-fork.Report", false},
		{"github.com/backtrace-labs/backtrace-gopher.Report", false},
		{"main.main", false},
	}
	for _, c := range cases {
		if got := isSDKFrame(c.name); got != c.want {
			t.Errorf("isSDKFrame(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSplitQualifiedFunction(t *testing.T) {
	cases := []struct{ in, lib, fn string }{
		{"main.main()", "main", "main"},
		{"main.main", "main", "main"},
		{"testing.(*T).Run(0x1, {0x2, 0x9}, 0x3)", "testing.(*T)", "Run"},
		{"github.com/x/y.fn(0x1)", "github.com/x/y", "fn"},
		{"pkg.F[go.shape.int](0x1)", "pkg", "F[go.shape.int]"},
		{"pkg.(*Cache[go.shape.string]).Get(0x1)", "pkg.(*Cache[go.shape.string])", "Get"},
		{"panic({0x1?, 0x2?})", "", "panic"},
		{"noDotsAtAll(0x1)", "", "noDotsAtAll"},
		{"main.main.func1()", "main.main", "func1"},
	}
	for _, c := range cases {
		lib, fn := splitQualifiedFunction(c.in)
		if lib != c.lib || fn != c.fn {
			t.Errorf("splitQualifiedFunction(%q) = (%q, %q), want (%q, %q)",
				c.in, lib, fn, c.lib, c.fn)
		}
	}
}
