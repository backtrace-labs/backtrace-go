package bt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, mutate func(*Config)) (*Client, *recordingServer) {
	t.Helper()
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)

	cfg := Config{
		Endpoint: rs.srv.URL,
		Token:    "client-test-token",
		Timeout:  5 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)
	return c, rs
}

func TestNewClientValidation(t *testing.T) {
	// Neutralize any BACKTRACE_* variables from the outer environment.
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")

	if _, err := NewClient(Config{}); err == nil {
		t.Error("expected error for missing endpoint")
	}
	if _, err := NewClient(Config{Endpoint: "ftp://example.com"}); err == nil {
		t.Error("expected error for non-http scheme")
	}
	if _, err := NewClient(Config{Endpoint: "http://example.com"}); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestClientReportDelivery(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		// Source text is opt-in; this test asserts context snippets.
		cfg.SourceCode = SourceCodeContext
	})

	c.Report(errors.New("client error"), map[string]interface{}{"who": "client"})
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	if rs.count() != 1 {
		t.Fatalf("got %d reports, want 1", rs.count())
	}
	attrs := attrsOf(t, rs.last())
	if attrs["error.message"] != "client error" || attrs["who"] != "client" {
		t.Errorf("unexpected attributes: %v", attrs)
	}

	// Context-mode source: this test file's frames are SDK-prefix-filtered
	// (the test lives in the SDK package), so the surviving frames come
	// from the testing package and snippets are read from GOROOT sources.
	sources, _ := rs.last()["sourceCode"].(map[string]interface{})
	foundSnippet := false
	for _, v := range sources {
		sc, _ := v.(map[string]interface{})
		if sc == nil {
			continue
		}
		if path, _ := sc["path"].(string); strings.HasSuffix(path, "client_test.go") {
			t.Errorf("SDK-package frame not filtered: %q", path)
		}
		if text, _ := sc["text"].(string); text != "" {
			if start, _ := sc["startLine"].(float64); start >= 1 {
				foundSnippet = true
			}
			if len(text) > 1<<16 {
				t.Errorf("context snippet suspiciously large (%d bytes): whole file embedded?", len(text))
			}
		}
	}
	if !foundSnippet {
		t.Error("no source context snippet found in payload")
	}
}

func TestTwoClientsCoexist(t *testing.T) {
	c1, rs1 := newTestClient(t, nil)
	c2, rs2 := newTestClient(t, nil)

	c1.ReportMessage("to first", nil)
	c2.ReportMessage("to second", nil)
	c1.Flush(5 * time.Second)
	c2.Flush(5 * time.Second)

	if rs1.count() != 1 || rs2.count() != 1 {
		t.Fatalf("cross-talk between clients: rs1=%d rs2=%d", rs1.count(), rs2.count())
	}
}

func TestQueueOverflowDoesNotBlockCaller(t *testing.T) {
	block := make(chan struct{})
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.QueueSize = 2
	})
	// Guarantee the handler is unblocked on every exit path — otherwise a
	// test failure would hang the deferred server Close.
	var once sync.Once
	unblock := func() { once.Do(func() { close(block) }) }
	t.Cleanup(unblock)

	rs.mu.Lock()
	rs.block = block
	rs.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			c.ReportMessage(fmt.Sprintf("burst %d", i), nil)
		}
	}()

	select {
	case <-done:
		// Callers returned immediately even though the transport is stuck.
	case <-time.After(3 * time.Second):
		t.Fatal("Report blocked the caller with a full queue")
	}

	if c.DroppedReports() == 0 {
		t.Error("expected dropped reports with a full queue")
	}

	rs.mu.Lock()
	rs.block = nil
	rs.mu.Unlock()
	unblock()
	c.Flush(5 * time.Second)
}

func TestFlushDoesNotStopWorker(t *testing.T) {
	c, rs := newTestClient(t, nil)

	c.ReportMessage("first", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("first Flush timed out")
	}
	c.ReportMessage("second", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("second Flush timed out")
	}
	if rs.count() != 2 {
		t.Fatalf("got %d reports, want 2", rs.count())
	}
}

