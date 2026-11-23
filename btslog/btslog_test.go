package btslog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/slogtest"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
	"github.com/backtrace-labs/backtrace-go/btslog"
	"github.com/backtrace-labs/backtrace-go/internal/fakebacktrace"
)

func TestMain(m *testing.M) {
	os.Unsetenv("BACKTRACE_ENDPOINT")
	os.Exit(m.Run())
}

func newClient(t *testing.T, mutate func(*bt.Config)) (*bt.Client, *fakebacktrace.Server) {
	t.Helper()
	srv := fakebacktrace.Start()
	t.Cleanup(srv.Close)
	cfg := bt.Config{Endpoint: srv.Endpoint, SourceCode: bt.SourceCodeNone}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := bt.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)
	return c, srv
}

func reports(t *testing.T, c *bt.Client, srv *fakebacktrace.Server) []fakebacktrace.Report {
	t.Helper()
	if !c.Flush(5 * time.Second) {
		t.Fatalf("Flush timed out (stats %+v)", c.Stats())
	}
	if s := c.Stats(); s.ServerReject != 0 {
		t.Fatalf("server rejected %d reports (stats %+v)", s.ServerReject, s)
	}
	return srv.Reports()
}

func onlyReport(t *testing.T, c *bt.Client, srv *fakebacktrace.Server) fakebacktrace.Report {
	t.Helper()
	all := reports(t, c, srv)
	if len(all) != 1 {
		t.Fatalf("got %d reports, want 1 (stats %+v)", len(all), c.Stats())
	}
	return all[0]
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("next wrote %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

type chargeError struct{ code int }

func (e *chargeError) Error() string { return fmt.Sprintf("charge declined (%d)", e.code) }

func TestHandlerConformsToSlogtest(t *testing.T) {
	c, srv := newClient(t, nil)
	cases := map[string]btslog.Options{
		"defaults":         {Client: c},
		"every record":     {Client: c, ReportLevel: slog.LevelDebug},
		"breadcrumbs only": {Client: c, BreadcrumbLevel: slog.LevelDebug, ReportLevel: slog.Level(100)},
		"no client":        {},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			var buf syncBuffer
			h := btslog.NewHandler(slog.NewJSONHandler(&buf, nil), opts)
			if err := slogtest.TestHandler(h, func() []map[string]any { return buf.lines(t) }); err != nil {
				t.Error(err)
			}
		})
	}
	if got := reports(t, c, srv); len(got) == 0 {
		t.Error("the every-record case sent no reports")
	}
}

func TestErrorRecordIsReported(t *testing.T) {
	c, srv := newClient(t, nil)
	var buf syncBuffer
	logger := slog.New(btslog.NewHandler(slog.NewJSONHandler(&buf, nil), btslog.Options{Client: c}))

	cause := &chargeError{code: 51}
	logger.With("service", "billing").WithGroup("charge").Error("charge failed",
		"err", fmt.Errorf("charging order: %w", cause),
		"order", 42,
		slog.Group("card", "brand", "visa", slog.Group("", "last4", "4242")),
		"retry", errors.New("gave up after 3 tries"),
		"refund", -5,
		"timeout", 1500*time.Millisecond,
		"items", map[string]int{"book": 2},
		slog.Int("", 7),
		slog.Group("empty"),
	)

	rep := onlyReport(t, c, srv)
	want := map[string]any{
		"error.message":     "charging order: charge declined (51)",
		"error.type":        "Exception",
		"log.message":       "charge failed",
		"service":           "billing",
		"charge.order":      float64(42),
		"charge.card.brand": "visa",
		"charge.card.last4": "4242",
		"charge.retry":      "gave up after 3 tries",
		"charge.refund":     "-5",
		"charge.timeout":    "1.5s",
	}
	for k, v := range want {
		if rep.Attributes[k] != v {
			t.Errorf("attribute %q = %#v, want %#v", k, rep.Attributes[k], v)
		}
	}
	for _, k := range []string{"charge.err", "err", "charge.items", "", "charge.", "charge.empty"} {
		if v, ok := rep.Attributes[k]; ok {
			t.Errorf("attribute %q = %#v, want none", k, v)
		}
	}
	if items, _ := rep.Annotations["charge.items"].(map[string]any); items["book"] != float64(2) {
		t.Errorf("annotation charge.items = %#v, want {book: 2}", rep.Annotations["charge.items"])
	}
	if len(rep.Classifiers) != 1 || rep.Classifiers[0] != "*btslog_test.chargeError" {
		t.Errorf("classifiers = %v, want [*btslog_test.chargeError]", rep.Classifiers)
	}

	lines := buf.lines(t)
	if len(lines) != 1 {
		t.Fatalf("next got %d records, want 1", len(lines))
	}
	charge, _ := lines[0]["charge"].(map[string]any)
	if lines[0]["msg"] != "charge failed" || lines[0]["level"] != "ERROR" || lines[0]["service"] != "billing" ||
		charge["err"] != "charging order: charge declined (51)" || charge["order"] != float64(42) {
		t.Errorf("next got %v, want the record unchanged", lines[0])
	}
}

