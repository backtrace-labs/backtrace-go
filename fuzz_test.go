package bt

import (
	"net/http"
	"strings"
	"testing"
)

func newRetryAfterResponse(value string) *http.Response {
	h := http.Header{}
	if value != "" {
		h.Set("Retry-After", value)
	}
	return &http.Response{Header: h}
}

// FuzzBuildThreads exercises the stack parser with arbitrary input: it must
// never panic and never read source text in metadata mode.
func FuzzBuildThreads(f *testing.F) {
	f.Add(stackFixture)
	f.Add("goroutine 1 [running]:\nmain.main()\n\tC:/x/y.go:42 +0x1f\n")
	f.Add("...additional frames elided...\n\n[originating from goroutine 3]:\n")
	f.Add("goroutine running on other thread; stack unavailable\n")
	f.Add("panic({0x1?, 0x2?})\n\tx:1\n")
	f.Fuzz(func(t *testing.T, stack string) {
		threads, sources, _ := buildThreads([]byte(stack), sourceOptions{
			mode: SourceCodeMetadata, contextLines: 4, tabWidth: 8,
		})
		for _, sc := range sources {
			if sc.Text != "" {
				t.Fatalf("metadata mode produced source text: %q", sc.Text)
			}
		}
		_ = threads
	})
}

// FuzzRedactURL: whatever the input, no query token value or userinfo
// password may survive into the output.
func FuzzRedactURL(f *testing.F) {
	f.Add("https://uni.sp.backtrace.io/post?format=json&token=SECRETCANARY")
	f.Add("https://submit.backtrace.io/universe/SECRETCANARY/json")
	f.Add("https://user:SECRETCANARY@host/path")
	f.Add("http://%zz")
	f.Fuzz(func(t *testing.T, raw string) {
		out := redactURL(raw)
		if strings.Contains(raw, "SECRETCANARY") &&
			strings.Contains(out, "SECRETCANARY") &&
			(strings.Contains(raw, "token=SECRETCANARY") ||
				strings.Contains(raw, ":SECRETCANARY@")) {
			t.Fatalf("credential survived redaction: %q -> %q", raw, out)
		}
	})
}

// FuzzRetryAfter: arbitrary header values must produce a bounded,
// non-negative pause.
func FuzzRetryAfter(f *testing.F) {
	f.Add("30")
	f.Add("2147483647")
	f.Add("-5")
	f.Add("Wed, 21 Oct 2015 07:28:00 GMT")
	f.Add("garbage")
	f.Fuzz(func(t *testing.T, header string) {
		resp := newRetryAfterResponse(header)
		d := retryAfter(resp)
		if d < 0 || d > rateLimitMaxPause {
			t.Fatalf("retryAfter(%q) = %v, outside [0, %v]", header, d, rateLimitMaxPause)
		}
	})
}

// FuzzSplitQualifiedFunction must never panic and never lose the input.
func FuzzSplitQualifiedFunction(f *testing.F) {
	f.Add("testing.(*T).Run(0x1, {0x2, 0x9}, 0x3)")
	f.Add("pkg.(*Cache[go.shape.string]).Get(0x1)")
	f.Add("panic({0x1?, 0x2?})")
	f.Add("noDots")
	f.Add("[[[[")
	f.Fuzz(func(t *testing.T, line string) {
		lib, fn := splitQualifiedFunction(line)
		_ = lib
		_ = fn
	})
}

// FuzzSafeMultipartName: outputs must be non-empty and free of control
// characters, quotes, and separators that could corrupt multipart framing.
func FuzzSafeMultipartName(f *testing.F) {
	f.Add("/var/log/app.log")
	f.Add("/tmp/evil\r\nname\x00\"")
	f.Add("")
	f.Add("..")
	f.Fuzz(func(t *testing.T, path string) {
		name := safeMultipartName(path)
		if name == "" {
			t.Fatal("empty multipart name")
		}
		if strings.ContainsAny(name, "\r\n\x00\"\\") {
			t.Fatalf("unsafe characters survived: %q", name)
		}
		if len(name) > maxMultipartNameLength {
			t.Fatalf("name too long: %d", len(name))
		}
	})
}
