package bthttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
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

func writeAndFlush(w http.ResponseWriter, body string) {
	io.WriteString(w, body)
	http.NewResponseController(w).Flush()
}

func serveExpectingAbort(t *testing.T, h http.Handler, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	h.ServeHTTP(w, r)
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
	serveExpectingAbort(t, wrapped, rec, httptest.NewRequest(http.MethodGet, "/", nil))

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

func TestErrAbortHandlerIsRepanickedUnreported(t *testing.T) {
	for _, repanic := range []bool{false, true} {
		t.Run(fmt.Sprintf("Repanic=%v", repanic), func(t *testing.T) {
			client, cap := newCaptureClient(t)
			h := New(Options{Client: client, Repanic: repanic})
			mux := http.NewServeMux()
			mux.HandleFunc("/partial", func(w http.ResponseWriter, r *http.Request) {
				writeAndFlush(w, "partial-")
				panic(http.ErrAbortHandler)
			})
			mux.HandleFunc("/early", func(w http.ResponseWriter, r *http.Request) {
				panic(http.ErrAbortHandler)
			})
			srv, logs := newAppServer(t, h.Handle(mux))

			res := mustGet(t, srv, "/partial")
			if res.status != http.StatusOK || res.body != "partial-" {
				t.Errorf("status=%d body=%q, want 200 %q", res.status, res.body, "partial-")
			}
			if !errors.Is(res.readErr, io.ErrUnexpectedEOF) {
				t.Errorf("body read error = %v, want unexpected EOF", res.readErr)
			}
			if _, err := get(t, srv, "/early"); err == nil {
				t.Error("aborted request before any write got a response")
			}

			flushReports(t, client)
			if n := cap.count(); n != 0 {
				t.Errorf("reports = %d, want 0", n)
			}
			if l := logs.String(); l != "" {
				t.Errorf("server log = %q, want empty", l)
			}
		})
	}
}

func TestSwallowedPanicAbortsCommittedResponse(t *testing.T) {
	client, cap := newCaptureClient(t)
	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /partial", func(w http.ResponseWriter, r *http.Request) {
		writeAndFlush(w, "partial")
		panic("late panic")
	})
	mux.HandleFunc("GET /header", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("panic after WriteHeader")
	})
	srv, logs := newAppServer(t, h.Handle(mux))

	res := mustGet(t, srv, "/partial")
	if res.status != http.StatusOK || res.body != "partial" {
		t.Errorf("status=%d body=%q, want 200 %q", res.status, res.body, "partial")
	}
	if !errors.Is(res.readErr, io.ErrUnexpectedEOF) {
		t.Errorf("body read error = %v, want unexpected EOF", res.readErr)
	}
	if _, err := get(t, srv, "/header"); err == nil {
		t.Error("response committed by WriteHeader alone arrived intact")
	}

	if n := cap.count(); n != 2 {
		t.Fatalf("reports = %d, want 2", n)
	}
	reports := cap.all()
	if reports[0].Attributes["error.message"] != "late panic" || reports[0].Attributes["request.route"] != "GET /partial" {
		t.Errorf("first report attributes = %v", reports[0].Attributes)
	}
	if l := logs.String(); l != "" {
		t.Errorf("server log = %q, want empty", l)
	}
}

func TestSynthesized500KeepsOnlyOuterHeaders(t *testing.T) {
	client, cap := newCaptureClient(t)
	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	inner := h.HandleFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("Set-Cookie", "session=abc")
		w.Header().Set("Location", "/next")
		w.Header().Set("Content-Length", "1000")
		w.Header().Set("Access-Control-Allow-Origin", "https://changed.example")
		panic("after setting headers")
	})
	cors := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		inner.ServeHTTP(w, r)
	})
	srv, _ := newAppServer(t, cors)

	res := mustGet(t, srv, "/")
	if res.status != http.StatusInternalServerError || res.body != "Internal Server Error\n" || res.readErr != nil {
		t.Errorf("status=%d body=%q readErr=%v, want a complete 500", res.status, res.body, res.readErr)
	}
	for _, key := range []string{"Cache-Control", "Set-Cookie", "Location"} {
		if v := res.header.Get(key); v != "" {
			t.Errorf("%s = %q on the synthesized 500", key, v)
		}
	}
	if v := res.header.Get("Access-Control-Allow-Origin"); v != "*" {
		t.Errorf("outer header Access-Control-Allow-Origin = %q, want %q", v, "*")
	}
	if v := res.header.Get("Content-Type"); v != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", v)
	}
	if cap.count() != 1 {
		t.Errorf("reports = %d, want 1", cap.count())
	}
}