func TestReportedErrorChoice(t *testing.T) {
	first, second, third := errors.New("first"), errors.New("second"), errors.New("third")
	cases := []struct {
		name    string
		log     func(*slog.Logger)
		message string
		attrs   map[string]any
	}{
		{
			name:    "err key preferred",
			log:     func(l *slog.Logger) { l.Error("m", "cause", first, "err", second, "error", third) },
			message: "second",
			attrs:   map[string]any{"cause": "first", "error": "third"},
		},
		{
			name:    "error key preferred",
			log:     func(l *slog.Logger) { l.Error("m", "cause", first, "error", second) },
			message: "second",
			attrs:   map[string]any{"cause": "first"},
		},
		{
			name:    "first error without a preferred key",
			log:     func(l *slog.Logger) { l.Error("m", "n", 1, "tags", []string{"a"}, "cause", first, "other", second) },
			message: "first",
			attrs:   map[string]any{"n": float64(1), "other": "second"},
		},
		{
			name:    "preferred key inside a group",
			log:     func(l *slog.Logger) { l.Error("m", "cause", first, slog.Group("db", "err", second)) },
			message: "second",
			attrs:   map[string]any{"cause": "first"},
		},
		{
			name:    "WithAttrs error comes first",
			log:     func(l *slog.Logger) { l.With("err", first).Error("m", "err", second) },
			message: "first",
			attrs:   map[string]any{"err": "second"},
		},
		{
			name:    "error that implements LogValuer",
			log:     func(l *slog.Logger) { l.Error("m", "n", 1, "err", declineError{"insufficient_funds"}) },
			message: "card declined: insufficient_funds",
			attrs:   map[string]any{"n": float64(1), "err.code": "insufficient_funds"},
		},
		{
			name:    "LogValuer error with a plain logged value",
			log:     func(l *slog.Logger) { l.Error("m", "err", textError{}) },
			message: "lookup failed (error text)",
		},
		{
			name:    "LogValuer error not reported keeps its logged value",
			log:     func(l *slog.Logger) { l.Error("m", "cause", textError{}, "err", first) },
			message: "first",
			attrs:   map[string]any{"cause": "lookup failed (logged)"},
		},
		{
			name:    "non-error value under err key",
			log:     func(l *slog.Logger) { l.Error("m", "err", "text", "cause", first) },
			message: "first",
			attrs:   map[string]any{"err": "text"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, srv := newClient(t, nil)
			tc.log(slog.New(btslog.NewHandler(nil, btslog.Options{Client: c})))
			rep := onlyReport(t, c, srv)
			if rep.Attributes["error.message"] != tc.message || rep.Attributes["log.message"] != "m" {
				t.Errorf("error.message = %v, log.message = %v, want %q and \"m\"",
					rep.Attributes["error.message"], rep.Attributes["log.message"], tc.message)
			}
			if _, want := tc.attrs["err"]; !want {
				if v, ok := rep.Attributes["err"]; ok {
					t.Errorf("attribute err = %#v, want none: it is the reported error", v)
				}
			}
			for k, v := range tc.attrs {
				if rep.Attributes[k] != v {
					t.Errorf("attribute %q = %#v, want %#v", k, rep.Attributes[k], v)
				}
			}
		})
	}
}