func TestFlushTimesOutWhenTransportStuck(t *testing.T) {
	block := make(chan struct{})
	c, rs := newTestClient(t, nil)
	var once sync.Once
	unblock := func() { once.Do(func() { close(block) }) }
	t.Cleanup(unblock)

	rs.mu.Lock()
	rs.block = block
	rs.mu.Unlock()

	c.ReportMessage("stuck", nil)
	if c.Flush(100 * time.Millisecond) {
		t.Error("Flush reported success while transport is stuck")
	}
	unblock()
	c.Flush(5 * time.Second)
}

func TestCloseDrainsAndIsIdempotent(t *testing.T) {
	c, rs := newTestClient(t, nil)

	c.ReportMessage("before close", nil)
	c.Close()
	c.Close() // must not panic or deadlock

	if rs.count() != 1 {
		t.Fatalf("Close did not drain the queue: %d reports", rs.count())
	}

	dropped := c.DroppedReports()
	c.ReportMessage("after close", nil)
	if c.DroppedReports() != dropped+1 {
		t.Error("report after Close was not counted as dropped")
	}
}

func TestBeforeSendMutates(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.BeforeSend = func(r *ReportData) *ReportData {
			r.Attributes["scrubbed"] = true
			return r
		}
	})
	c.ReportMessage("hello", nil)
	c.Flush(5 * time.Second)

	if attrs := attrsOf(t, rs.last()); attrs["scrubbed"] != true {
		t.Errorf("BeforeSend mutation lost: %v", attrs["scrubbed"])
	}
}

func TestBeforeSendDrops(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.BeforeSend = func(r *ReportData) *ReportData { return nil }
	})
	c.ReportMessage("dropped", nil)
	c.Flush(5 * time.Second)

	if rs.count() != 0 {
		t.Fatalf("BeforeSend nil did not drop the report")
	}
}

// TestBeforeSendPanicDropsReport: a panicking hook must fail closed — the
// report may be half-scrubbed, so it is dropped and counted, never sent.
func TestBeforeSendPanicDropsReport(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.BeforeSend = func(r *ReportData) *ReportData {
			r.Attributes["half"] = "scrubbed"
			panic("hook bug")
		}
	})
	c.ReportMessage("must not leave the process", nil)
	c.Flush(5 * time.Second)

	if rs.count() != 0 {
		t.Fatalf("half-scrubbed report was sent despite BeforeSend panic: %d", rs.count())
	}
	if c.DroppedReports() != 1 {
		t.Errorf("dropped counter = %d, want 1", c.DroppedReports())
	}
}

func TestSampleRate(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.SampleRate = 0.000001
	})
	for i := 0; i < 50; i++ {
		c.ReportMessage("sampled", nil)
	}
	c.Flush(5 * time.Second)

	if rs.count() > 5 {
		t.Errorf("sampling ineffective: %d of 50 delivered at rate 1e-6", rs.count())
	}
}

func TestSampleRateZeroValueSendsEverything(t *testing.T) {
	cfg := Config{Endpoint: "http://example.com"}.normalize()
	if cfg.SampleRate != 1.0 {
		t.Errorf("zero-value SampleRate = %v, want 1.0", cfg.SampleRate)
	}
}

func TestErrorChainCapture(t *testing.T) {
	c, rs := newTestClient(t, nil)

	inner := errors.New("root cause")
	middle := fmt.Errorf("middle: %w", inner)
	outer := fmt.Errorf("outer: %w", middle)
	c.Report(outer, nil)
	c.Flush(5 * time.Second)

	attrs := attrsOf(t, rs.last())
	if attrs["error.type"] != "*fmt.wrapError" {
		t.Errorf("error.type = %v", attrs["error.type"])
	}
	annotations, _ := rs.last()["annotations"].(map[string]interface{})
	chain, _ := annotations["Error Chain"].([]interface{})
	if len(chain) != 3 {
		t.Fatalf("error chain length = %d, want 3", len(chain))
	}
	last, _ := chain[2].(map[string]interface{})
	if last["message"] != "root cause" {
		t.Errorf("chain tail = %v", last)
	}
}