func TestInformationalStatusDoesNotCommit(t *testing.T) {
	client, cap := newCaptureClient(t)
	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	mux := http.NewServeMux()
	mux.HandleFunc("/created", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</app.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "created")
	})
	mux.HandleFunc("/panic", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		panic("after early hints")
	})
	mux.HandleFunc("/invalid", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(42)
	})
	srv, _ := newAppServer(t, h.Handle(mux))

	var mu sync.Mutex
	var informational []int
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		mu.Lock()
		informational = append(informational, code)
		mu.Unlock()
		return nil
	}}
	req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, srv.URL+"/created", nil)
	res, err := do(freshClient(t), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusCreated || res.body != "created" {
		t.Errorf("status=%d body=%q, want 201 %q", res.status, res.body, "created")
	}
	mu.Lock()
	if len(informational) != 1 || informational[0] != http.StatusEarlyHints {
		t.Errorf("1xx responses = %v, want [103]", informational)
	}
	mu.Unlock()

	if res := mustGet(t, srv, "/panic"); res.status != http.StatusInternalServerError || res.readErr != nil {
		t.Errorf("panic after 103: status=%d readErr=%v, want a complete 500", res.status, res.readErr)
	}
	if res := mustGet(t, srv, "/invalid"); res.status != http.StatusInternalServerError || res.readErr != nil {
		t.Errorf("invalid WriteHeader code: status=%d readErr=%v, want a complete 500", res.status, res.readErr)
	}

	reports := cap.all()
	if len(reports) != 2 {
		t.Fatalf("reports = %d, want 2", len(reports))
	}
	if msg, _ := reports[1].Attributes["error.message"].(string); !strings.Contains(msg, "invalid WriteHeader code 42") {
		t.Errorf("error.message = %q", msg)
	}
}

func TestSwitchingProtocolsCommits(t *testing.T) {
	inner := &plainWriter{}
	tw := &trackingWriter{ResponseWriter: inner}
	tw.WriteHeader(http.StatusEarlyHints)
	if tw.wroteHeader {
		t.Fatal("103 committed the response")
	}
	tw.WriteHeader(http.StatusSwitchingProtocols)
	if !tw.wroteHeader || tw.status != http.StatusSwitchingProtocols {
		t.Errorf("101: wroteHeader=%v status=%d", tw.wroteHeader, tw.status)
	}
	if fmt.Sprint(inner.statuses) != "[103 101]" {
		t.Errorf("codes passed through = %v", inner.statuses)
	}
}

func TestReportStatusSkipsHijackedConnections(t *testing.T) {
	client, _ := newCaptureClient(t)
	statusCalls := make(chan int, 1)
	h := New(Options{Client: client, ReportStatus: func(status int, r *http.Request) bool {
		statusCalls <- status
		return true
	}})
	done := make(chan struct{}, 1)
	srv, _ := newAppServer(t, signalReturn(h.HandleFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
		buf.Flush()
	}), done))

	res := mustGet(t, srv, "/")
	if res.status != http.StatusOK || res.body != "hi" || res.readErr != nil {
		t.Errorf("status=%d body=%q readErr=%v", res.status, res.body, res.readErr)
	}
	waitSignal(t, done)
	select {
	case status := <-statusCalls:
		t.Errorf("ReportStatus called for a hijacked connection with %d", status)
	default:
	}
}

