package bthttp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
)

type capture struct {
	mu       sync.Mutex
	payloads []map[string]interface{}
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.payloads)
}

func (c *capture) last() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.payloads) == 0 {
		return nil
	}
	return c.payloads[len(c.payloads)-1]
}

func newCaptureClient(t *testing.T) (*bt.Client, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		payload := map[string]interface{}{}
		if json.Unmarshal(body, &payload) == nil {
			cap.mu.Lock()
			cap.payloads = append(cap.payloads, payload)
			cap.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client, err := bt.NewClient(bt.Config{Endpoint: srv.URL, Token: "middleware-test"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(client.Close)
	return client, cap
}

func TestMiddlewareReportsPanicsWithRequestAttributes(t *testing.T) {
	client, cap := newCaptureClient(t)

	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	wrapped := h.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("handler exploded")
	}))

	req := httptest.NewRequest(http.MethodPost, "/checkout?item=1", nil)
	req.Header.Set("User-Agent", "bthttp-test-agent")
	wrapped.ServeHTTP(httptest.NewRecorder(), req)

	if cap.count() != 1 {
		t.Fatalf("reports = %d, want 1", cap.count())
	}
	attrs, _ := cap.last()["attributes"].(map[string]interface{})
	if attrs["error.message"] != "handler exploded" {
		t.Errorf("error.message = %v", attrs["error.message"])
	}
	if attrs["request.url"] != "/checkout" || attrs["request.method"] != "POST" {
		t.Errorf("request attributes wrong: url=%v method=%v", attrs["request.url"], attrs["request.method"])
	}
	if attrs["request.user_agent"] != "bthttp-test-agent" {
		t.Errorf("request.user_agent = %v", attrs["request.user_agent"])
	}
	if attrs["report_type"] != "panic" {
		t.Errorf("report_type = %v", attrs["report_type"])
	}
}

func TestMiddlewareNoPanicPassthrough(t *testing.T) {
	client, cap := newCaptureClient(t)

	var served bool
	h := New(Options{Client: client})
	wrapped := h.HandleFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusTeapot)
	})

	rec := httptest.NewRecorder()
	wrapped(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))

	if !served || rec.Code != http.StatusTeapot {
		t.Error("handler not executed normally")
	}
	client.Flush(2 * time.Second)
	if cap.count() != 0 {
		t.Errorf("healthy request produced %d reports", cap.count())
	}
}

func TestMiddlewareRepanic(t *testing.T) {
	client, cap := newCaptureClient(t)

	h := New(Options{Client: client, Repanic: true, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	wrapped := h.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("must propagate")
	}))

	didPanic := false
	func() {
		defer func() {
			if recover() != nil {
				didPanic = true
			}
		}()
		wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()

	if !didPanic {
		t.Error("Repanic: panic was swallowed")
	}
	if cap.count() != 1 {
		t.Errorf("reports = %d, want 1", cap.count())
	}
}

func TestMiddlewareSwallowsWithoutRepanic(t *testing.T) {
	client, cap := newCaptureClient(t)

	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	wrapped := h.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("swallowed")
	}))

	// Must not panic.
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if cap.count() != 1 {
		t.Errorf("reports = %d, want 1", cap.count())
	}
}
