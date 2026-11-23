package bt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

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