func TestRequestMethodIsCapped(t *testing.T) {
	client, cap := newCaptureClient(t)
	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	srv, _ := newAppServer(t, h.HandleFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("long method")
	}))

	req, err := http.NewRequest(strings.Repeat("M", 100), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := do(freshClient(t), req); err != nil || res.status != http.StatusInternalServerError {
		t.Fatalf("status=%d err=%v, want 500", res.status, err)
	}
	if got := lastAttrs(cap)["request.method"]; got != strings.Repeat("M", 64) {
		t.Errorf("request.method = %v, want 64 bytes", got)
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

type authKey struct{}

func TestSetRoute(t *testing.T) {
	cloning := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authKey{}, "user-1")))
		})
	}
	newMux := func(setRoute string) *http.ServeMux {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
			route := setRoute
			if route == "pattern" {
				route = r.Pattern
			}
			if route != "" {
				SetRoute(r.Context(), route)
			}
			panic("user lookup failed")
		})
		return mux
	}
	cases := []struct {
		name     string
		handler  func(http.Handler) http.Handler
		setRoute string
		want     any
	}{
		{"behind cloning middleware", cloning, "pattern", "GET /users/{id}"},
		{"behind cloning middleware without SetRoute", cloning, "", nil},
		{"Request.Pattern wins", func(m http.Handler) http.Handler { return m }, "/override", "GET /users/{id}"},
		{"capped", cloning, strings.Repeat("r", 3000), strings.Repeat("r", 2048)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, cap := newCaptureClient(t)
			h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
			srv, _ := newAppServer(t, h.Handle(tc.handler(newMux(tc.setRoute))))
			if res := mustGet(t, srv, "/users/7"); res.status != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", res.status)
			}
			if got := lastAttrs(cap)["request.route"]; got != tc.want {
				t.Errorf("request.route = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("outside a Handler", func(t *testing.T) {
		SetRoute(context.Background(), "/x")
		SetRoute(bt.WithScope(context.Background()), "/x")
		var nilCtx context.Context
		SetRoute(nilCtx, "/x")
	})
}

