package bt

import (
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
	c, rs := newTestClient(t, nil)

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

// TestPanicReportRetriesFullQueue pins the bounded blocking enqueue for
// panic reports: with the queue full they retry instead of dropping.
func TestPanicReportRetriesFullQueue(t *testing.T) {
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

	done := make(chan struct{})
	go func() {
		c.ReportPanicValue("panic while queue full", nil)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("panic report returned immediately: dropped instead of retrying")
	case <-time.After(100 * time.Millisecond):
		// Still retrying, as intended.
	}

	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("panic report never enqueued after queue freed")
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
	if !c.Flush(time.Millisecond) {
		t.Error("Flush on nil client should trivially succeed")
	}
	c.Close()
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
