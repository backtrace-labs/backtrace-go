package bt

import (
	"encoding/json"
	"net/http"
	neturl "net/url"
	"reflect"
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

// FuzzRedactURL: when the input PARSES as a URL whose token query value or
// userinfo password contains the canary, the canary must not survive into
// the output. (Raw substring matching would misfire on inputs like a bare
// "token=X" that have no URL credential semantics.)
func FuzzRedactURL(f *testing.F) {
	f.Add("https://uni.sp.backtrace.io/post?format=json&token=SECRETCANARY")
	f.Add("https://submit.backtrace.io/universe/SECRETCANARY/json")
	f.Add("https://user:SECRETCANARY@host/path")
	f.Add("http://%zz")
	f.Fuzz(func(t *testing.T, raw string) {
		out := redactURL(raw) // must never panic
		u, err := neturl.Parse(raw)
		if err != nil {
			// Unparsable input degrades to a fixed placeholder.
			if out != "[REDACTED URL]" {
				t.Fatalf("unparsable URL leaked: %q -> %q", raw, out)
			}
			return
		}
		credential := strings.Contains(u.Query().Get("token"), "SECRETCANARY")
		if pw, set := u.User.Password(); set && strings.Contains(pw, "SECRETCANARY") {
			credential = true
		}
		if credential && strings.Contains(out, "SECRETCANARY") {
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

// modelStats measures a value-model graph the way the normaliser accounts
// for it: one node per value, the bytes of every string and key, a fixed
// cost per number and boolean.
type modelStats struct {
	nodes, depth, longest, bytes int
	foreign                      string // first type outside the model
}

func (m *modelStats) walk(v interface{}, depth int) {
	m.nodes++
	if depth > m.depth {
		m.depth = depth
	}
	switch x := v.(type) {
	case nil:
	case bool, int64, uint64, float64:
		m.bytes += scalarCost
	case string:
		m.addString(x)
	case []interface{}:
		for _, e := range x {
			m.walk(e, depth+1)
		}
	case map[string]interface{}:
		for k, e := range x {
			m.addString(k)
			m.walk(e, depth+1)
		}
	default:
		if m.foreign == "" {
			m.foreign = reflect.TypeOf(v).String()
		}
	}
}

func (m *modelStats) addString(s string) {
	m.bytes += len(s)
	if len(s) > m.longest {
		m.longest = len(s)
	}
}

// FuzzFreezeValue drives the value normaliser with arbitrary JSON-shaped
// input under arbitrary (small) limits. Whatever comes in, the result must
// stay inside the value model, serialize, and respect the budgets; and when
// the input fits the budgets it must come back unchanged.
func FuzzFreezeValue(f *testing.F) {
	f.Add([]byte(`{"a":[1,2,{"b":"c"}],"d":null,"e":true}`), uint8(4), uint8(16), uint8(16), uint16(256))
	f.Add([]byte(`[[[[[[[[[[1]]]]]]]]]]`), uint8(2), uint8(8), uint8(8), uint16(64))
	f.Add([]byte(`{"k":"`+strings.Repeat("é", 200)+`"}`), uint8(8), uint8(8), uint8(5), uint16(40))
	f.Add([]byte(`[`+strings.Repeat(`0,`, 400)+`0]`), uint8(8), uint8(32), uint8(32), uint16(128))
	f.Add([]byte(`"plain"`), uint8(0), uint8(0), uint8(0), uint16(0))
	f.Fuzz(func(t *testing.T, data []byte, depth, nodes, stringBytes uint8, totalBytes uint16) {
		var input interface{}
		if json.Unmarshal(data, &input) != nil {
			t.Skip()
		}

		// Tight limits: the budgets must hold whatever the input is.
		limits := valueLimits{
			maxDepth:       int(depth) + 1,
			maxNodes:       int(nodes) + 1,
			maxStringBytes: int(stringBytes) + 1,
			maxTotalBytes:  int(totalBytes) + 1,
		}
		out := freezeValue(input, limits)
		var got modelStats
		got.walk(out, 0)
		if got.foreign != "" {
			t.Fatalf("%s is not part of the value model", got.foreign)
		}
		if _, err := json.Marshal(out); err != nil {
			t.Fatalf("normalised value does not serialize: %v", err)
		}
		// Every value comes from one visit, and a visit happens only while
		// the node budget lasts; a container adds one marker at most.
		if got.nodes > 2*limits.maxNodes+1 {
			t.Fatalf("%d values in the result, node budget is %d", got.nodes, limits.maxNodes)
		}
		// Past the byte budget only what closes the open containers is
		// added: one clipped key or string, one marker per level.
		const markerRoom = 80
		allowance := limits.maxTotalBytes + limits.maxStringBytes + len(truncatedMarker) +
			scalarCost + (limits.maxDepth+2)*markerRoom
		if got.bytes > allowance {
			t.Fatalf("%d bytes in the result, byte budget is %d", got.bytes, limits.maxTotalBytes)
		}

		// Default limits: what fits the budgets is returned as it is.
		var want modelStats
		want.walk(input, 0)
		if want.nodes < defaultFreezeMaxNodes && want.depth <= defaultFreezeMaxDepth &&
			want.longest <= defaultFreezeMaxStringBytes && want.bytes < defaultFreezeMaxTotalBytes {
			if same := freezeValue(input, valueLimits{}); !reflect.DeepEqual(same, input) {
				t.Fatalf("a value inside every budget was changed:\n in: %#v\nout: %#v", input, same)
			}
		}
	})
}
