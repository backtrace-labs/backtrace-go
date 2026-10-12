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

func lastAttrs(c *capture) map[string]interface{} {
	p := c.last()
	if p == nil {
		return nil
	}
	attrs, _ := p["attributes"].(map[string]interface{})
	return attrs
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

func TestMiddlewareReportsPanics(t *testing.T) {
	client, cap := newCaptureClient(t)

	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /checkout", func(w http.ResponseWriter, r *http.Request) {
		panic("handler exploded")
	})
	wrapped := h.Handle(mux)

	req := httptest.NewRequest(http.MethodPost, "/checkout?item=1", nil)
	req.Header.Set("User-Agent", "bthttp-test-agent")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if cap.count() != 1 {
		t.Fatalf("reports = %d, want 1", cap.count())
	}
	attrs := lastAttrs(cap)
	if attrs["error.message"] != "handler exploded" {
		t.Errorf("error.message = %v", attrs["error.message"])
	}
	if attrs["report_type"] != "panic" {
		t.Errorf("report_type = %v", attrs["report_type"])
	}
	if attrs["request.method"] != "POST" {
		t.Errorf("request.method = %v", attrs["request.method"])
	}
	if attrs["request.route"] != "POST /checkout" {
		t.Errorf("request.route = %v", attrs["request.route"])
	}

	// PII defaults OFF: raw URL, host, remote address, user agent absent.
	for _, key := range []string{"request.url", "request.host", "request.remote_addr", "request.user_agent"} {
		if _, present := attrs[key]; present {
			t.Errorf("%s sent without SendDefaultPII", key)
		}
	}

	// A swallowed panic on an uncommitted response becomes a 500, not an
	// empty 200.
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestMiddlewareSendDefaultPII(t *testing.T) {
	client, cap := newCaptureClient(t)

	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second,
		SendDefaultPII: true})
	wrapped := h.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("with pii")
	}))

	req := httptest.NewRequest(http.MethodGet, "/orders?id=1", nil)
	req.Header.Set("User-Agent", "bthttp-test-agent")
	wrapped.ServeHTTP(httptest.NewRecorder(), req)

	attrs := lastAttrs(cap)
	if attrs["request.url"] != "/orders" {
		t.Errorf("request.url = %v", attrs["request.url"])
	}
	if attrs["request.user_agent"] != "bthttp-test-agent" {
		t.Errorf("request.user_agent = %v", attrs["request.user_agent"])
	}
	if attrs["request.host"] == nil || attrs["request.remote_addr"] == nil {
		t.Errorf("host/remote_addr missing with SendDefaultPII: %v", attrs)
	}
}

func TestMiddlewareRequestAttributesCallback(t *testing.T) {
	client, cap := newCaptureClient(t)

	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second,
		RequestAttributes: func(r *http.Request) map[string]interface{} {
			return map[string]interface{}{"tenant.id": r.Header.Get("X-Tenant")}
		}})
	wrapped := h.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("with callback")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant", "acme")
	wrapped.ServeHTTP(httptest.NewRecorder(), req)

	if attrs := lastAttrs(cap); attrs["tenant.id"] != "acme" {
		t.Errorf("callback attribute missing: %v", attrs["tenant.id"])
	}
}

func TestMiddlewarePanickingCallbackIsContained(t *testing.T) {
	client, cap := newCaptureClient(t)

	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second,
		RequestAttributes: func(r *http.Request) map[string]interface{} {
			panic("callback bug")
		}})
	wrapped := h.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("handler panic")
	}))

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil)) // must not panic

	if cap.count() != 1 {
		t.Fatalf("report lost to callback panic: %d", cap.count())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
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

func TestMiddlewareCommittedResponseKeptOnPanic(t *testing.T) {
	client, _ := newCaptureClient(t)

	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	wrapped := h.Handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted) // response committed...
		panic("late panic")                // ...then the handler dies
	}))

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d; committed response must not be overwritten", rec.Code)
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
