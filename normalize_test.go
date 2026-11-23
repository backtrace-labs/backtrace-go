package bt

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type testNamedInt int

type testNamedString string

type testNamedFloat float64

type testNamedFloat32 float32

type testNamedBool bool

type testNamedUint uint16

type testSelfPointer *testSelfPointer

type testStringer struct{ v string }

func (s testStringer) String() string { return "stringer:" + s.v }

type testValueError struct{}

func (testValueError) Error() string { return "value error" }

func TestScalarValueRules(t *testing.T) {
	when := time.Date(2026, 11, 4, 10, 30, 0, 123000000, time.UTC)
	n := 7
	var nilPtr *int
	var typedNilErr *testPanicMethodError
	var nilMap map[string]int

	cases := []struct {
		name string
		in   any
		want any
		kind valueKind
	}{
		{"string", "s", "s", valueScalar},
		{"bool", true, true, valueScalar},
		{"int", 5, int64(5), valueScalar},
		{"negative int", -5, "-5", valueScalar},
		{"negative int8", int8(-8), "-8", valueScalar},
		{"int16", int16(16), int64(16), valueScalar},
		{"negative int32", int32(-32), "-32", valueScalar},
		{"int64", int64(64), int64(64), valueScalar},
		{"uint", uint(1), uint64(1), valueScalar},
		{"uint8", uint8(8), uint64(8), valueScalar},
		{"uint16", uint16(16), uint64(16), valueScalar},
		{"uint32", uint32(32), uint64(32), valueScalar},
		{"uintptr", uintptr(9), uint64(9), valueScalar},
		{"named bool", testNamedBool(true), true, valueScalar},
		{"named uint", testNamedUint(3), uint64(3), valueScalar},
		{"named float32", testNamedFloat32(2), int64(2), valueScalar},
		{"uint64 max", uint64(math.MaxUint64), "18446744073709551615", valueScalar},
		{"whole float", 3.0, int64(3), valueScalar},
		{"negative zero", math.Copysign(0, -1), int64(0), valueScalar},
		{"fraction", 0.75, "0.75", valueScalar},
		{"negative whole float", -2.0, "-2", valueScalar},
		{"float at 2^53", float64(1 << 53), "9.007199254740992e+15", valueScalar},
		{"float32", float32(0.1), "0.1", valueScalar},
		{"NaN", math.NaN(), "[bt: non-finite float NaN]", valueScalar},
		{"+Inf", math.Inf(1), "[bt: non-finite float +Inf]", valueScalar},
		{"-Inf", math.Inf(-1), "[bt: non-finite float -Inf]", valueScalar},
		{"error", errors.New("boom"), "boom", valueScalar},
		{"value error", testValueError{}, "value error", valueScalar},
		{"time", when, "2026-11-04T10:30:00.123Z", valueScalar},
		{"time pointer", &when, "2026-11-04T10:30:00.123Z", valueScalar},
		{"stringer", testStringer{"x"}, nil, valueComposite},
		{"named int", testNamedInt(4), int64(4), valueScalar},
		{"negative named int", testNamedInt(-4), "-4", valueScalar},
		{"named string", testNamedString("n"), "n", valueScalar},
		{"named float", testNamedFloat(1.5), "1.5", valueScalar},
		{"int pointer", &n, int64(7), valueScalar},
		{"complex", complex(1, 2), "[bt: unsupported value of type complex128]", valueScalar},
		{"nil", nil, nil, valueNil},
		{"nil pointer", nilPtr, nil, valueNil},
		{"typed nil error", typedNilErr, nil, valueNil},
		{"nil map", nilMap, nil, valueNil},
		{"map", map[string]int{"a": 1}, nil, valueComposite},
		{"slice", []string{"a"}, nil, valueComposite},
		{"array", [2]int{1, 2}, nil, valueComposite},
		{"struct", struct{ A int }{1}, nil, valueComposite},
		{"struct pointer", &struct{ A int }{1}, nil, valueComposite},
	}
	for _, c := range cases {
		got, kind := scalarValue(c.in)
		if kind != c.kind || got != c.want {
			t.Errorf("%s: scalarValue(%#v) = (%#v, %v), want (%#v, %v)", c.name, c.in, got, kind, c.want, c.kind)
		}
	}

	var got any
	var kind valueKind
	mustNotPanic(t, "panicking Stringer", func() { got, kind = scalarValue(testPanicStringer{}) })
	if kind != valueComposite {
		t.Errorf("panicking Stringer = (%#v, %v), want a composite", got, kind)
	}
	got, kind = scalarValue(make(chan int))
	if _, ok := got.(string); kind != valueScalar || !ok {
		t.Errorf("chan = (%#v, %v), want a string", got, kind)
	}

	var self testSelfPointer
	self = &self
	got, kind = scalarValue(self)
	if _, ok := got.(string); kind != valueScalar || !ok {
		t.Errorf("self-referencing pointer = (%#v, %v), want a string", got, kind)
	}
}

