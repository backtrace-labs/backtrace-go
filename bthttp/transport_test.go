package bthttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	bt "github.com/backtrace-labs/backtrace-go"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type idleCloser struct {
	roundTripFunc
	closed int
}

func (c *idleCloser) CloseIdleConnections() { c.closed++ }

type panickingError struct{}

func (panickingError) Error() string { panic("Error() bug") }

func scopeBreadcrumbs(t *testing.T, client *bt.Client, cap *capture, ctx context.Context) []bt.Breadcrumb {
	t.Helper()
	before := cap.count()
	client.ReportContext(ctx, "read breadcrumbs", nil)
	flushReports(t, client)
	if cap.count() != before+1 {
		t.Fatalf("reports = %d, want %d", cap.count(), before+1)
	}
	return cap.last().Breadcrumbs
}

func TestTransportRecordsBreadcrumbOnScope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "created")
	}))
	t.Cleanup(upstream.Close)
	host := strings.TrimPrefix(upstream.URL, "http://")

	for _, pii := range []bool{false, true} {
		name := "default"
		if pii {
			name = "SendDefaultPII"
		}
		t.Run(name, func(t *testing.T) {
			client, cap := newCaptureClient(t)
			hc := &http.Client{Transport: NewTransport(nil, TransportOptions{SendDefaultPII: pii})}
			ctx := bt.WithScope(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/items?token=secret", strings.NewReader("payload"))
			res, err := do(hc, req)
			if err != nil {
				t.Fatal(err)
			}
			if res.status != http.StatusCreated || res.body != "created" || res.readErr != nil {
				t.Errorf("status=%d body=%q readErr=%v", res.status, res.body, res.readErr)
			}

			crumbs := scopeBreadcrumbs(t, client, cap, ctx)
			if len(crumbs) != 1 {
				t.Fatalf("breadcrumbs = %+v, want 1", crumbs)
			}
			b := crumbs[0]
			wantMessage := "POST " + host + " 201"
			if pii {
				wantMessage = "POST " + host + "/items 201"
			}
			if b.Type != bt.BreadcrumbTypeHTTP || b.Level != bt.BreadcrumbInfo || b.Message != wantMessage {
				t.Errorf("type=%q level=%q message=%q, want http info %q", b.Type, b.Level, b.Message, wantMessage)
			}
			if b.Attributes["method"] != "POST" || b.Attributes["host"] != host || b.Attributes["status"] != float64(201) {
				t.Errorf("attributes = %v", b.Attributes)
			}
			if d, _ := b.Attributes["duration_ms"].(float64); d < 30 {
				t.Errorf("duration_ms = %v, want at least 30", b.Attributes["duration_ms"])
			}
			path, hasPath := b.Attributes["path"]
			query, hasQuery := b.Attributes["query"]
			if pii {
				if path != "/items" || query != "token=secret" {
					t.Errorf("path=%v query=%v", path, query)
				}
			} else if hasPath || hasQuery || strings.Contains(b.Message, "/items") {
				t.Errorf("path or query recorded without SendDefaultPII: %+v", b)
			}
		})
	}
}

func TestTransportWithoutScopeRecordsNothing(t *testing.T) {
	client, cap := newCaptureClient(t)
	bt.SetDefaultClient(client)
	t.Cleanup(func() { bt.SetDefaultClient(nil) })
	var got *http.Request
	want := &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}
	tr := NewTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r
		return want, nil
	}), TransportOptions{})

	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil || resp != want || got != req {
		t.Errorf("resp=%p err=%v forwarded=%p, want the base's response for the same request", resp, err, got)
	}
	client.Report("no scope", nil)
	flushReports(t, client)
	if crumbs := cap.last().Breadcrumbs; len(crumbs) != 0 {
		t.Errorf("breadcrumbs = %+v, want none", crumbs)
	}
}

func TestTransportRecordsErrors(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		client, cap := newCaptureClient(t)
		dead := httptest.NewServer(http.NotFoundHandler())
		deadURL := dead.URL
		dead.Close()

		ctx := bt.WithScope(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, deadURL+"/x", nil)
		hc := &http.Client{Transport: NewTransport(&http.Transport{}, TransportOptions{})}
		if _, err := do(hc, req); err == nil {
			t.Fatal("request to a closed server succeeded")
		}
		crumbs := scopeBreadcrumbs(t, client, cap, ctx)
		if len(crumbs) != 1 {
			t.Fatalf("breadcrumbs = %+v", crumbs)
		}
		b := crumbs[0]
		if text, _ := b.Attributes["error"].(string); text == "" || b.Level != bt.BreadcrumbWarning {
			t.Errorf("level=%q error=%v", b.Level, b.Attributes["error"])
		}
		if _, ok := b.Attributes["status"]; ok || !strings.Contains(b.Message, " failed: ") {
			t.Errorf("breadcrumb = %+v", b)
		}
	})

	urlErr := &url.Error{Op: "Get", URL: "http://upstream.test/secret?token=x", Err: errors.New("upstream refused")}
	for _, tc := range []struct {
		name string
		err  error
		pii  bool
		want string
	}{
		{"url.Error without PII", urlErr, false, "upstream refused"},
		{"url.Error with PII", urlErr, true, urlErr.Error()},
		{"panicking Error", panickingError{}, false, "error text unavailable"},
		{"long Error", errors.New(strings.Repeat("e", 1023) + "é"), false, strings.Repeat("e", 1023)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, cap := newCaptureClient(t)
			tr := NewTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, tc.err
			}), TransportOptions{SendDefaultPII: tc.pii})
			ctx := bt.WithScope(context.Background())
			req := httptest.NewRequest(http.MethodGet, "http://upstream.test/secret?token=x", nil).WithContext(ctx)
			if _, err := tr.RoundTrip(req); err != tc.err {
				t.Errorf("RoundTrip error = %v, want the base's error", err)
			}
			crumbs := scopeBreadcrumbs(t, client, cap, ctx)
			if len(crumbs) != 1 || crumbs[0].Attributes["error"] != tc.want {
				t.Errorf("breadcrumbs = %+v, want error %q", crumbs, tc.want)
			}
		})
	}
}