func TestBreadcrumbsRingAndAnnotation(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.MaxBreadcrumbs = 8
	})
	for i := 0; i < 12; i++ {
		c.AddBreadcrumb(Breadcrumb{Message: fmt.Sprintf("crumb %d", i)})
	}
	c.ReportMessage("with crumbs", nil)
	c.Flush(5 * time.Second)

	annotations, _ := rs.last()["annotations"].(map[string]interface{})
	crumbs, _ := annotations["breadcrumbs"].([]interface{})
	if len(crumbs) != 8 {
		t.Fatalf("breadcrumb count = %d, want 8 (ring capacity)", len(crumbs))
	}
	first, _ := crumbs[0].(map[string]interface{})
	if first["message"] != "crumb 4" {
		t.Errorf("oldest breadcrumb = %v, want crumb 4 (eviction order)", first["message"])
	}
	if first["level"] != "info" || first["type"] != "manual" {
		t.Errorf("breadcrumb defaults not applied: %v", first)
	}

	// Breadcrumbs also ship as the bt-breadcrumbs-0 attachment the
	// Backtrace UI reads (same schema as the other Backtrace SDKs).
	atts := rs.lastAttachments()
	raw, ok := atts["attachment_bt-breadcrumbs-0"]
	if !ok {
		t.Fatalf("bt-breadcrumbs-0 attachment missing; parts: %v", atts)
	}
	var fileCrumbs []map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &fileCrumbs); err != nil {
		t.Fatalf("breadcrumb attachment is not a JSON array: %v", err)
	}
	if len(fileCrumbs) != 8 || fileCrumbs[0]["message"] != "crumb 4" {
		t.Errorf("breadcrumb attachment content wrong: %d entries, first %v",
			len(fileCrumbs), fileCrumbs[0])
	}
}

// TestPanicAndFlushRetriesFullQueue pins ReportPanicValueAndFlush: with the
// queue full it retries admission under its single deadline instead of
// dropping, then waits for delivery. Plain ReportPanicValue stays
// non-blocking.
func TestPanicAndFlushRetriesFullQueue(t *testing.T) {
	block := make(chan struct{})
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.QueueSize = 1
	})
	var once sync.Once
	unblock := func() { once.Do(func() { close(block) }) }
	t.Cleanup(unblock)

	rs.mu.Lock()
	rs.block = block
	rs.mu.Unlock()

	c.ReportMessage("occupies the worker", nil)
	select {
	case <-rs.entered: // worker is now stuck inside the handler
	case <-time.After(3 * time.Second):
		t.Fatal("worker never reached the transport")
	}
	c.ReportMessage("fills the queue", nil)

	// Plain ReportPanicValue must return immediately (drop + count).
	before := c.Stats().QueueFull
	c.ReportPanicValue("non-blocking panic", nil)
	if got := c.Stats().QueueFull; got != before+1 {
		t.Errorf("non-blocking panic with full queue: QueueFull = %d, want %d", got, before+1)
	}

	done := make(chan bool, 1)
	go func() {
		done <- c.ReportPanicValueAndFlush("panic while queue full", nil, 10*time.Second)
	}()

	select {
	case <-done:
		t.Fatal("ReportPanicValueAndFlush returned immediately: dropped instead of retrying")
	case <-time.After(100 * time.Millisecond):
		// Still retrying under its deadline, as intended.
	}

	unblock()
	select {
	case delivered := <-done:
		if !delivered {
			t.Error("ReportPanicValueAndFlush = false after queue freed within deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReportPanicValueAndFlush never returned after queue freed")
	}
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if rs.count() != 3 {
		t.Errorf("reports delivered = %d, want 3 (panic report lost?)", rs.count())
	}
}