func TestNormalizeAttributesMovesCompositesAndRemovesNil(t *testing.T) {
	attrs := map[string]any{
		"keep":          1,
		"gone":          nil,
		"tags":          []string{"a", "b"},
		"taken":         map[string]int{"x": 1},
		"error.message": strings.Repeat("é", 600),
	}
	annotations := map[string]any{"taken": "existing"}

	normalizeAttributes(attrs, annotations)

	if _, ok := attrs["gone"]; ok {
		t.Error("nil attribute kept")
	}
	if attrs["keep"] != int64(1) {
		t.Errorf("keep = %#v", attrs["keep"])
	}
	if _, ok := attrs["tags"]; ok {
		t.Error("slice attribute left in attributes")
	}
	if raw, ok := annotations["tags"].(json.RawMessage); !ok || string(raw) != `["a","b"]` {
		t.Errorf("tags annotation = %#v", annotations["tags"])
	}
	if annotations["taken"] != "existing" {
		t.Errorf("existing annotation overwritten: %#v", annotations["taken"])
	}
	msg, _ := attrs["error.message"].(string)
	if len(msg) > maxErrorMessageBytes || !utf8.ValidString(msg) || len(msg) < maxErrorMessageBytes-1 {
		t.Errorf("error.message = %d bytes (valid UTF-8 %v), want <= %d", len(msg), utf8.ValidString(msg), maxErrorMessageBytes)
	}
}

func TestCappedErrorMessageKeepsFullText(t *testing.T) {
	long := strings.Repeat("m", maxErrorMessageBytes) + "TAIL"
	cases := []struct {
		name        string
		msg         string
		annotations map[string]any
		want        any
	}{
		{"long message", long, map[string]any{}, long},
		{"existing annotation wins", long, map[string]any{messageAnnotation: "kept"}, "kept"},
		{"short message", "short", map[string]any{}, nil},
	}
	for _, c := range cases {
		attrs := map[string]any{"error.message": c.msg}
		normalizeAttributes(attrs, c.annotations)
		if got := c.annotations[messageAnnotation]; got != c.want {
			t.Errorf("%s: Message annotation = %.20q, want %.20q", c.name, got, c.want)
		}
		if msg, _ := attrs["error.message"].(string); len(msg) > maxErrorMessageBytes {
			t.Errorf("%s: error.message = %d bytes", c.name, len(msg))
		}
	}
}

func TestEncodeCompositeFreezesFirst(t *testing.T) {
	if raw := encodeComposite(map[string]float64{"ratio": math.NaN()}); !json.Valid(raw) || !strings.Contains(string(raw), "[bt: non-finite float NaN]") {
		t.Errorf("NaN map = %.200s, want valid JSON with the NaN marker", raw)
	}
	if text := string(encodeComposite(map[string]int{"a": 1})); text != `{"a":1}` {
		t.Errorf("encodeComposite = %q", text)
	}
}

