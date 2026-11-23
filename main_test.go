package bt

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingServer captures submitted reports (JSON or multipart) for
// assertions.
type recordingServer struct {
	mu          sync.Mutex
	payloads    []map[string]interface{}
	attachments []map[string]string // parallel to payloads; part name -> content
	headers     []http.Header       // parallel to payloads
	status      int
	statuses    []int                    // consumed one per request before status applies
	block       chan struct{}            // when non-nil, handler blocks until closed
	holds       map[string]chan struct{} // error.message -> handler blocks until closed
	entered     chan struct{}            // signaled when a handler starts blocking
	requests    int
	srv         *httptest.Server
}

func newRecordingServer() *recordingServer {
	rs := &recordingServer{status: http.StatusOK, entered: make(chan struct{}, 64)}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		block := rs.block
		status := rs.status
		if len(rs.statuses) > 0 {
			status, rs.statuses = rs.statuses[0], rs.statuses[1:]
		}
		rs.requests++
		rs.mu.Unlock()

		if err := gunzipRequest(r); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload map[string]interface{}
		atts := map[string]string{}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			if err := r.ParseMultipartForm(64 << 20); err == nil {
				for field, headers := range r.MultipartForm.File {
					f, err := headers[0].Open()
					if err != nil {
						continue
					}
					content, _ := io.ReadAll(f)
					f.Close()
					if field == "upload_file" {
						m := map[string]interface{}{}
						if json.Unmarshal(content, &m) == nil {
							payload = m
						}
					} else {
						atts[field] = string(content)
					}
				}
			}
		} else if body, err := io.ReadAll(r.Body); err == nil {
			m := map[string]interface{}{}
			if json.Unmarshal(body, &m) == nil {
				payload = m
			}
		}
		if payload != nil {
			msg, _ := attrsOrNil(payload)["error.message"].(string)
			rs.mu.Lock()
			if hold := rs.holds[msg]; hold != nil {
				block = hold
			}
			rs.mu.Unlock()
		}

		if block != nil {
			select {
			case rs.entered <- struct{}{}:
			default:
			}
			<-block
		}

		if payload != nil {
			rs.mu.Lock()
			rs.payloads = append(rs.payloads, payload)
			rs.attachments = append(rs.attachments, atts)
			rs.headers = append(rs.headers, r.Header.Clone())
			rs.mu.Unlock()
		}
		w.WriteHeader(status)
	}))
	return rs
}

func gunzipRequest(r *http.Request) error {
	if r.Header.Get("Content-Encoding") != "gzip" {
		return nil
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(zr)
	return nil
}

func attrsOrNil(payload map[string]any) map[string]any {
	attrs, _ := payload["attributes"].(map[string]any)
	return attrs
}

func (rs *recordingServer) lastHeader() http.Header {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.headers) == 0 {
		return nil
	}
	return rs.headers[len(rs.headers)-1]
}

func (rs *recordingServer) messages() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var out []string
	for _, p := range rs.payloads {
		msg, _ := attrsOrNil(p)["error.message"].(string)
		out = append(out, msg)
	}
	return out
}

func (rs *recordingServer) lastAttachments() map[string]string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.attachments) == 0 {
		return nil
	}
	return rs.attachments[len(rs.attachments)-1]
}

func (rs *recordingServer) count() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.payloads)
}

func (rs *recordingServer) last() map[string]interface{} {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.payloads) == 0 {
		return nil
	}
	return rs.payloads[len(rs.payloads)-1]
}

func (rs *recordingServer) reset() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.payloads = nil
	rs.attachments = nil
	rs.headers = nil
}

func attrsOf(t *testing.T, payload map[string]interface{}) map[string]interface{} {
	t.Helper()
	attrs, ok := payload["attributes"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload has no attributes object: %v", payload)
	}
	return attrs
}

// legacyServer backs the legacy global API for the whole test binary; the
// default client is process-global, so it is configured exactly once.
var legacyServer *recordingServer

func TestMain(m *testing.M) {
	legacyServer = newRecordingServer()
	Options.Endpoint = legacyServer.srv.URL
	Options.Token = "test-token"
	Options.CaptureAllGoroutines = true
	os.Exit(m.Run())
}