// TestFlushSucceedsAfterQueueFullRetry pins Flush's retry loop: a queue that
// is full when Flush is called must not produce a false negative once it
// drains within the timeout.
func TestFlushSucceedsAfterQueueFullRetry(t *testing.T) {
	block := make(chan struct{})
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.QueueSize = 1
	})
	var once sync.Once
	unblock := func() { once.Do(func() { close(block) }) }
	t.Cleanup(unblock)

	rs.mu.Lock()
	rs.block = block
	rs.mu.Unlock()

	c.ReportMessage("occupies the worker", nil)
	select {
	case <-rs.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never reached the transport")
	}
	c.ReportMessage("fills the queue", nil)

	res := make(chan bool, 1)
	go func() { res <- c.Flush(10 * time.Second) }()
	time.Sleep(50 * time.Millisecond) // let Flush hit the queue-full retry path
	unblock()

	select {
	case ok := <-res:
		if !ok {
			t.Error("Flush returned false although the queue drained within the timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Flush stuck")
	}
}

func TestServerErrorCountsAsDropped(t *testing.T) {
	c, rs := newTestClient(t, nil)
	rs.mu.Lock()
	rs.status = http.StatusInternalServerError
	rs.mu.Unlock()

	c.ReportMessage("rejected", nil)
	c.Flush(5 * time.Second)

	if c.DroppedReports() != 1 {
		t.Errorf("5xx not counted as dropped: %d", c.DroppedReports())
	}
}

func TestRateLimit429PausesSubmissions(t *testing.T) {
	c, rs := newTestClient(t, nil)
	rs.mu.Lock()
	rs.status = http.StatusTooManyRequests
	rs.mu.Unlock()

	c.ReportMessage("first", nil)
	c.Flush(5 * time.Second)

	received := rs.count() // server saw the 429'd request
	c.ReportMessage("second", nil)
	c.Flush(5 * time.Second)

	if rs.count() != received {
		t.Error("submission was not paused after 429")
	}
	if c.DroppedReports() < 2 {
		t.Errorf("dropped counter = %d, want >= 2", c.DroppedReports())
	}
}

func TestClientConcurrencySafety(t *testing.T) {
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.QueueSize = 4
	})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				c.SetAttribute("g", g)
				c.AddBreadcrumb(Breadcrumb{Message: "b"})
				c.Report(errors.New("concurrent"), nil)
				c.Flush(10 * time.Millisecond)
			}
		}(g)
	}
	wg.Wait()
	c.Flush(5 * time.Second)
}