type declineError struct{ code string }

func (e declineError) Error() string { return "card declined: " + e.code }

func (e declineError) LogValue() slog.Value { return slog.GroupValue(slog.String("code", e.code)) }

type textError struct{}

func (textError) Error() string { return "lookup failed (error text)" }

func (textError) LogValue() slog.Value { return slog.StringValue("lookup failed (logged)") }

func TestLogValuerErrorIsReportedAsTheError(t *testing.T) {
	c, srv := newClient(t, nil)
	logger := slog.New(btslog.NewHandler(nil, btslog.Options{Client: c}))
	logger.Info("charging", "err", declineError{"retry"})
	logger.Error("charge failed", "err", declineError{"insufficient_funds"})

	rep := onlyReport(t, c, srv)
	if len(rep.Classifiers) != 1 || rep.Classifiers[0] != "btslog_test.declineError" {
		t.Errorf("classifiers = %v, want [btslog_test.declineError]", rep.Classifiers)
	}
	assertBreadcrumbs(t, rep.Breadcrumbs, []bt.Breadcrumb{{Level: bt.BreadcrumbInfo, Type: bt.BreadcrumbTypeLog,
		Message: "charging", Attributes: map[string]any{"err.code": "retry"}}})
}

func TestMessageRecordIsReported(t *testing.T) {
	c, srv := newClient(t, nil)
	slog.New(btslog.NewHandler(nil, btslog.Options{Client: c})).Error("disk almost full", "free_mb", 12)

	rep := onlyReport(t, c, srv)
	if rep.Attributes["error.message"] != "disk almost full" || rep.Attributes["error.type"] != "Message" ||
		rep.Attributes["free_mb"] != float64(12) {
		t.Errorf("attributes = %v", rep.Attributes)
	}
	if _, ok := rep.Attributes["log.message"]; ok {
		t.Errorf("log.message = %v, want none on a message report", rep.Attributes["log.message"])
	}
	if len(rep.Classifiers) != 1 || rep.Classifiers[0] != "message" {
		t.Errorf("classifiers = %v, want [message]", rep.Classifiers)
	}
}