func TestEncodeAnnotationsIsolatesFailures(t *testing.T) {
	out := encodeAnnotations(map[string]any{
		"good":    map[string]int{"a": 1},
		"nan":     math.NaN(),
		"chan":    make(chan int),
		"raw":     json.RawMessage(`{"ok":true}`),
		"badraw":  json.RawMessage(`{"broken"`),
		"panics":  panickyJSON{},
		"nothing": nil,
	})
	if string(out["good"]) != `{"a":1}` || string(out["raw"]) != `{"ok":true}` || string(out["nothing"]) != "null" {
		t.Errorf("encodable annotations changed: %s %s %s", out["good"], out["raw"], out["nothing"])
	}
	for _, k := range []string{"nan", "chan", "badraw", "panics"} {
		var s string
		if err := json.Unmarshal(out[k], &s); err != nil || !strings.Contains(s, "not JSON-encodable") {
			t.Errorf("%s = %s, want placeholder string", k, out[k])
		}
	}
	if _, err := json.Marshal(out); err != nil {
		t.Errorf("encoded annotations do not marshal: %v", err)
	}
}

func TestTruncateUTF8(t *testing.T) {
	if got := truncateUTF8("héllo", 2); got != "h" {
		t.Errorf("truncateUTF8 split a rune: %q", got)
	}
	if got := truncateUTF8("abc", 5); got != "abc" {
		t.Errorf("short string changed: %q", got)
	}
	if got := truncateUTF8("abc", 0); got != "" {
		t.Errorf("zero budget = %q", got)
	}
}

func TestErrorClassifier(t *testing.T) {
	custom := &testWrapErr{msg: "custom"}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"plain", errors.New("x"), "*errors.errorString"},
		{"wrapped sentinel", fmt.Errorf("ctx: %w", errors.New("x")), "*errors.errorString"},
		{"multi-wrapped sentinels", fmt.Errorf("%w; %w", errors.New("a"), errors.New("b")), "*errors.errorString"},
		{"joined sentinel", errors.Join(errors.New("a")), "*errors.errorString"},
		{"wrapped custom", fmt.Errorf("ctx: %w", custom), "*bt.testWrapErr"},
		{"custom over sentinel", &testWrapErr{msg: "outer", inner: errors.New("root")}, "*bt.testWrapErr"},
		{"join takes first branch", fmt.Errorf("ctx: %w", errors.Join(testValueError{}, custom)), "bt.testValueError"},
		{"innermost wins", &testWrapErr{msg: "outer", inner: testValueError{}}, "bt.testValueError"},
	}
	for _, c := range cases {
		if got := errorClassifier(c.err); got != c.want {
			t.Errorf("%s: errorClassifier = %q, want %q", c.name, got, c.want)
		}
	}

	cyclic := &testWrapErr{msg: "cycle"}
	cyclic.inner = cyclic
	if got := errorClassifier(cyclic); got != "*bt.testWrapErr" {
		t.Errorf("cyclic chain classifier = %q", got)
	}

	var typedNil *testPanicMethodError
	if got := errorClassifier(typedNil); got != "*bt.testPanicMethodError" {
		t.Errorf("typed-nil classifier = %q", got)
	}
}

