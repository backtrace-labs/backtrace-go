package bthttp

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
	"github.com/backtrace-labs/backtrace-go/internal/fakebacktrace"
)

type capture struct {
	mu       sync.Mutex
	payloads []fakebacktrace.Report
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.payloads)
}

func (c *capture) last() fakebacktrace.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.payloads) == 0 {
		return fakebacktrace.Report{}
	}
	return c.payloads[len(c.payloads)-1]
}

func (c *capture) all() []fakebacktrace.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fakebacktrace.Report(nil), c.payloads...)
}

func lastAttrs(c *capture) map[string]interface{} {
	return c.last().Attributes
}

func newCaptureClient(t *testing.T) (*bt.Client, *capture) {
	return newDelayedCaptureClient(t, 0)
}

func newDelayedCaptureClient(t *testing.T, holdBeforeRecord time.Duration) (*bt.Client, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep, err := fakebacktrace.Decode(r)
		if err != nil {
			t.Errorf("decode report: %v", err)
			return
		}
		time.Sleep(holdBeforeRecord)
		cap.mu.Lock()
		cap.payloads = append(cap.payloads, rep)
		cap.mu.Unlock()
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

func flushReports(t *testing.T, c *bt.Client) {
	t.Helper()
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
}

type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newAppServer(t *testing.T, h http.Handler) (*httptest.Server, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(logs, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, logs
}

func signalReturn(next http.Handler, done chan<- struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		done <- struct{}{}
	})
}

func waitSignal(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}
}

type result struct {
	status  int
	header  http.Header
	body    string
	readErr error
}

func freshClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func do(c *http.Client, req *http.Request) (result, error) {
	resp, err := c.Do(req)
	if err != nil {
		return result{}, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, header: resp.Header, body: string(body), readErr: readErr}, nil
}

func get(t *testing.T, srv *httptest.Server, path string) (result, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return do(freshClient(t), req)
}

func mustGet(t *testing.T, srv *httptest.Server, path string) result {
	t.Helper()
	res, err := get(t, srv, path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return res
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

func TestMiddlewareUsesSetDefaultClient(t *testing.T) {
	client, cap := newCaptureClient(t)
	bt.SetDefaultClient(client)
	t.Cleanup(func() { bt.SetDefaultClient(nil) })

	h := New(Options{WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	wrapped := h.HandleFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("default client panic")
	})
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if cap.count() != 1 {
		t.Fatalf("reports = %d, want 1 delivered before the handler returned", cap.count())
	}
	if attrs := lastAttrs(cap); attrs["error.message"] != "default client panic" || attrs["request.method"] != "GET" {
		t.Errorf("attributes = %v", attrs)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestEachRequestRunsUnderItsOwnScope(t *testing.T) {
	client, cap := newCaptureClient(t)
	h := New(Options{Client: client})
	inner := h.HandleFunc(func(w http.ResponseWriter, r *http.Request) {
		scope := bt.ScopeFromContext(r.Context())
		if scope == nil {
			t.Error("request context carries no scope")
			return
		}
		user := r.URL.Query().Get("user")
		scope.SetAttribute("user.id", user)
		scope.AddBreadcrumb(bt.Breadcrumb{Message: "loading cart for " + user, Type: bt.BreadcrumbTypeUser})
		panic("cart failed")
	})
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := bt.WithScope(r.Context())
		bt.ScopeFromContext(ctx).SetAttribute("tenant", "acme")
		inner.ServeHTTP(w, r.WithContext(ctx))
	})
	srv, _ := newAppServer(t, outer)

	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := get(t, srv, fmt.Sprintf("/?user=u-%d", i))
			if err != nil || res.status != http.StatusInternalServerError {
				t.Errorf("user u-%d: status=%d err=%v", i, res.status, err)
			}
		}()
	}
	wg.Wait()
	flushReports(t, client)

	reports := cap.all()
	if len(reports) != n {
		t.Fatalf("reports = %d, want %d", len(reports), n)
	}
	seen := map[string]bool{}
	for _, rep := range reports {
		user, _ := rep.Attributes["user.id"].(string)
		seen[user] = true
		if rep.Attributes["tenant"] != "acme" {
			t.Errorf("%s: tenant = %v, want the outer scope's value", user, rep.Attributes["tenant"])
		}
		if len(rep.Breadcrumbs) != 1 || rep.Breadcrumbs[0].Message != "loading cart for "+user {
			t.Errorf("%s: breadcrumbs = %+v, want only its own", user, rep.Breadcrumbs)
		}
	}
	if len(seen) != n {
		t.Errorf("user.id values = %v, want %d distinct", seen, n)
	}
}