func TestLevelsChooseReportOrBreadcrumb(t *testing.T) {
	c, srv := newClient(t, nil)
	var buf syncBuffer
	logger := slog.New(btslog.NewHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}), btslog.Options{Client: c}))

	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	logRecord := func(level slog.Level, msg string, args ...any) {
		r := slog.NewRecord(at, level, msg, 0)
		r.Add(args...)
		if err := logger.Handler().Handle(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	logRecord(slog.LevelDebug, "debug line")
	logRecord(slog.LevelInfo, "info line", "step", 1)
	logRecord(slog.LevelWarn, "warn line", slog.Group("db", "rows", 3))
	logRecord(slog.LevelError, "error line")
	c.ReportMessage("trail", nil)

	all := reports(t, c, srv)
	if len(all) != 2 || all[0].Attributes["error.message"] != "error line" {
		t.Fatalf("got %d reports, want the error record and the trail", len(all))
	}
	want := []bt.Breadcrumb{
		{Timestamp: at.UnixMilli(), Level: bt.BreadcrumbInfo, Type: bt.BreadcrumbTypeLog, Message: "info line",
			Attributes: map[string]any{"step": float64(1)}},
		{Timestamp: at.UnixMilli(), Level: bt.BreadcrumbWarning, Type: bt.BreadcrumbTypeLog, Message: "warn line",
			Attributes: map[string]any{"db.rows": float64(3)}},
	}
	for _, rep := range all {
		assertBreadcrumbs(t, rep.Breadcrumbs, want)
	}
	if got := len(buf.lines(t)); got != 4 {
		t.Errorf("next got %d records, want all 4", got)
	}
}

func TestBreadcrumbLevelMapping(t *testing.T) {
	c, srv := newClient(t, nil)
	h := btslog.NewHandler(nil, btslog.Options{
		Client:          c,
		BreadcrumbLevel: slog.LevelDebug - 4,
		ReportLevel:     slog.LevelError + 8,
	})
	for _, l := range []slog.Level{slog.LevelDebug - 4, slog.LevelDebug, slog.LevelInfo - 1, slog.LevelInfo, slog.LevelWarn - 1,
		slog.LevelWarn, slog.LevelError - 1, slog.LevelError, slog.LevelError + 4} {
		if err := h.Handle(context.Background(), slog.NewRecord(time.Time{}, l, l.String(), 0)); err != nil {
			t.Fatal(err)
		}
	}
	c.ReportMessage("trail", nil)

	rep := onlyReport(t, c, srv)
	want := map[string]string{
		"DEBUG-4": "debug", "DEBUG": "debug", "DEBUG+3": "debug", "INFO": "info", "INFO+3": "info",
		"WARN": "warning", "WARN+3": "warning", "ERROR": "error", "ERROR+4": "error",
	}
	if len(rep.Breadcrumbs) != len(want) {
		t.Fatalf("got %d breadcrumbs, want %d: %+v", len(rep.Breadcrumbs), len(want), rep.Breadcrumbs)
	}
	for _, b := range rep.Breadcrumbs {
		if b.Level != want[b.Message] || b.Type != bt.BreadcrumbTypeLog || b.Timestamp == 0 {
			t.Errorf("breadcrumb %q: level %q type %q timestamp %d, want level %q type log and a timestamp",
				b.Message, b.Level, b.Type, b.Timestamp, want[b.Message])
		}
	}
}

func TestLevelVarChangesLevelsAtRunTime(t *testing.T) {
	c, srv := newClient(t, nil)
	var reportLevel slog.LevelVar
	reportLevel.Set(slog.LevelError)
	logger := slog.New(btslog.NewHandler(nil, btslog.Options{Client: c, ReportLevel: &reportLevel}))

	logger.Warn("warn before")
	reportLevel.Set(slog.LevelWarn)
	logger.Warn("warn after")

	rep := onlyReport(t, c, srv)
	if rep.Attributes["error.message"] != "warn after" {
		t.Errorf("reported %v, want \"warn after\"", rep.Attributes["error.message"])
	}
	assertBreadcrumbs(t, rep.Breadcrumbs, []bt.Breadcrumb{{Level: bt.BreadcrumbWarning, Type: bt.BreadcrumbTypeLog, Message: "warn before"}})
}

func TestBreadcrumbsGoToContextScope(t *testing.T) {
	c, srv := newClient(t, nil)
	logger := slog.New(btslog.NewHandler(nil, btslog.Options{Client: c}))

	ctx := bt.WithScope(context.Background())
	bt.ScopeFromContext(ctx).SetAttribute("request.id", "r-1")
	logger.Info("process-wide")
	logger.InfoContext(ctx, "in request", "user", "u-7")
	logger.ErrorContext(ctx, "request failed", "err", errors.New("timeout"))
	logger.Error("background failure")

	all := reports(t, c, srv)
	if len(all) != 2 {
		t.Fatalf("got %d reports, want 2", len(all))
	}
	inRequest, background := all[0], all[1]
	if inRequest.Attributes["error.message"] != "timeout" || inRequest.Attributes["request.id"] != "r-1" {
		t.Errorf("request report attributes = %v, want the scope's request.id", inRequest.Attributes)
	}
	assertBreadcrumbs(t, inRequest.Breadcrumbs, []bt.Breadcrumb{
		{Level: bt.BreadcrumbInfo, Type: bt.BreadcrumbTypeLog, Message: "process-wide"},
		{Level: bt.BreadcrumbInfo, Type: bt.BreadcrumbTypeLog, Message: "in request", Attributes: map[string]any{"user": "u-7"}},
	})
	if _, ok := background.Attributes["request.id"]; ok || background.Attributes["error.message"] != "background failure" {
		t.Errorf("background report attributes = %v, want no scope data", background.Attributes)
	}
	assertBreadcrumbs(t, background.Breadcrumbs, []bt.Breadcrumb{{Level: bt.BreadcrumbInfo, Type: bt.BreadcrumbTypeLog, Message: "process-wide"}})
}

func assertBreadcrumbs(t *testing.T, got, want []bt.Breadcrumb) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d breadcrumbs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if w.Timestamp != 0 && g.Timestamp != w.Timestamp {
			t.Errorf("breadcrumb %d timestamp = %d, want %d", i, g.Timestamp, w.Timestamp)
		}
		if g.Level != w.Level || g.Type != w.Type || g.Message != w.Message || len(g.Attributes) != len(w.Attributes) {
			t.Errorf("breadcrumb %d = %+v, want %+v", i, g, w)
			continue
		}
		for k, v := range w.Attributes {
			if g.Attributes[k] != v {
				t.Errorf("breadcrumb %d attribute %q = %#v, want %#v", i, k, g.Attributes[k], v)
			}
		}
	}
}