func TestPrepareBreadcrumbLimits(t *testing.T) {
	long, ok := prepareBreadcrumb(Breadcrumb{Message: strings.Repeat("é", 1000)})
	if !ok || len(long.Message) > maxBreadcrumbMessageBytes || !utf8.ValidString(long.Message) {
		t.Errorf("message cap: ok=%v len=%d valid=%v", ok, len(long.Message), utf8.ValidString(long.Message))
	}

	attrs, ok := prepareBreadcrumb(Breadcrumb{
		Message: "kept",
		Attributes: map[string]any{
			"ratio":  math.NaN(),
			"neg":    -1,
			"nested": map[string]int{"a": 1},
			"gone":   nil,
			"err":    errors.New("e"),
		},
	})
	if !ok {
		t.Fatal("breadcrumb with convertible attributes dropped")
	}
	want := map[string]any{"ratio": "[bt: non-finite float NaN]", "neg": "-1", "nested": `{"a":1}`, "err": "e"}
	if len(attrs.Attributes) != len(want) {
		t.Errorf("attributes = %#v, want %#v", attrs.Attributes, want)
	}
	for k, v := range want {
		if attrs.Attributes[k] != v {
			t.Errorf("attribute %s = %#v, want %#v", k, attrs.Attributes[k], v)
		}
	}

	big, ok := prepareBreadcrumb(Breadcrumb{
		Message:    "message survives",
		Attributes: map[string]any{"blob": strings.Repeat("x", 8000)},
	})
	if !ok || big.Attributes != nil || big.Message != "message survives" {
		t.Errorf("oversized attributes: ok=%v attrs=%d message=%q", ok, len(big.Attributes), big.Message)
	}

	mixed, ok := prepareBreadcrumb(Breadcrumb{
		Message:    strings.Repeat("<", 1024),
		Attributes: map[string]any{"blob": strings.Repeat("y", 3000)},
	})
	if !ok || mixed.Attributes == nil || len(mixed.Message) == 0 || len(mixed.Message) == 1024 {
		t.Errorf("message not shortened to fit attributes: ok=%v attrs=%v message=%d", ok, mixed.Attributes != nil, len(mixed.Message))
	}

	escaped, ok := prepareBreadcrumb(Breadcrumb{
		Message:    strings.Repeat("<", 1024),
		Attributes: map[string]any{"blob": strings.Repeat("<", 3000)},
	})
	if !ok || escaped.Attributes != nil || len(escaped.Message) == 0 || len(escaped.Message) == 1024 {
		t.Errorf("attributes not dropped or message not cut: ok=%v attrs=%v message=%d", ok, escaped.Attributes != nil, len(escaped.Message))
	}

	for _, b := range []Breadcrumb{long, attrs, big, mixed, escaped} {
		b.ID = math.MaxUint64
		if size := encodedBreadcrumbSize(b); size > maxBreadcrumbBytes {
			t.Errorf("breadcrumb encodes to %d bytes, cap %d", size, maxBreadcrumbBytes)
		}
	}

	if _, ok := prepareBreadcrumb(Breadcrumb{Type: strings.Repeat("t", 5000)}); ok {
		t.Error("breadcrumb that cannot fit was kept")
	}
}