func TestNonBlockingPanicReportAfterRequestCancellation(t *testing.T) {
	client, cap := newCaptureClient(t)
	returned := make(chan struct{}, 1)
	handler := New(Options{Client: client}).HandleFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		panic("client gave up")
	})
	srv, _ := newAppServer(t, signalReturn(handler, returned))

	c := freshClient(t)
	c.Timeout = 50 * time.Millisecond
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("request did not time out")
	}
	waitSignal(t, returned)
	flushReports(t, client)
	if got := lastAttrs(cap)["error.message"]; got != "client gave up" {
		t.Errorf("error.message = %v", got)
	}
}

func TestPanicReportIgnoresRequestCancellation(t *testing.T) {
	client, cap := newDelayedCaptureClient(t, 300*time.Millisecond)
	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	finished := make(chan int, 1)
	handler := h.HandleFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		panic("client gave up")
	})
	srv, _ := newAppServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		finished <- cap.count()
	}))

	c := freshClient(t)
	c.Timeout = 50 * time.Millisecond
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("request did not time out")
	}
	select {
	case n := <-finished:
		if n != 1 {
			t.Errorf("reports delivered when the handler returned = %d, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}
}

func TestHandlerReportsCarryRequestScope(t *testing.T) {
	client, cap := newCaptureClient(t)
	srv, _ := newAppServer(t, New(Options{Client: client}).HandleFunc(func(w http.ResponseWriter, r *http.Request) {
		bt.ScopeFromContext(r.Context()).SetAttribute("order.id", "o-9")
		client.ReportContext(r.Context(), errors.New("payment declined"), nil)
		w.WriteHeader(http.StatusPaymentRequired)
	}))

	if res := mustGet(t, srv, "/"); res.status != http.StatusPaymentRequired {
		t.Errorf("status = %d", res.status)
	}
	flushReports(t, client)
	if attrs := lastAttrs(cap); attrs["order.id"] != "o-9" || attrs["error.message"] != "payment declined" {
		t.Errorf("attributes = %v", attrs)
	}
	if _, set := lastAttrs(cap)["request.method"]; set {
		t.Error("request attributes added to a report the middleware did not make")
	}
}

func TestOuterMiddlewareSeesRouteMatch(t *testing.T) {
	type seen struct {
		pattern, id, rest string
		recovered         bool
	}
	for _, tc := range []struct {
		name    string
		repanic bool
		path    string
	}{
		{"returned", false, "/items/42/a/b"},
		{"swallowed panic", false, "/items/42/panic"},
		{"repanic", true, "/items/42/panic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newCaptureClient(t)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /items/{id}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
				if r.PathValue("rest") == "panic" {
					panic("item failed")
				}
			})
			wrapped := New(Options{Client: client, Repanic: tc.repanic}).Handle(mux)
			var got seen
			outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() {
					got.recovered = recover() != nil
					got.pattern, got.id, got.rest = r.Pattern, r.PathValue("id"), r.PathValue("rest")
				}()
				wrapped.ServeHTTP(w, r)
			})
			outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tc.path, nil))

			wantRest := strings.TrimPrefix(tc.path, "/items/42/")
			want := seen{"GET /items/{id}/{rest...}", "42", wantRest, tc.repanic}
			if got != want {
				t.Errorf("outer middleware saw %+v, want %+v", got, want)
			}
		})
	}

	t.Run("behind an outer ServeMux", func(t *testing.T) {
		client, _ := newCaptureClient(t)
		inner := http.NewServeMux()
		inner.HandleFunc("GET /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {})
		outerMux := http.NewServeMux()
		outerMux.Handle("/api/{section}/", New(Options{Client: client}).Handle(inner))
		var pattern, id string
		logging := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			outerMux.ServeHTTP(w, r)
			pattern, id = r.Pattern, r.PathValue("id")
		})
		logging.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/items/7", nil))
		if pattern != "GET /api/items/{id}" || id != "7" {
			t.Errorf("pattern=%q id=%q", pattern, id)
		}
	})
}