func TestLegacyReportDelivers(t *testing.T) {
	legacyServer.reset()

	Report(errors.New("it broke"), map[string]interface{}{"custom": "value"})
	if !Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	if got := legacyServer.count(); got != 1 {
		t.Fatalf("expected 1 report, got %d", got)
	}
	payload := legacyServer.last()

	if payload["lang"] != "go" {
		t.Errorf("lang = %v, want go", payload["lang"])
	}
	if payload["agent"] != "backtrace-go" {
		t.Errorf("agent = %v, want backtrace-go", payload["agent"])
	}
	if payload["agentVersion"] != Version {
		t.Errorf("agentVersion = %v, want %s", payload["agentVersion"], Version)
	}

	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if u, _ := payload["uuid"].(string); !uuidRe.MatchString(u) {
		t.Errorf("uuid %q is not RFC 4122 v4", u)
	}

	attrs := attrsOf(t, payload)
	if attrs["error.message"] != "it broke" {
		t.Errorf("error.message = %v", attrs["error.message"])
	}
	if attrs["custom"] != "value" {
		t.Errorf("custom attribute missing: %v", attrs["custom"])
	}
	if attrs["report_type"] != "error" {
		t.Errorf("report_type = %v, want error", attrs["report_type"])
	}
	for _, key := range []string{
		"backtrace.version", "backtrace.agent", "hostname", "uname.sysname",
		"cpu.arch", "process.id", "application", "application.session",
		"go.version", "runtime.goroutines",
	} {
		if _, ok := attrs[key]; !ok {
			t.Errorf("default attribute %q missing", key)
		}
	}

	threads, ok := payload["threads"].(map[string]interface{})
	if !ok || len(threads) == 0 {
		t.Fatalf("threads missing or empty: %v", payload["threads"])
	}
	if payload["mainThread"] != "0" {
		t.Errorf("mainThread = %v, want 0", payload["mainThread"])
	}
	if _, ok := threads["0"]; !ok {
		t.Errorf("faulting thread 0 missing; threads: %d", len(threads))
	}

	classifiers, _ := payload["classifiers"].([]interface{})
	if len(classifiers) == 0 || classifiers[0] != "*errors.errorString" {
		t.Errorf("classifiers = %v, want [*errors.errorString ...]", classifiers)
	}
}

func TestLegacyReportNilIsNoop(t *testing.T) {
	legacyServer.reset()
	Report(nil, nil)
	Flush(2 * time.Second)
	if got := legacyServer.count(); got != 0 {
		t.Fatalf("nil report was sent: %d", got)
	}
}

func TestLegacyReportDoesNotMutateCallerMap(t *testing.T) {
	legacyServer.reset()
	extra := map[string]interface{}{"k": "v"}
	Report("some message", extra)
	Flush(5 * time.Second)

	if _, polluted := extra["report_type"]; polluted {
		t.Error("caller's attribute map was mutated")
	}
	attrs := attrsOf(t, legacyServer.last())
	if attrs["report_type"] != "error" {
		t.Errorf("report_type = %v, want error (v1.0.2 value for messages)", attrs["report_type"])
	}
}

// TestReportPanicIsDeterministic verifies the marker-based flush: the report
// must be delivered before ReportPanic re-panics, every time (the historical
// implementation lost ~50% of panic reports to a select race).
func TestReportPanicIsDeterministic(t *testing.T) {
	legacyServer.reset()

	const iterations = 25
	for i := 0; i < iterations; i++ {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("ReportPanic did not re-panic")
				}
			}()
			defer ReportPanic(nil)
			panic("deterministic panic")
		}()
	}

	if got := legacyServer.count(); got != iterations {
		t.Fatalf("lost panic reports: got %d, want %d", got, iterations)
	}
	attrs := attrsOf(t, legacyServer.last())
	if attrs["report_type"] != "panic" {
		t.Errorf("report_type = %v, want panic", attrs["report_type"])
	}
}

func TestLegacyReportAndRecoverPanic(t *testing.T) {
	legacyServer.reset()

	func() {
		defer ReportAndRecoverPanic(nil)
		panic("recovered panic")
	}()
	// Reaching this line proves the panic was swallowed.

	if !Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if got := legacyServer.count(); got != 1 {
		t.Fatalf("expected 1 report, got %d", got)
	}
}

// TestFinishSendingReportsKeepsWorkerAlive is the regression test for the
// historical bug where FinishSendingReports killed the worker permanently.
func TestFinishSendingReportsKeepsWorkerAlive(t *testing.T) {
	legacyServer.reset()

	Report(errors.New("before finish"), nil)
	FinishSendingReports()
	FinishSendingReports() // second call must not deadlock

	Report(errors.New("after finish"), nil)
	if !Flush(5 * time.Second) {
		t.Fatal("Flush timed out after FinishSendingReports")
	}

	if got := legacyServer.count(); got != 2 {
		t.Fatalf("reports after FinishSendingReports are lost: got %d, want 2", got)
	}
}

func TestSetAttributeIsConcurrencySafe(t *testing.T) {
	legacyServer.reset()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				SetAttribute("concurrent", g*1000+i)
				SetAttributes(map[string]interface{}{"batch": i})
				Report(errors.New("concurrent report"), nil)
			}
		}(g)
	}
	wg.Wait()
	Flush(10 * time.Second)

	if legacyServer.count() == 0 {
		t.Fatal("no reports delivered")
	}
	attrs := attrsOf(t, legacyServer.last())
	if _, ok := attrs["concurrent"]; !ok {
		t.Error("attribute set via SetAttribute missing")
	}
}