func TestTransportCapsFields(t *testing.T) {
	client, cap := newCaptureClient(t)
	tr := NewTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}), TransportOptions{SendDefaultPII: true})
	ctx := bt.WithScope(context.Background())
	longHost := strings.Repeat("h", 300)
	req := httptest.NewRequest(http.MethodGet, "http://upstream.test/", nil).WithContext(ctx)
	req.Method = strings.Repeat("M", 100)
	req.URL.Host = longHost
	req.URL.Path = "/" + strings.Repeat("p", 2000)
	req.URL.RawQuery = strings.Repeat("q", 2000)
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	splitReq := httptest.NewRequest(http.MethodGet, "http://upstream.test/", nil).WithContext(ctx)
	splitReq.URL.Host = strings.Repeat("h", 255) + "é"
	splitReq.URL.Path = "/" + strings.Repeat("p", 1022) + "é"
	splitReq.URL.RawQuery = strings.Repeat("q", 1023) + "é"
	if _, err := tr.RoundTrip(splitReq); err != nil {
		t.Fatal(err)
	}

	crumbs := scopeBreadcrumbs(t, client, cap, ctx)
	if len(crumbs) != 2 {
		t.Fatalf("breadcrumbs = %+v", crumbs)
	}
	a := crumbs[0].Attributes
	if a["method"] != strings.Repeat("M", 64) || a["host"] != longHost[:256] {
		t.Errorf("method=%v host=%v", a["method"], a["host"])
	}
	if p, _ := a["path"].(string); len(p) != 1024 {
		t.Errorf("path length = %d, want 1024", len(p))
	}
	if q, _ := a["query"].(string); len(q) != 1024 {
		t.Errorf("query length = %d, want 1024", len(q))
	}
	if a["status"] != float64(200) {
		t.Errorf("status = %v", a["status"])
	}

	split := crumbs[1].Attributes
	if split["host"] != strings.Repeat("h", 255) {
		t.Errorf("host = %v, want the cap without the split rune", split["host"])
	}
	if split["path"] != "/"+strings.Repeat("p", 1022) {
		t.Errorf("path = %v, want the cap without the split rune", split["path"])
	}
	if split["query"] != strings.Repeat("q", 1023) {
		t.Errorf("query = %v, want the cap without the split rune", split["query"])
	}
	if strings.ContainsRune(crumbs[1].Message, utf8.RuneError) {
		t.Errorf("message = %q", crumbs[1].Message)
	}
}

func TestTransportCloseIdleConnections(t *testing.T) {
	base := &idleCloser{}
	hc := &http.Client{Transport: NewTransport(base, TransportOptions{})}
	hc.CloseIdleConnections()
	if base.closed != 1 {
		t.Errorf("base CloseIdleConnections calls = %d, want 1", base.closed)
	}
	(&http.Client{Transport: NewTransport(roundTripFunc(nil), TransportOptions{})}).CloseIdleConnections()
}

func TestTransportEmptyMethodIsGet(t *testing.T) {
	client, cap := newCaptureClient(t)
	tr := NewTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}), TransportOptions{})
	ctx := bt.WithScope(context.Background())
	req := httptest.NewRequest(http.MethodGet, "http://upstream.test/", nil).WithContext(ctx)
	req.Method = ""
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	crumbs := scopeBreadcrumbs(t, client, cap, ctx)
	if len(crumbs) != 1 || crumbs[0].Message != "GET upstream.test 200" || crumbs[0].Attributes["method"] != "GET" {
		t.Fatalf("breadcrumbs = %+v", crumbs)
	}
}

func TestTransportBaseWithoutResponseOrError(t *testing.T) {
	client, cap := newCaptureClient(t)
	tr := NewTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, nil
	}), TransportOptions{})
	ctx := bt.WithScope(context.Background())
	req := httptest.NewRequest(http.MethodDelete, "http://upstream.test/", nil).WithContext(ctx)
	if resp, err := tr.RoundTrip(req); resp != nil || err != nil {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	crumbs := scopeBreadcrumbs(t, client, cap, ctx)
	if len(crumbs) != 1 || crumbs[0].Message != "DELETE upstream.test" {
		t.Fatalf("breadcrumbs = %+v", crumbs)
	}
	if _, ok := crumbs[0].Attributes["status"]; ok {
		t.Errorf("status recorded without a response: %v", crumbs[0].Attributes)
	}
}

func TestTransportWithNilBaseAsDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = NewTransport(nil, TransportOptions{})

	client, cap := newCaptureClient(t)
	client.Report(errors.New("sent through the wrapped default transport"), nil)
	flushReports(t, client)
	if cap.count() != 1 || client.Stats().Delivered != 1 {
		t.Fatalf("reports = %d, stats = %+v, want one delivered report", cap.count(), client.Stats())
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(upstream.Close)
	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	http.DefaultClient.CloseIdleConnections()
}