func TestReportStatus(t *testing.T) {
	type call struct {
		path   string
		status int
	}
	newServer := func(t *testing.T, opts Options) (*httptest.Server, *logBuffer, chan call) {
		calls := make(chan call, 16)
		if opts.ReportStatus == nil {
			opts.ReportStatus = func(status int, r *http.Request) bool {
				calls <- call{r.URL.Path, status}
				return status >= 500
			}
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /busy", func(w http.ResponseWriter, r *http.Request) {
			bt.ScopeFromContext(r.Context()).SetAttribute("user.id", "u-7")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "busy")
		})
		mux.HandleFunc("GET /ok", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "fine")
		})
		mux.HandleFunc("GET /empty", func(w http.ResponseWriter, r *http.Request) {})
		mux.HandleFunc("GET /hints", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
		})
		mux.HandleFunc("GET /odd", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(599)
		})
		mux.HandleFunc("GET /panic", func(w http.ResponseWriter, r *http.Request) {
			panic("handler panic")
		})
		srv, logs := newAppServer(t, New(opts).Handle(mux))
		return srv, logs, calls
	}
	nextCall := func(t *testing.T, calls chan call) call {
		t.Helper()
		select {
		case c := <-calls:
			return c
		case <-time.After(5 * time.Second):
			t.Fatal("ReportStatus not called")
			return call{}
		}
	}

	t.Run("reports chosen statuses", func(t *testing.T) {
		client, cap := newCaptureClient(t)
		srv, _, calls := newServer(t, Options{Client: client})

		if res := mustGet(t, srv, "/busy"); res.status != http.StatusServiceUnavailable || res.body != "busy" {
			t.Errorf("busy: status=%d body=%q", res.status, res.body)
		}
		if c := nextCall(t, calls); c != (call{"/busy", 503}) {
			t.Errorf("call = %+v", c)
		}
		flushReports(t, client)
		if cap.count() != 1 {
			t.Fatalf("reports = %d, want 1", cap.count())
		}
		rep := cap.last()
		want := map[string]any{
			"error.message":   "HTTP 503 Service Unavailable",
			"error.type":      "Message",
			"report_type":     "error",
			"response.status": float64(503),
			"request.method":  "GET",
			"request.route":   "GET /busy",
			"request.proto":   "HTTP/1.1",
			"user.id":         "u-7",
		}
		for k, v := range want {
			if rep.Attributes[k] != v {
				t.Errorf("%s = %v, want %v", k, rep.Attributes[k], v)
			}
		}
		if fmt.Sprint(rep.Classifiers) != "[message]" {
			t.Errorf("classifiers = %v", rep.Classifiers)
		}
		if got := rep.Attributes["_mod_fingerprint"]; got != "bthttp status 503 GET /busy" {
			t.Errorf("_mod_fingerprint = %v", got)
		}

		for _, tc := range []struct {
			path   string
			status int
		}{{"/ok", 200}, {"/empty", 200}, {"/hints", 200}} {
			res := mustGet(t, srv, tc.path)
			if res.status != tc.status {
				t.Errorf("%s: client status = %d", tc.path, res.status)
			}
			if c := nextCall(t, calls); c != (call{tc.path, tc.status}) {
				t.Errorf("call = %+v, want %s %d", c, tc.path, tc.status)
			}
		}

		mustGet(t, srv, "/odd")
		if c := nextCall(t, calls); c != (call{"/odd", 599}) {
			t.Errorf("call = %+v", c)
		}
		flushReports(t, client)
		if got := lastAttrs(cap)["error.message"]; got != "HTTP 599" {
			t.Errorf("error.message = %v, want HTTP 599", got)
		}
		if got := lastAttrs(cap)["_mod_fingerprint"]; got != "bthttp status 599 GET /odd" {
			t.Errorf("_mod_fingerprint = %v", got)
		}
		if cap.count() != 2 {
			t.Errorf("reports = %d, want 2", cap.count())
		}
	})

	t.Run("fingerprint", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			attrs map[string]any
			mux   bool
			want  any
		}{
			{"no route", nil, false, "bthttp status 503"},
			{"RequestAttributes replaces it", map[string]any{"_mod_fingerprint": "checkout"}, true, "checkout"},
			{"nil from RequestAttributes removes it", map[string]any{"_mod_fingerprint": nil}, true, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				client, cap := newCaptureClient(t)
				h := New(Options{
					Client:            client,
					ReportStatus:      func(status int, r *http.Request) bool { return true },
					RequestAttributes: func(*http.Request) map[string]any { return tc.attrs },
				})
				var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusServiceUnavailable)
				})
				if tc.mux {
					mux := http.NewServeMux()
					mux.Handle("GET /busy", handler)
					handler = mux
				}
				h.Handle(handler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/busy", nil))
				flushReports(t, client)
				got, set := lastAttrs(cap)["_mod_fingerprint"]
				if got != tc.want || set != (tc.want != nil) {
					t.Errorf("_mod_fingerprint = %v (set %v), want %v", got, set, tc.want)
				}
			})
		}
	})

	t.Run("after request cancellation", func(t *testing.T) {
		client, cap := newCaptureClient(t)
		returned := make(chan struct{}, 1)
		h := New(Options{Client: client, ReportStatus: func(int, *http.Request) bool { return true }})
		handler := h.HandleFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
			w.WriteHeader(http.StatusGatewayTimeout)
		})
		srv, _ := newAppServer(t, signalReturn(handler, returned))

		c := freshClient(t)
		c.Timeout = 50 * time.Millisecond
		if _, err := c.Get(srv.URL); err == nil {
			t.Fatal("request did not time out")
		}
		waitSignal(t, returned)
		flushReports(t, client)
		if got := lastAttrs(cap)["error.message"]; got != "HTTP 504 Gateway Timeout" {
			t.Errorf("error.message = %v", got)
		}
	})

	t.Run("not called for panics", func(t *testing.T) {
		client, cap := newCaptureClient(t)
		srv, _, calls := newServer(t, Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
		if res := mustGet(t, srv, "/panic"); res.status != http.StatusInternalServerError {
			t.Errorf("status = %d", res.status)
		}
		if cap.count() != 1 || lastAttrs(cap)["report_type"] != "panic" {
			t.Errorf("reports = %d, last = %v", cap.count(), lastAttrs(cap))
		}
		select {
		case c := <-calls:
			t.Errorf("ReportStatus called for a panic: %+v", c)
		default:
		}
	})

	t.Run("panicking callback", func(t *testing.T) {
		client, cap := newCaptureClient(t)
		srv, logs, _ := newServer(t, Options{Client: client, ReportStatus: func(int, *http.Request) bool {
			panic("callback bug")
		}})
		if res := mustGet(t, srv, "/busy"); res.status != http.StatusServiceUnavailable || res.body != "busy" || res.readErr != nil {
			t.Errorf("status=%d body=%q readErr=%v", res.status, res.body, res.readErr)
		}
		flushReports(t, client)
		if cap.count() != 0 {
			t.Errorf("reports = %d, want 0", cap.count())
		}
		if l := logs.String(); l != "" {
			t.Errorf("server log = %q", l)
		}
	})

	t.Run("default client", func(t *testing.T) {
		client, cap := newCaptureClient(t)
		bt.SetDefaultClient(client)
		t.Cleanup(func() { bt.SetDefaultClient(nil) })
		srv, _, calls := newServer(t, Options{})
		mustGet(t, srv, "/busy")
		nextCall(t, calls)
		flushReports(t, client)
		if got := lastAttrs(cap)["error.message"]; got != "HTTP 503 Service Unavailable" {
			t.Errorf("error.message = %v", got)
		}
	})
}