func TestAttachmentsMultipartSubmission(t *testing.T) {
	dir := t.TempDir()
	attachmentPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(attachmentPath, []byte("log line 1\nlog line 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Same basename in another directory: must arrive under a distinct
	// part name instead of overwriting the first attachment.
	dupDir := filepath.Join(dir, "dup")
	if err := os.MkdirAll(dupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dupPath := filepath.Join(dupDir, "app.log")
	if err := os.WriteFile(dupPath, []byte("other content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file whose NATURAL basename collides with a generated candidate:
	// uniquification must probe past it instead of overwriting.
	natPath := filepath.Join(dir, "app_1.log")
	if err := os.WriteFile(natPath, []byte("natural\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	type received struct {
		reportJSON  map[string]interface{}
		attachments map[string]string
	}
	got := make(chan received, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("expected multipart submission: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec := received{attachments: map[string]string{}}
		for field, headers := range r.MultipartForm.File {
			f, err := headers[0].Open()
			if err != nil {
				continue
			}
			content, _ := io.ReadAll(f)
			f.Close()
			if field == "upload_file" {
				payload := map[string]interface{}{}
				if json.Unmarshal(content, &payload) == nil {
					rec.reportJSON = payload
				}
			} else {
				rec.attachments[field] = string(content)
			}
		}
		got <- rec
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := NewClient(Config{
		Endpoint:        srv.URL,
		Token:           "attach-test",
		AttachmentPaths: []string{attachmentPath, natPath, dupPath, filepath.Join(dir, "missing.txt")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.ReportMessage("with attachment", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	select {
	case rec := <-got:
		if rec.reportJSON == nil {
			t.Fatal("upload_file part missing or not JSON")
		}
		if rec.reportJSON["lang"] != "go" {
			t.Errorf("report JSON lang = %v", rec.reportJSON["lang"])
		}
		if rec.attachments["attachment_app.log"] != "log line 1\nlog line 2\n" {
			t.Errorf("attachment content = %q", rec.attachments["attachment_app.log"])
		}
		if rec.attachments["attachment_app_1.log"] != "natural\n" {
			t.Errorf("natural basename lost its name: %v", rec.attachments)
		}
		if rec.attachments["attachment_app_2.log"] != "other content\n" {
			t.Errorf("duplicate basename not uniquified past natural collision: %v", rec.attachments)
		}
		if len(rec.attachments) != 3 {
			t.Errorf("attachments = %v (unreadable file not skipped, or parts collided)", rec.attachments)
		}
	case <-time.After(time.Second):
		t.Fatal("no submission received")
	}
}

type testPanicMethodError struct{}

func (*testPanicMethodError) Error() string { panic("Error panic") }

type testPanicStringer struct{}

func (testPanicStringer) String() string { panic("String panic") }

type testPanicLogger struct{}

func (testPanicLogger) Printf(string, ...interface{}) { panic("logger panic") }

func mustNotPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s let an application panic escape: %v", name, r)
		}
	}()
	f()
}

// TestPublicReportingContainsApplicationPanics pins the "never panic"
// contract at every boundary that executes caller-controlled code: error
// and stringer implementations, unwrap methods, and the diagnostic logger.
func TestPublicReportingContainsApplicationPanics(t *testing.T) {
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.Debug = true
		cfg.Logger = testPanicLogger{}
	})

	var typedNil *testPanicMethodError
	var err error = typedNil // non-nil interface, panicking Error()

	mustNotPanic(t, "ReportError", func() { c.ReportError(err, nil) })
	mustNotPanic(t, "Report", func() { c.Report(testPanicStringer{}, nil) })
	mustNotPanic(t, "ReportPanicValue", func() { c.ReportPanicValue(testPanicStringer{}, nil) })
	mustNotPanic(t, "ReportPanicValueAndFlush", func() {
		c.ReportPanicValueAndFlush(testPanicStringer{}, nil, 100*time.Millisecond)
	})
	mustNotPanic(t, "Flush", func() { _ = c.Flush(time.Second) })
}

// reentrantLogger forwards every diagnostic line back into the SDK — the
// logging-adapter pattern that historically recursed to a fatal stack
// overflow. depth tracks the maximum observed nesting.
type reentrantLogger struct {
	c     *Client
	depth atomic.Int32
	max   atomic.Int32
}

func (l *reentrantLogger) Printf(format string, v ...interface{}) {
	d := l.depth.Add(1)
	defer l.depth.Add(-1)
	if d > l.max.Load() {
		l.max.Store(d)
	}
	if d > 25 {
		// A guard failure would blow the stack long before this, but
		// bail out rather than crash the whole test binary.
		return
	}
	l.c.ReportMessage("from logger", nil)
}

// TestReentrantLoggerIsContained pins the diag re-entrancy guard: a Logger
// that reports back into the SDK must not recurse unboundedly on the
// closed-client or full-queue drop paths.
func TestReentrantLoggerIsContained(t *testing.T) {
	logger := &reentrantLogger{}
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.Debug = true
		cfg.Logger = logger
		cfg.QueueSize = 1
	})
	logger.c = c

	// Closed-client drop path: deterministic infinite recursion before
	// the guard existed.
	c.Close()
	c.ReportMessage("after close", nil)

	if got := logger.max.Load(); got > 2 {
		t.Errorf("re-entrant logger nested to depth %d; guard not effective", got)
	}
}

// TestSourceMetadataDefault pins the production-safe default: frames carry
// path/line metadata but no source text leaves the host.
func TestSourceMetadataDefault(t *testing.T) {
	c, rs := newTestClient(t, nil) // no SourceCode override

	c.ReportMessage("metadata only", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	sources, _ := rs.last()["sourceCode"].(map[string]interface{})
	if len(sources) == 0 {
		t.Fatal("metadata mode should still reference paths")
	}
	for id, v := range sources {
		sc, _ := v.(map[string]interface{})
		if sc == nil {
			continue
		}
		if text, _ := sc["text"].(string); text != "" {
			t.Errorf("source entry %s carries text in metadata mode", id)
		}
		if path, _ := sc["path"].(string); path == "" {
			t.Errorf("source entry %s missing path", id)
		}
	}
}

// TestFlushIsCaptureTimeBarrier pins the sequence-based flush: reports
// enqueued after Flush is called must not extend its wait.
func TestFlushIsCaptureTimeBarrier(t *testing.T) {
	block := make(chan struct{})
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.QueueSize = 64
	})
	var once sync.Once
	unblock := func() { once.Do(func() { close(block) }) }
	t.Cleanup(unblock)

	rs.mu.Lock()
	rs.block = block
	rs.mu.Unlock()

	c.ReportMessage("pre-flush", nil)
	select {
	case <-rs.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never reached the transport")
	}

	flushed := make(chan bool, 1)
	go func() { flushed <- c.Flush(10 * time.Second) }()

	// Concurrent producers keep pouring reports in AFTER the flush call.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					c.ReportMessage("post-flush noise", nil)
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond) // flush is now waiting on the barrier
	unblock()

	select {
	case ok := <-flushed:
		if !ok {
			t.Error("Flush = false although its pre-call backlog drained")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Flush starved by post-call producers (not a capture-time barrier)")
	}
	close(stop)
	wg.Wait()
	c.Flush(10 * time.Second)
}

// TestStatsBreakdown pins the reasoned discard counters.
func TestStatsBreakdown(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.BeforeSend = func(r *ReportData) *ReportData {
			if r.Attributes["drop.me"] == true {
				return nil
			}
			return r
		}
	})

	c.ReportMessage("delivered", nil)
	c.ReportMessage("hook-dropped", map[string]interface{}{"drop.me": true})
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	stats := c.Stats()
	if stats.Accepted != 2 {
		t.Errorf("Accepted = %d, want 2", stats.Accepted)
	}
	if stats.Delivered != 1 {
		t.Errorf("Delivered = %d, want 1", stats.Delivered)
	}
	if stats.BeforeSend != 1 {
		t.Errorf("BeforeSend drops = %d, want 1", stats.BeforeSend)
	}
	if rs.count() != 1 {
		t.Errorf("server received %d reports, want 1", rs.count())
	}

	// Server rejection is classified separately.
	rs.mu.Lock()
	rs.status = http.StatusInternalServerError
	rs.mu.Unlock()
	c.ReportMessage("rejected", nil)
	c.Flush(5 * time.Second)
	if got := c.Stats().ServerReject; got != 1 {
		t.Errorf("ServerReject = %d, want 1", got)
	}
	if c.DroppedReports() != c.Stats().BeforeSend+c.Stats().ServerReject {
		t.Errorf("DroppedReports = %d, want sum of reasons", c.DroppedReports())
	}
}

// TestCloseContextCancelsBlockedTransport pins bounded shutdown: a stuck
// transport cannot hold CloseContext past its deadline, and the SDK root
// context aborts the in-flight request.
func TestCloseContextCancelsBlockedTransport(t *testing.T) {
	block := make(chan struct{})
	c, rs := newTestClient(t, nil)
	var once sync.Once
	unblock := func() { once.Do(func() { close(block) }) }
	t.Cleanup(unblock)

	rs.mu.Lock()
	rs.block = block
	rs.mu.Unlock()

	c.ReportMessage("stuck in flight", nil)
	select {
	case <-rs.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never reached the transport")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	completed := c.CloseContext(ctx)
	if completed {
		t.Error("CloseContext reported clean completion despite a blocked transport")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("CloseContext took %v; deadline not honored", elapsed)
	}
	unblock()
	// The worker exits on its own after cancellation aborts the request.
	select {
	case <-c.workerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("worker leaked after CloseContext")
	}
}

// TestCaptureStackCap pins the stack budget.
func TestCaptureStackCap(t *testing.T) {
	out := captureStack(true, 2048)
	if len(out) > 2048 {
		t.Errorf("stack = %d bytes, cap 2048", len(out))
	}
	if len(out) == 0 {
		t.Error("empty stack")
	}
}

// TestNilClientIsSafe pins the documented contract: every method on a nil
// *Client (ignored NewClient error) is a safe no-op.
func TestNilClientIsSafe(t *testing.T) {
	var c *Client
	c.Report(errors.New("ignored"), nil)
	c.ReportError(errors.New("ignored"), nil)
	c.ReportMessage("ignored", nil)
	c.ReportPanicValue("ignored", nil)
	c.SetAttribute("k", "v")
	c.SetAttributes(map[string]interface{}{"k": "v"})
	c.AddBreadcrumb(Breadcrumb{Message: "ignored"})
	if got := c.DroppedReports(); got != 0 {
		t.Errorf("DroppedReports on nil = %d", got)
	}
	if got := c.Stats(); got != (ClientStats{}) {
		t.Errorf("Stats on nil = %+v", got)
	}
	if !c.Flush(time.Millisecond) {
		t.Error("Flush on nil client should trivially succeed")
	}
	if !c.FlushContext(context.Background()) {
		t.Error("FlushContext on nil client should trivially succeed")
	}
	if !c.ReportPanicValueAndFlush("v", nil, time.Millisecond) {
		t.Error("ReportPanicValueAndFlush on nil client should trivially succeed")
	}
	if !c.CloseContext(context.Background()) {
		t.Error("CloseContext on nil client should trivially succeed")
	}
	c.Close()
}

// TestNegativeMaxAttachmentsDisables pins the documented contract: a
// negative MaxAttachments sends NO attachments (privacy opt-out), rather
// than removing the count cap.
func TestNegativeMaxAttachmentsDisables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.log")
	if err := os.WriteFile(path, []byte("must not upload"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.AttachmentPaths = []string{path}
		cfg.MaxAttachments = -1
	})
	c.ReportMessage("no attachments", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	if rs.count() != 1 {
		t.Fatalf("reports = %d, want 1", rs.count())
	}
	for name := range rs.lastAttachments() {
		if strings.HasPrefix(name, "attachment_secret") {
			t.Errorf("attachment sent despite MaxAttachments=-1: %s", name)
		}
	}
}

// TestSourceRootsSymlinkDenied pins the symlink-resolution fix: a link
// under an allowed root pointing outside it must not smuggle content in.
func TestSourceRootsSymlinkDenied(t *testing.T) {
	allowedDir := t.TempDir()
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.go")
	if err := os.WriteFile(secret, []byte("classified\nlines\nhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(allowedDir, "linked.go")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	stack := "goroutine 1 [running]:\n" +
		"main.a()\n" +
		"\t" + link + ":2 +0x1\n"

	_, sources, _ := buildThreads([]byte(stack), sourceOptions{
		mode: SourceCodeContext, contextLines: 2, tabWidth: 8,
		roots: []string{allowedDir},
	})
	for _, sc := range sources {
		if sc.Text != "" {
			t.Errorf("symlink escaped SourceRoots: %q", sc.Text)
		}
	}
}

func TestUUID4Format(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		u := uuid4()
		if len(u) != 36 || u[14] != '4' {
			t.Fatalf("bad uuid: %q", u)
		}
		if seen[u] {
			t.Fatalf("duplicate uuid generated: %q", u)
		}
		seen[u] = true
	}
}