func TestBreadcrumbIDsSeededFromClock(t *testing.T) {
	jan2026 := uint64(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	if breadcrumbIDSeed < jan2026 || breadcrumbIDSeed > uint64(time.Now().UnixMilli()) {
		t.Fatalf("seed = %d, want the package start time in Unix ms", breadcrumbIDSeed)
	}
	r := newBreadcrumbRing(4)
	r.add(Breadcrumb{Message: "a"})
	r.add(Breadcrumb{Message: "b"})
	crumbs := r.snapshot()
	if len(crumbs) != 2 || crumbs[0].ID < breadcrumbIDSeed || crumbs[1].ID <= crumbs[0].ID {
		t.Errorf("ids = %v; want at or above the seed %d and increasing", crumbs, breadcrumbIDSeed)
	}
}

func TestBreadcrumbConstants(t *testing.T) {
	levels := []string{BreadcrumbDebug, BreadcrumbInfo, BreadcrumbWarning, BreadcrumbError, BreadcrumbFatal}
	if strings.Join(levels, ",") != "debug,info,warning,error,fatal" {
		t.Errorf("levels = %v", levels)
	}
	types := []string{BreadcrumbTypeManual, BreadcrumbTypeLog, BreadcrumbTypeNavigation, BreadcrumbTypeHTTP,
		BreadcrumbTypeSystem, BreadcrumbTypeUser, BreadcrumbTypeConfiguration}
	if strings.Join(types, ",") != "manual,log,navigation,http,system,user,configuration" {
		t.Errorf("types = %v", types)
	}
}

func TestShrinkReportSteps(t *testing.T) {
	chain := make([]errorChainLink, 20)
	for i := range chain {
		chain[i] = errorChainLink{ID: i, Type: "*bt.testWrapErr", Message: strings.Repeat("m", 100)}
	}
	build := func() *ReportData {
		return &ReportData{
			UUID:        uuid4(),
			Timestamp:   1,
			Classifiers: []string{"message"},
			Attributes:  map[string]any{"long": strings.Repeat("a", 20000)},
			Annotations: map[string]any{
				"Dependencies":          map[string]string{"dep": strings.Repeat("d", 20000)},
				"Environment Variables": map[string]string{"ENV": strings.Repeat("e", 20000)},
				"Error Chain":           append([]errorChainLink(nil), chain...),
				"extra":                 strings.Repeat("x", 20000),
			},
			Threads: map[string]Thread{
				"0": {Name: "main", Fault: true, Stacks: []StackFrame{{FuncName: "f", SourceCodeID: "0"}}},
				"1": {Name: "other", Stacks: []StackFrame{{FuncName: "g", SourceCodeID: "1"}}},
			},
			MainThread: "0",
			SourceCode: map[string]SourceCode{
				"0": {Path: "/a.go", Text: strings.Repeat("s", 20000), StartLine: 1, TabWidth: 8},
				"1": {Path: "/b.go"},
			},
		}
	}

	r := build()
	body, steps, err := shrinkReport(r, 1<<20)
	if err != nil || body == nil || strings.Join(steps, ",") != "source_text" {
		t.Fatalf("first step: body=%v steps=%v err=%v", body != nil, steps, err)
	}
	if r.SourceCode["0"] != (SourceCode{Path: "/a.go"}) {
		t.Errorf("source text kept: %+v", r.SourceCode["0"])
	}
	if r.Attributes[truncatedAttribute] != "source_text" {
		t.Errorf("%s = %v", truncatedAttribute, r.Attributes[truncatedAttribute])
	}

	r = build()
	body, steps, err = shrinkReport(r, 20000)
	if err != nil || body == nil || len(body) > 20000 {
		t.Fatalf("full trim: body=%d steps=%v err=%v", len(body), steps, err)
	}
	if got := strings.Join(steps, ","); got != "source_text,threads,dependencies_env,error_chain,long_attributes,annotations" {
		t.Errorf("steps = %s", got)
	}
	if _, ok := r.Threads["1"]; ok {
		t.Error("non-faulting thread kept")
	}
	if _, ok := r.SourceCode["1"]; ok {
		t.Error("source entry of a dropped thread kept")
	}
	if len(r.Annotations) != 0 {
		t.Errorf("annotations kept: %v", r.Annotations)
	}
	if long, _ := r.Attributes["long"].(string); len(long) != maxTrimmedAttributeBytes {
		t.Errorf("long attribute = %d bytes", len(long))
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if attrsOf(t, wire)[truncatedAttribute] != "source_text,threads,dependencies_env,error_chain,long_attributes,annotations" {
		t.Errorf("wire %s = %v", truncatedAttribute, attrsOf(t, wire)[truncatedAttribute])
	}

	r = build()
	r.Annotations = map[string]any{"Error Chain": append([]errorChainLink(nil), chain...)}
	r.Attributes = map[string]any{}
	r.SourceCode = nil
	delete(r.Threads, "1")
	full, _ := json.Marshal(r.toWire())
	body, steps, _ = shrinkReport(r, len(full)-1)
	if body == nil || strings.Join(steps, ",") != "error_chain" {
		t.Errorf("chain trim: steps=%v", steps)
	}
	if got, _ := r.Annotations["Error Chain"].([]errorChainLink); len(got) != maxTrimmedErrorChain {
		t.Errorf("error chain = %d nodes, want %d", len(got), maxTrimmedErrorChain)
	}

	r = build()
	if body, _, err := shrinkReport(r, 10); body != nil || err != nil {
		t.Errorf("impossible limit: body=%v err=%v", body != nil, err)
	}
}

func TestEnsureMainThread(t *testing.T) {
	threads, _, mainThread := buildThreads(nil, sourceOptions{mode: SourceCodeMetadata})
	r := &ReportData{Threads: threads, MainThread: mainThread}
	ensureMainThread(r)
	if r.MainThread != "main" {
		t.Errorf("MainThread = %q, want main", r.MainThread)
	}
	if th, ok := r.Threads["main"]; !ok || th.Stacks == nil {
		t.Errorf("main thread = %+v, %v", th, ok)
	}

	r = &ReportData{MainThread: "0", Threads: map[string]Thread{"0": {Name: "goroutine 1 [running]"}}}
	ensureMainThread(r)
	if len(r.Threads) != 1 || r.Threads["0"].Name != "goroutine 1 [running]" {
		t.Errorf("existing main thread replaced: %+v", r.Threads)
	}
}