func TestEnabledAndNext(t *testing.T) {
	c, srv := newClient(t, nil)
	var buf syncBuffer
	h := btslog.NewHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}), btslog.Options{Client: c})
	ctx := context.Background()
	for level, want := range map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: true, slog.LevelWarn: true, slog.LevelError: true} {
		if got := h.Enabled(ctx, level); got != want {
			t.Errorf("Enabled(%v) = %v, want %v", level, got, want)
		}
	}
	logger := slog.New(h)
	logger.Debug("dropped")
	logger.Info("breadcrumb only")
	logger.Warn("both")
	c.ReportMessage("trail", nil)

	lines := buf.lines(t)
	if len(lines) != 1 || lines[0]["msg"] != "both" {
		t.Errorf("next got %v, want only the WARN record", lines)
	}
	rep := onlyReport(t, c, srv)
	assertBreadcrumbs(t, rep.Breadcrumbs, []bt.Breadcrumb{
		{Level: bt.BreadcrumbInfo, Type: bt.BreadcrumbTypeLog, Message: "breadcrumb only"},
		{Level: bt.BreadcrumbWarning, Type: bt.BreadcrumbTypeLog, Message: "both"},
	})

	discard := btslog.NewHandler(nil, btslog.Options{Client: c, BreadcrumbLevel: slog.LevelWarn})
	if discard.Enabled(ctx, slog.LevelInfo) || !discard.Enabled(ctx, slog.LevelWarn) {
		t.Error("with a nil next, Enabled must follow BreadcrumbLevel and ReportLevel only")
	}
	if err := discard.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelWarn, "x", 0)); err != nil {
		t.Errorf("Handle with a nil next = %v", err)
	}
	if discard.WithAttrs(nil) != discard || discard.WithGroup("") != discard {
		t.Error("empty WithAttrs and WithGroup must return the handler itself")
	}
}

type failingHandler struct{ slog.Handler }

var errWrite = errors.New("write failed")

func (failingHandler) Handle(context.Context, slog.Record) error { return errWrite }

func TestHandleReturnsNextError(t *testing.T) {
	c, srv := newClient(t, nil)
	h := btslog.NewHandler(failingHandler{slog.NewJSONHandler(io.Discard, nil)}, btslog.Options{Client: c})
	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelError, "boom", 0)); !errors.Is(err, errWrite) {
		t.Errorf("Handle = %v, want the next handler's error", err)
	}
	if got := len(reports(t, c, srv)); got != 1 {
		t.Errorf("got %d reports, want 1", got)
	}
}

func TestDerivedHandlersDoNotShareAttrs(t *testing.T) {
	c, srv := newClient(t, nil)
	base := slog.New(btslog.NewHandler(nil, btslog.Options{Client: c})).With("base", 1)
	left := base.With("side", "left")
	right := base.With("side", "right").WithGroup("g")

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(2)
		go func() { defer wg.Done(); left.Error("left", "i", i) }()
		go func() { defer wg.Done(); right.Error("right", "i", i) }()
	}
	wg.Wait()

	all := reports(t, c, srv)
	if len(all) != 40 {
		t.Fatalf("got %d reports, want 40", len(all))
	}
	for _, rep := range all {
		a := rep.Attributes
		switch a["error.message"] {
		case "left":
			if a["side"] != "left" || a["base"] != float64(1) || a["i"] == nil || a["g.i"] != nil {
				t.Errorf("left report attributes = %v", a)
			}
		case "right":
			if a["side"] != "right" || a["base"] != float64(1) || a["g.i"] == nil || a["i"] != nil {
				t.Errorf("right report attributes = %v", a)
			}
		default:
			t.Errorf("unexpected report %v", a["error.message"])
		}
	}
}