// TestLegacySetAttributeNormalisesAtStore pins that the package-level
// SetAttribute stores a detached copy. Stored raw, a nested map would be
// walked at capture time without any lock while the application may still
// be writing to it: a concurrent map access, which is fatal.
func TestLegacySetAttributeNormalisesAtStore(t *testing.T) {
	legacyServer.reset()

	nested := map[string]interface{}{"state": "stored"}
	SetAttribute("legacy.nested", nested)
	t.Cleanup(func() {
		legacyAttrMu.Lock()
		delete(Options.Attributes, "legacy.nested")
		legacyAttrMu.Unlock()
	})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			nested["state"] = i
			nested["extra"] = i
		}
	}()
	for i := 0; i < 50; i++ {
		Report(errors.New("legacy normalised"), nil)
	}
	close(stop)
	wg.Wait()
	Flush(10 * time.Second)

	got, _ := annotationsOf(t, legacyServer.last())["legacy.nested"].(map[string]any)
	if len(got) != 1 || got["state"] != "stored" {
		t.Errorf("stored value follows later changes to the caller's map: %v", got)
	}
}

// TestLegacyEnvVarsOption pins that Options.EnvVars reaches capture through
// the package-level API.
func TestLegacyEnvVarsOption(t *testing.T) {
	t.Setenv("BT_LEGACY_WANTED", "keep")
	t.Setenv("BT_LEGACY_OTHER", "drop")
	legacyServer.reset()

	previous := Options.EnvVars
	Options.EnvVars = []string{"BT_LEGACY_WANTED"}
	t.Cleanup(func() { Options.EnvVars = previous })

	Report(errors.New("legacy env"), nil)
	Flush(10 * time.Second)

	annotations, _ := legacyServer.last()["annotations"].(map[string]interface{})
	env, _ := annotations["Environment Variables"].(map[string]interface{})
	if len(env) != 1 || env["BT_LEGACY_WANTED"] != "keep" {
		t.Errorf("Options.EnvVars capture = %v, want only BT_LEGACY_WANTED", env)
	}
}

// resetDefaultClientForTest detaches the process-global default client so a
// test can exercise the unconfigured path, restoring everything afterwards.
func resetDefaultClientForTest(t *testing.T) {
	t.Helper()
	defaultClientMu.Lock()
	oldClient, oldExplicit := defaultClientV, explicitClient
	defaultClientV, explicitClient = nil, nil
	defaultClientMu.Unlock()
	oldOptions := Options
	t.Cleanup(func() {
		defaultClientMu.Lock()
		defaultClientV, explicitClient = oldClient, oldExplicit
		defaultClientMu.Unlock()
		Options = oldOptions
	})
}

// TestUnconfiguredPanicHelpersDoNotRecover pins the legacy semantic: while
// reporting is unconfigured, the deferred panic helpers must not touch the
// panic at all — the application's panic proceeds exactly as if the handler
// were absent.
func TestUnconfiguredPanicHelpersDoNotRecover(t *testing.T) {
	resetDefaultClientForTest(t)
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	Options.Endpoint = ""
	Options.Token = ""

	recoverValue := func(f func()) (v interface{}) {
		defer func() { v = recover() }()
		f()
		return nil
	}

	if got := recoverValue(func() {
		defer ReportAndRecoverPanic(nil)
		panic("original")
	}); got != "original" {
		t.Fatalf("ReportAndRecoverPanic swallowed an unconfigured panic: recovered %#v, want it to propagate", got)
	}

	if got := recoverValue(func() {
		defer ReportPanic(nil)
		panic("original2")
	}); got != "original2" {
		t.Fatalf("ReportPanic altered an unconfigured panic: recovered %#v", got)
	}
}

func TestLegacyEnvVarAnnotations(t *testing.T) {
	legacyServer.reset()
	t.Setenv("BT_TEST_SECRET_TOKEN", "hunter2")
	// Multi-'=' value without credentials: must survive intact.
	t.Setenv("BT_TEST_JAVA_OPTS", "-Da=b -Dc=d")
	// Connection string with embedded credentials: redacted by value shape.
	t.Setenv("BT_TEST_DATABASE_URL", "postgres://u:p@h/db?sslmode=require")

	Options.SendEnvVars = true
	defer func() { Options.SendEnvVars = false }()

	Report(errors.New("env test"), nil)
	Flush(5 * time.Second)

	annotations, _ := legacyServer.last()["annotations"].(map[string]interface{})
	env, _ := annotations["Environment Variables"].(map[string]interface{})
	if env == nil {
		t.Fatal("Environment Variables annotation missing")
	}
	if env["BT_TEST_SECRET_TOKEN"] != redactedValue {
		t.Errorf("secret env var not redacted: %v", env["BT_TEST_SECRET_TOKEN"])
	}
	if env["BT_TEST_JAVA_OPTS"] != "-Da=b -Dc=d" {
		t.Errorf("env value truncated at '=': %v", env["BT_TEST_JAVA_OPTS"])
	}
	if env["BT_TEST_DATABASE_URL"] != redactedValue {
		t.Errorf("connection string with credentials not redacted: %v", env["BT_TEST_DATABASE_URL"])
	}
}