func TestOutboundBreadcrumbsReachPanicReport(t *testing.T) {
	client, cap := newCaptureClient(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	outbound := &http.Client{Transport: NewTransport(&http.Transport{}, TransportOptions{})}

	h := New(Options{Client: client, WaitForDelivery: true, FlushTimeout: 5 * time.Second})
	srv, _ := newAppServer(t, h.HandleFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.URL+"/inventory?sku=1", nil)
		resp, err := outbound.Do(req)
		if err != nil {
			t.Errorf("outbound request: %v", err)
			return
		}
		resp.Body.Close()
		panic("upstream unavailable")
	}))

	if res := mustGet(t, srv, "/"); res.status != http.StatusInternalServerError {
		t.Fatalf("status = %d", res.status)
	}
	crumbs := cap.last().Breadcrumbs
	if len(crumbs) != 1 {
		t.Fatalf("breadcrumbs = %+v, want the outbound request", crumbs)
	}
	host := strings.TrimPrefix(upstream.URL, "http://")
	if b := crumbs[0]; b.Type != "http" || b.Message != "GET "+host+" 502" || b.Attributes["status"] != float64(502) {
		t.Errorf("breadcrumb = %+v", b)
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

func TestUnconfiguredReportingIsHarmless(t *testing.T) {
	t.Setenv("BACKTRACE_ENDPOINT", "")
	t.Setenv("BACKTRACE_TOKEN", "")
	if bt.DefaultClient() != nil {
		t.Skip("a default client exists in this test binary")
	}
	var statusCalls atomic.Int32
	h := New(Options{WaitForDelivery: true, ReportStatus: func(int, *http.Request) bool {
		statusCalls.Add(1)
		return true
	}})
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "fine") })
	mux.HandleFunc("/panic", func(w http.ResponseWriter, r *http.Request) { panic("nobody listening") })
	srv, logs := newAppServer(t, h.Handle(mux))

	if res := mustGet(t, srv, "/ok"); res.status != http.StatusOK || res.body != "fine" {
		t.Errorf("ok: status=%d body=%q", res.status, res.body)
	}
	if res := mustGet(t, srv, "/panic"); res.status != http.StatusInternalServerError || res.readErr != nil {
		t.Errorf("panic: status=%d readErr=%v", res.status, res.readErr)
	}
	if n := statusCalls.Load(); n != 1 {
		t.Errorf("ReportStatus calls = %d, want 1", n)
	}
	if l := logs.String(); l != "" {
		t.Errorf("server log = %q", l)
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

func TestBoundedStringIsRuneSafe(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{"abé", 3, "ab"},
		{"abé", 4, "abé"},
		{"a😀", 2, "a"},
		{"a😀", 4, "a"},
		{"a😀", 5, "a😀"},
		{"😀", 3, ""},
		{"ab" + strings.Repeat("\x80", 8), 8, "ab\x80\x80"},
	} {
		if got := boundedString(tc.in, tc.max); got != tc.want {
			t.Errorf("boundedString(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}