type panickingLeveler struct{}

func (panickingLeveler) Level() slog.Level { panic("level") }

type panickingValueContext struct{ context.Context }

func (panickingValueContext) Value(any) any { panic("value") }

type cyclicValuer struct{}

func (c cyclicValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("leaf", "x"), slog.Any("next", c))
}

func TestCyclicGroupValueIsCut(t *testing.T) {
	c, srv := newClient(t, nil)
	slog.New(btslog.NewHandler(nil, btslog.Options{Client: c})).Error("cyclic", "tree", cyclicValuer{})

	rep := onlyReport(t, c, srv)
	key := "tree"
	for range 32 {
		if rep.Attributes[key+".leaf"] != "x" {
			t.Fatalf("attribute %q = %#v, want \"x\"", key+".leaf", rep.Attributes[key+".leaf"])
		}
		key += ".next"
	}
	if v, ok := rep.Attributes[key+".leaf"]; ok {
		t.Errorf("attribute %q = %#v, want groups beyond 32 levels left out", key+".leaf", v)
	}
}

type panickingValuer struct{}

func (panickingValuer) LogValue() slog.Value { panic("log value") }

type panickingMarshaler struct{}

func (panickingMarshaler) MarshalJSON() ([]byte, error) { panic("marshal") }

type panickingError struct{}

func (*panickingError) Error() string { panic("error") }

func TestHandlerWorkIsPanicContained(t *testing.T) {
	c, srv := newClient(t, nil)
	var buf syncBuffer
	next := slog.NewJSONHandler(&buf, nil)

	broken := btslog.NewHandler(next, btslog.Options{Client: c, ReportLevel: panickingLeveler{}, BreadcrumbLevel: panickingLeveler{}})
	if broken.Enabled(context.Background(), slog.LevelInfo) != true {
		t.Error("Enabled must fall back to next when the Levelers panic")
	}
	slog.New(broken).Error("leveler panics")

	h := btslog.NewHandler(next, btslog.Options{Client: c})
	logger := slog.New(h)
	logger.ErrorContext(panickingValueContext{context.Background()}, "context panics")
	logger.InfoContext(panickingValueContext{context.Background()}, "context panics on a breadcrumb")
	logger.Error("values panic", "err", errors.New("reported"), "valuer", panickingValuer{}, "marshaler", panickingMarshaler{})
	var typedNil *panickingError
	logger.Error("error panics", "err", typedNil)
	logger.Error("Error() panics", "err", &panickingError{})

	if got := len(buf.lines(t)); got != 6 {
		t.Errorf("next got %d records, want 6", got)
	}
	all := reports(t, c, srv)
	if len(all) != 3 {
		t.Fatalf("got %d reports, want 3 (stats %+v)", len(all), c.Stats())
	}
	if msg, _ := all[0].Attributes["valuer"].(string); !strings.Contains(msg, "LogValue panicked") {
		t.Errorf("valuer attribute = %#v, want slog's LogValue panic error", all[0].Attributes["valuer"])
	}
	if msg, _ := all[0].Attributes["marshaler"].(string); !strings.Contains(msg, "MarshalJSON failed") {
		t.Errorf("marshaler attribute = %#v, want the SDK's MarshalJSON failure marker", all[0].Attributes["marshaler"])
	}
	for _, rep := range all[1:] {
		if msg, _ := rep.Attributes["error.message"].(string); !strings.Contains(msg, "Error() panicked") {
			t.Errorf("error.message = %q, want the SDK placeholder", msg)
		}
	}
}

