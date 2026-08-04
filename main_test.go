package bt

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingServer captures submitted reports (JSON or multipart) for
// assertions.
type recordingServer struct {
	mu          sync.Mutex
	payloads    []map[string]interface{}
	attachments []map[string]string // parallel to payloads; part name -> content
	status      int
	block       chan struct{} // when non-nil, handler blocks until closed
	entered     chan struct{} // signaled when a handler starts blocking
	srv         *httptest.Server
}

func newRecordingServer() *recordingServer {
	rs := &recordingServer{status: http.StatusOK, entered: make(chan struct{}, 64)}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		block := rs.block
		status := rs.status
		rs.mu.Unlock()

		if block != nil {
			select {
			case rs.entered <- struct{}{}:
			default:
			}
			<-block
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
			rs.mu.Lock()
			rs.payloads = append(rs.payloads, payload)
			rs.attachments = append(rs.attachments, atts)
			rs.mu.Unlock()
		}
		w.WriteHeader(status)
	}))
	return rs
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
	if len(classifiers) == 0 || classifiers[0] != "error" {
		t.Errorf("classifiers = %v, want [error ...]", classifiers)
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
	if attrs["report_type"] != "message" {
		t.Errorf("report_type = %v, want message", attrs["report_type"])
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

func TestLegacyEnvVarAnnotations(t *testing.T) {
	legacyServer.reset()
	t.Setenv("BT_TEST_SECRET_TOKEN", "hunter2")
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
	if env["BT_TEST_DATABASE_URL"] != "postgres://u:p@h/db?sslmode=require" {
		t.Errorf("env value truncated at '=': %v", env["BT_TEST_DATABASE_URL"])
	}
}