func TestLegacySetAttributeNilSuppressesDefault(t *testing.T) {
	legacyServer.reset()
	SetAttribute("hostname", nil)
	t.Cleanup(func() {
		legacyAttrMu.Lock()
		delete(Options.Attributes, "hostname")
		legacyAttrMu.Unlock()
	})

	Report(errors.New("no hostname"), nil)
	if !Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if _, ok := attrsOf(t, legacyServer.last())["hostname"]; ok {
		t.Error("hostname sent after SetAttribute(hostname, nil)")
	}
}

func TestClearedEndpointDisablesLegacyReporting(t *testing.T) {
	resetDefaultClientForTest(t)
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	savedDefault := defaultDiagLogger
	t.Cleanup(func() {
		disabledWarnOnce = sync.Once{}
		defaultDiagLogger = savedDefault
	})
	disabledWarnOnce = sync.Once{}
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	logger := &lineLogger{}
	defaultDiagLogger = logger
	Options.Endpoint = rs.srv.URL
	Options.Token = "test-token"

	Report(errors.New("before"), nil)
	c := DefaultClient()
	if c == nil {
		t.Fatal("DefaultClient = nil with a valid endpoint")
	}
	t.Cleanup(c.Close)

	Options.Endpoint = ""
	if DefaultClient() != nil {
		t.Error("DefaultClient != nil after the endpoint was cleared")
	}
	Report(errors.New("cleared"), nil)
	escaped := recoveredFrom(func() {
		defer ReportAndRecoverPanic(nil)
		panic("not swallowed")
	})
	if escaped != "not swallowed" {
		t.Errorf("ReportAndRecoverPanic swallowed the panic with no endpoint: %v", escaped)
	}

	Options.Endpoint = "not a url ::"
	Report(errors.New("invalid"), nil)
	Report(errors.New("invalid again"), nil)

	Options.Endpoint = rs.srv.URL
	Report(errors.New("restored"), nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if got := rs.messages(); !slices.Equal(got, []string{"before", "restored"}) {
		t.Errorf("delivered %q, want before and restored", got)
	}
	if s := c.Stats(); s.Accepted != 2 || s.Delivered != 2 || s.Network != 0 || s.Serialization != 0 {
		t.Errorf("stats = %+v, want 2 accepted and delivered", s)
	}
	if lines := logger.all(); len(lines) != 1 || !strings.HasPrefix(lines[0], "reporting disabled: bt: invalid Config.Endpoint") {
		t.Errorf("logged %q, want one reporting-disabled warning", lines)
	}
}

type reportingLogger struct{ calls atomic.Int32 }

func (l *reportingLogger) Printf(string, ...any) {
	if l.calls.Add(1) < 100 {
		Report("from the logger", nil)
	}
}

func TestReportingDisabledDiagnosticDoesNotRecurse(t *testing.T) {
	resetDefaultClientForTest(t)
	t.Setenv(envEndpoint, "")
	reentrant := &reportingLogger{}
	Options.Endpoint = ""
	Options.DebugBacktrace = true
	Options.Logger = reentrant

	Report("reentrant", nil)
	if n := reentrant.calls.Load(); n != 1 {
		t.Errorf("re-entrant Logger called %d times, want 1", n)
	}
}

func TestLegacyMaxBreadcrumbsFixedAtCreation(t *testing.T) {
	resetDefaultClientForTest(t)
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	rs := newRecordingServer()
	t.Cleanup(rs.srv.Close)
	Options = OptionsStruct{Endpoint: rs.srv.URL, MaxBreadcrumbs: -1}
	c := DefaultClient()
	if c == nil {
		t.Fatal("DefaultClient = nil with a valid endpoint")
	}
	t.Cleanup(c.Close)

	Options.MaxBreadcrumbs = 0
	ctx := WithScope(context.Background())
	ScopeFromContext(ctx).AddBreadcrumb(Breadcrumb{Message: "scope"})
	c.ReportContext(ctx, "x", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if _, ok := rs.lastAttachments()["attachment_bt-breadcrumbs-0"]; ok {
		t.Error("breadcrumbs sent after MaxBreadcrumbs = -1 at creation")
	}
}