func TestNilClientUsesDefaultClient(t *testing.T) {
	logger := slog.New(btslog.NewHandler(nil, btslog.Options{}))
	logger.Error("unconfigured")
	logger.Info("unconfigured breadcrumb")

	c, srv := newClient(t, nil)
	bt.SetDefaultClient(c)
	t.Cleanup(func() { bt.SetDefaultClient(nil) })
	logger.Info("default breadcrumb")
	logger.Error("default report")

	rep := onlyReport(t, c, srv)
	if rep.Attributes["error.message"] != "default report" {
		t.Errorf("reported %v, want \"default report\"", rep.Attributes["error.message"])
	}
	assertBreadcrumbs(t, rep.Breadcrumbs, []bt.Breadcrumb{{Level: bt.BreadcrumbInfo, Type: bt.BreadcrumbTypeLog, Message: "default breadcrumb"}})
}

func TestSDKDiagnosticsDoNotRecurse(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelError, slog.LevelInfo} {
		t.Run(level.String(), func(t *testing.T) {
			var buf syncBuffer
			h := btslog.NewHandler(slog.NewJSONHandler(&buf, nil), btslog.Options{})
			c, srv := newClient(t, func(cfg *bt.Config) {
				cfg.Debug = true
				cfg.Logger = slog.NewLogLogger(h, level)
			})
			bt.SetDefaultClient(c)
			t.Cleanup(func() { bt.SetDefaultClient(nil) })

			logger := slog.New(h)
			logger.Error("first", "err", errors.New("first failure"))
			reports(t, c, srv)
			logger.Error("second")
			reports(t, c, srv)

			all := reports(t, c, srv)
			if len(all) != 2 || c.Stats().Accepted != 2 {
				t.Fatalf("got %d reports, %d accepted, want 2: SDK diagnostics were reported", len(all), c.Stats().Accepted)
			}
			if crumbs := all[1].Breadcrumbs; len(crumbs) != 0 {
				t.Errorf("second report breadcrumbs = %+v, want none from SDK diagnostics", crumbs)
			}
			if out := buf.String(); !strings.Contains(out, "sending report") || !strings.Contains(out, "delivered") {
				t.Errorf("next did not get the SDK diagnostics: %s", out)
			}
		})
	}
}

func TestLegacyDiagnosticsDoNotRecurse(t *testing.T) {
	saved := bt.Options
	t.Cleanup(func() { bt.Options = saved })

	var buf syncBuffer
	h := btslog.NewHandler(slog.NewJSONHandler(&buf, nil), btslog.Options{})
	shared := slog.NewLogLogger(h, slog.LevelInfo)
	bt.Options.DebugBacktrace = true
	bt.Options.Logger = shared

	returns(t, func() { shared.Print("hello") })

	eventually(t, func() bool { return strings.Contains(buf.String(), "reporting disabled") })
	out := buf.String()
	if strings.Count(out, "hello") != 1 || strings.Count(out, "reporting disabled") != 1 {
		t.Errorf("next got %s, want the record and one reporting-disabled diagnostic", out)
	}
}

func TestSharedLogLoggerDoesNotDeadlock(t *testing.T) {
	cases := map[string]func(*bt.Config){
		"closed client": nil,
		"sampled out":   func(cfg *bt.Config) { cfg.SampleRate = 1e-12 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var buf syncBuffer
			h := btslog.NewHandler(slog.NewJSONHandler(&buf, nil), btslog.Options{})
			shared := slog.NewLogLogger(h, slog.LevelError)
			c, _ := newClient(t, func(cfg *bt.Config) {
				cfg.Debug = true
				cfg.Logger = shared
				if mutate != nil {
					mutate(cfg)
				}
			})
			bt.SetDefaultClient(c)
			t.Cleanup(func() { bt.SetDefaultClient(nil) })
			if mutate == nil {
				c.Close()
			}

			returns(t, func() { shared.Printf("through the shared logger") })
			want := "client closed"
			if mutate != nil {
				want = "sampled out"
			}
			eventually(t, func() bool { return strings.Contains(buf.String(), want) })
			if got := strings.Count(buf.String(), want); got != 1 {
				t.Errorf("logged %d %q diagnostics, want 1: %s", got, want, buf.String())
			}
			if got := strings.Count(buf.String(), "through the shared logger"); got != 1 {
				t.Errorf("next got the record %d times, want 1: %s", got, buf.String())
			}
		})
	}
}

func returns(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return")
	}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}
