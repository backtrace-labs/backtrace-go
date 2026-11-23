package bt

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errFakeNetwork = errors.New("fake network failure")

const fakeURL = "http://backtrace.invalid/post?format=json&token=secret-token"

type rtStep struct {
	status    int
	err       error
	wroteBody bool
}

type scriptedTransport struct {
	mu      sync.Mutex
	steps   []rtStep
	bodies  [][]byte
	headers []http.Header
}

func (s *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	s.mu.Lock()
	step := s.steps[min(len(s.bodies), len(s.steps)-1)]
	s.bodies = append(s.bodies, body)
	s.headers = append(s.headers, req.Header.Clone())
	s.mu.Unlock()
	// net/http calls WroteRequest once the whole body went out; send relies on it.
	if step.err == nil || step.wroteBody {
		if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
	}
	if step.err != nil {
		return nil, step.err
	}
	return &http.Response{
		StatusCode: step.status,
		Status:     fmt.Sprintf("%d %s", step.status, http.StatusText(step.status)),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"response":"ok"}`)),
		Request:    req,
	}, nil
}

func (s *scriptedTransport) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func newScriptedTransport(steps ...rtStep) (*httpTransport, *scriptedTransport) {
	rt := &scriptedTransport{steps: steps}
	tr := newHTTPTransport(&http.Client{Transport: rt}, time.Second)
	tr.backoff = noBackoff
	return tr, rt
}

type waitLog struct{ waits []time.Duration }

func (w *waitLog) wait(d time.Duration) bool {
	w.waits = append(w.waits, d)
	return true
}

var testPayload = payload{body: []byte(`{"uuid":"same-uuid"}`), contentType: "application/json"}

func TestRetryDecisions(t *testing.T) {
	type tc struct {
		name     string
		steps    []rtStep
		retries  int
		attempts int
		reason   dropReason
		ok       bool
		warned   bool
	}
	cases := []tc{
		{name: "network error before the body is retried", steps: []rtStep{{err: errFakeNetwork}}, retries: 2, attempts: 3, reason: dropNetwork},
		{name: "network error after the body is not retried", steps: []rtStep{{err: errFakeNetwork, wroteBody: true}}, retries: 2, attempts: 1, reason: dropNetwork},
		{name: "network error then success", steps: []rtStep{{err: errFakeNetwork}, {status: 200}}, retries: 2, attempts: 2, ok: true},
		{name: "503 then success", steps: []rtStep{{status: 503}, {status: 200}}, retries: 2, attempts: 2, ok: true},
		{name: "429 pauses and is not retried", steps: []rtStep{{status: 429}}, retries: 2, attempts: 1, reason: dropRateLimit},
		{name: "405 is not retried", steps: []rtStep{{status: 405}}, retries: 2, attempts: 1, reason: dropServerReject},
		{name: "retries disabled", steps: []rtStep{{status: 503}}, retries: 0, attempts: 1, reason: dropServerReject},
	}
	for _, code := range []int{408, 500, 502, 503, 504} {
		cases = append(cases, tc{name: fmt.Sprintf("%d is retried", code), steps: []rtStep{{status: code}},
			retries: 2, attempts: 3, reason: dropServerReject})
	}
	for _, code := range []int{400, 401, 403, 404, 413} {
		cases = append(cases, tc{name: fmt.Sprintf("%d is permanent", code), steps: []rtStep{{status: code}},
			retries: 2, attempts: 1, reason: dropServerReject, warned: true})
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, rt := newScriptedTransport(c.steps...)
			logs := &lineLogger{}
			var w waitLog
			res := tr.send(context.Background(), fakeURL, testPayload, c.retries, w.wait, diag{logger: logs})

			if rt.calls() != c.attempts {
				t.Errorf("attempts = %d, want %d", rt.calls(), c.attempts)
			}
			if res.retries != c.attempts-1 || len(w.waits) != c.attempts-1 {
				t.Errorf("retries = %d, waits = %d, want %d", res.retries, len(w.waits), c.attempts-1)
			}
			if c.ok {
				if res.err != nil {
					t.Errorf("send = %+v, want delivery", res)
				}
			} else if res.err == nil || res.reason != c.reason {
				t.Errorf("send = (%v, %v), want reason %v", res.reason, res.err, c.reason)
			}
			for i, body := range rt.bodies {
				if !bytes.Equal(body, testPayload.body) {
					t.Errorf("attempt %d sent %q, want the same body (and UUID) every time", i, body)
				}
			}
			lines := logs.all()
			if warned := len(lines) == 1; warned != c.warned || len(lines) > 1 {
				t.Errorf("warnings without Debug = %q, want warned=%v", lines, c.warned)
			}
			for _, line := range lines {
				if strings.Contains(line, "secret-token") || !strings.Contains(line, "(destination http://backtrace.invalid)") {
					t.Errorf("warning shows more than the destination's scheme and host: %q", line)
				}
			}
		})
	}
}

func TestRetryWaitBudget(t *testing.T) {
	tr, rt := newScriptedTransport(rtStep{status: 503})
	tr.backoff = func(int) time.Duration { return 25 * time.Second }
	var w waitLog
	res := tr.send(context.Background(), fakeURL, testPayload, 100, w.wait, diag{})

	want := []time.Duration{25 * time.Second, 25 * time.Second, 10 * time.Second}
	if !slices.Equal(w.waits, want) {
		t.Errorf("waits = %v, want %v (60s total per report)", w.waits, want)
	}
	if rt.calls() != 4 || res.retries != 3 || res.reason != dropServerReject {
		t.Errorf("attempts = %d, result = %+v, want 4 attempts and a ServerReject drop", rt.calls(), res)
	}
}

func TestFullJitterBounds(t *testing.T) {
	for retry := 0; retry <= 64; retry++ {
		ceiling := retryBackoffCap
		if retry < 5 {
			ceiling = retryBackoffBase << retry
		}
		var largest time.Duration
		for i := 0; i < 200; i++ {
			d := fullJitter(retry)
			if d < 0 || d >= ceiling {
				t.Fatalf("fullJitter(%d) = %v, want [0, %v)", retry, d, ceiling)
			}
			largest = max(largest, d)
		}
		if largest < ceiling/2 {
			t.Errorf("fullJitter(%d) never exceeded %v in 200 draws; not spread over [0, %v)", retry, ceiling/2, ceiling)
		}
	}
}

func TestFailureCircuitOpensAfterConsecutiveNetworkFailures(t *testing.T) {
	tr, rt := newScriptedTransport(rtStep{err: errFakeNetwork})
	for i := 0; i < circuitThreshold; i++ {
		if res := tr.send(context.Background(), fakeURL, testPayload, 0, (&waitLog{}).wait, diag{}); res.reason != dropNetwork {
			t.Fatalf("send %d reason = %v, want dropNetwork", i, res.reason)
		}
	}
	reason, err := tr.pauseState()
	if reason != dropNetwork || err != errCircuitOpen {
		t.Fatalf("pauseState = (%v, %v), want open circuit", reason, err)
	}
	tr.mu.Lock()
	remaining := time.Until(tr.pauseUntil)
	tr.mu.Unlock()
	if remaining <= circuitPause-5*time.Second || remaining > circuitPause {
		t.Errorf("circuit pause = %v, want about %v", remaining, circuitPause)
	}
	res := tr.send(context.Background(), fakeURL, testPayload, 0, (&waitLog{}).wait, diag{})
	if res.err != errCircuitOpen || rt.calls() != circuitThreshold {
		t.Errorf("send while open = %v after %d calls, want errCircuitOpen and no request", res.err, rt.calls())
	}
}

func TestFailureCircuitStopsRetries(t *testing.T) {
	tr, rt := newScriptedTransport(rtStep{err: errFakeNetwork})
	var w waitLog
	res := tr.send(context.Background(), fakeURL, testPayload, 10, w.wait, diag{})
	if rt.calls() != circuitThreshold || res.err != errCircuitOpen || res.reason != dropNetwork {
		t.Errorf("attempts = %d, result = (%v, %v), want %d attempts then the open circuit",
			rt.calls(), res.reason, res.err, circuitThreshold)
	}
	if res.retries != circuitThreshold-1 || len(w.waits) != circuitThreshold-1 {
		t.Errorf("retries = %d, waits = %d, want %d each (no wait or retry after the circuit opened)",
			res.retries, len(w.waits), circuitThreshold-1)
	}
}

func TestPauseDuringRetryWaitIsNotARetry(t *testing.T) {
	tr, rt := newScriptedTransport(rtStep{status: 503})
	waits := 0
	pauseWhileWaiting := func(time.Duration) bool {
		waits++
		tr.pause(time.Minute, dropRateLimit, errRateLimited)
		return true
	}
	res := tr.send(context.Background(), fakeURL, testPayload, 2, pauseWhileWaiting, diag{})
	if rt.calls() != 1 || waits != 1 || res.retries != 0 || res.reason != dropRateLimit || res.err != errRateLimited {
		t.Errorf("attempts = %d, waits = %d, result = %+v, want one attempt, one wait, no retry and the rate-limit pause",
			rt.calls(), waits, res)
	}
}

func TestResponseResetsFailureCircuit(t *testing.T) {
	fail := rtStep{err: errFakeNetwork}
	tr, _ := newScriptedTransport(fail, fail, fail, fail, rtStep{status: 500}, fail, fail, fail, fail, fail)
	for i := 0; i < 9; i++ {
		tr.send(context.Background(), fakeURL, testPayload, 0, (&waitLog{}).wait, diag{})
	}
	if _, err := tr.pauseState(); err != nil {
		t.Fatalf("paused after 4+4 failures split by a response: %v", err)
	}
	tr.send(context.Background(), fakeURL, testPayload, 0, (&waitLog{}).wait, diag{})
	if _, err := tr.pauseState(); err != errCircuitOpen {
		t.Errorf("pauseState after 5 consecutive failures = %v, want errCircuitOpen", err)
	}
}

type blockingTransport struct {
	wroteBody bool
	entered   chan struct{}
	calls     atomic.Int32
}

func (b *blockingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b.calls.Add(1)
	_, _ = io.Copy(io.Discard, req.Body)
	if trace := httptrace.ContextClientTrace(req.Context()); b.wroteBody && trace != nil && trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestRootCancellationCountsAsClosed(t *testing.T) {
	rt := &blockingTransport{entered: make(chan struct{}, 1)}
	tr := newHTTPTransport(&http.Client{Transport: rt}, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-rt.entered
		cancel()
	}()
	res := tr.send(ctx, fakeURL, testPayload, 2, (&waitLog{}).wait, diag{})
	if res.reason != dropClosed || res.retries != 0 || rt.calls.Load() != 1 {
		t.Errorf("send = %+v after %d calls, want one attempt dropped as Closed", res, rt.calls.Load())
	}
	tr.mu.Lock()
	failures := tr.failures
	tr.mu.Unlock()
	if failures != 0 {
		t.Errorf("cancellation counted as %d network failures", failures)
	}

	tr2, _ := newScriptedTransport(rtStep{status: 503})
	stop := func(time.Duration) bool { return false }
	if res := tr2.send(context.Background(), fakeURL, testPayload, 2, stop, diag{}); res.reason != dropClosed {
		t.Errorf("send with an aborted retry wait = %v, want dropClosed", res.reason)
	}
}

func TestAttemptTimeoutRetriedOnlyBeforeBodySent(t *testing.T) {
	for _, wrote := range []bool{false, true} {
		rt := &blockingTransport{wroteBody: wrote}
		tr := newHTTPTransport(&http.Client{Transport: rt}, 20*time.Millisecond)
		tr.backoff = noBackoff
		res := tr.send(context.Background(), fakeURL, testPayload, 1, (&waitLog{}).wait, diag{})
		want := int32(2)
		if wrote {
			want = 1
		}
		if rt.calls.Load() != want || res.reason != dropNetwork {
			t.Errorf("wroteBody=%v: attempts = %d, reason = %v, want %d attempts and dropNetwork",
				wrote, rt.calls.Load(), res.reason, want)
		}
	}
}

func TestRequestHeaders(t *testing.T) {
	tr, rt := newScriptedTransport(rtStep{status: 200})
	wait := (&waitLog{}).wait
	tr.send(context.Background(), fakeURL, payload{body: make([]byte, expectContinueBytes), contentType: "application/json", gzipped: true}, 0, wait, diag{})
	tr.send(context.Background(), fakeURL, payload{body: make([]byte, expectContinueBytes-1), contentType: "application/json"}, 0, wait, diag{})

	big, small := rt.headers[0], rt.headers[1]
	if big.Get("Expect") != "100-continue" || big.Get("Content-Encoding") != "gzip" {
		t.Errorf("256 KiB gzip body headers = %v, want Expect: 100-continue and Content-Encoding: gzip", big)
	}
	if small.Get("Expect") != "" || small.Get("Content-Encoding") != "" {
		t.Errorf("small plain body headers = %v, want no Expect or Content-Encoding", small)
	}
	if small.Get("Content-Type") != "application/json" || !strings.HasPrefix(small.Get("User-Agent"), "backtrace-go/") {
		t.Errorf("headers = %v", small)
	}
}

func gunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	return out
}

func TestBuildPayloadCompression(t *testing.T) {
	body := []byte(`{"uuid":"abc","lang":"go"}`)
	p, err := buildPayload(body, nil, nil, multipartLimits{}, true, new(atomic.Bool), diag{})
	if err != nil || !p.gzipped || p.contentType != "application/json" || !bytes.Equal(gunzip(t, p.body), body) {
		t.Errorf("compressed JSON payload = %+v, %v", p, err)
	}
	p, err = buildPayload(body, nil, nil, multipartLimits{}, false, new(atomic.Bool), diag{})
	if err != nil || p.gzipped || !bytes.Equal(p.body, body) {
		t.Errorf("uncompressed JSON payload = %+v, %v", p, err)
	}
	limits := multipartLimits{maxAttachments: 1, maxAttachmentBytes: 1 << 10, maxTotalBytes: 1 << 10}
	p, err = buildPayload(body, nil, []inlinePart{{name: "bt-breadcrumbs-0", data: []byte("[]")}}, limits, true, new(atomic.Bool), diag{})
	if err != nil || !p.gzipped || !strings.HasPrefix(p.contentType, "multipart/form-data") {
		t.Fatalf("multipart payload = %+v, %v", p, err)
	}
	plain := gunzip(t, p.body)
	if !bytes.Contains(plain, []byte(`name="upload_file"`)) || !bytes.Contains(plain, body) {
		t.Errorf("multipart body does not carry the report: %q", plain)
	}
	if !bytes.Contains(plain, []byte(`name="attachment_bt-breadcrumbs-0"`)) {
		t.Errorf("multipart body does not carry the breadcrumb part: %q", plain)
	}
}

func TestCompressedSubmission(t *testing.T) {
	c, rs := newTestClient(t, nil)
	c.ReportMessage("zipped", nil)
	if attrsOf(t, flushOne(t, c, rs))["error.message"] != "zipped" {
		t.Error("gzip payload not decoded")
	}
	if got := rs.lastHeader().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip by default", got)
	}

	c, rs = newTestClient(t, func(cfg *Config) { cfg.DisableCompression = true })
	c.ReportMessage("plain", nil)
	if attrsOf(t, flushOne(t, c, rs))["error.message"] != "plain" {
		t.Error("plain payload not received")
	}
	if got := rs.lastHeader().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q with DisableCompression", got)
	}
}

func TestLegacyOptionsDisableCompression(t *testing.T) {
	old := Options.DisableCompression
	t.Cleanup(func() { Options.DisableCompression = old })
	Options.DisableCompression = true
	if !optionsToConfig().DisableCompression {
		t.Error("Options.DisableCompression not carried into the legacy config")
	}
}

func TestRetriedReportKeepsUUID(t *testing.T) {
	c, rs := newTestClient(t, nil)
	rs.mu.Lock()
	rs.statuses = []int{503, 502}
	rs.mu.Unlock()

	c.ReportMessage("retried", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if rs.count() != 3 {
		t.Fatalf("attempts received = %d, want 3", rs.count())
	}
	rs.mu.Lock()
	uuids := map[any]bool{}
	for _, p := range rs.payloads {
		uuids[p["uuid"]] = true
	}
	rs.mu.Unlock()
	if len(uuids) != 1 {
		t.Errorf("attempts carried %d distinct UUIDs, want 1", len(uuids))
	}
	if s := c.Stats(); s.Delivered != 1 || s.Retries != 2 || s.ServerReject != 0 || c.DroppedReports() != 0 {
		t.Errorf("stats = %+v, want Delivered 1, Retries 2, no drops", s)
	}
}

func TestPermanentRejectionLoggedOnceWithoutDebug(t *testing.T) {
	logs := &lineLogger{}
	c, rs := newTestClient(t, func(cfg *Config) { cfg.Logger = logs })
	rs.mu.Lock()
	rs.status = http.StatusForbidden
	rs.mu.Unlock()

	for i := 0; i < 3; i++ {
		c.ReportMessage("forbidden", nil)
	}
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if rs.count() != 3 {
		t.Errorf("requests = %d, want 3 (403 is not retried)", rs.count())
	}
	if s := c.Stats(); s.ServerReject != 3 || s.Retries != 0 {
		t.Errorf("stats = %+v, want ServerReject 3, Retries 0", s)
	}
	lines := logs.all()
	if len(lines) != 1 {
		t.Fatalf("log lines = %q, want exactly one warning", lines)
	}
	if !strings.Contains(lines[0], "403") || strings.Contains(lines[0], "client-test-token") {
		t.Errorf("warning = %q, want the status and no token", lines[0])
	}
}

func TestPrefixedEndpointTokenStaysOutOfLogs(t *testing.T) {
	const token = "51cc8e69c5b62fa8c72dc963e730f1e851cc8e69c5b62fa8c72dc963e730f1e8"
	logs := &lineLogger{}
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.Endpoint += "/backtrace/universe/" + token + "/json"
		cfg.Token = ""
		cfg.Debug = true
		cfg.Logger = logs
	})
	rs.mu.Lock()
	rs.status = http.StatusForbidden
	rs.mu.Unlock()

	c.ReportMessage("forbidden", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	lines := logs.all()
	if len(lines) < 2 {
		t.Fatalf("log lines = %q, want the debug send line and the warning", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, token) {
			t.Errorf("token in log line %q", line)
		}
	}
}

func TestRedirects(t *testing.T) {
	newRedirector := func(t *testing.T, code int, target string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target+r.URL.RequestURI(), code)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	newClient := func(t *testing.T, endpoint string, hc *http.Client) *Client {
		c, err := NewClient(Config{Endpoint: endpoint, Token: "redirect-token", HTTPClient: hc, Logger: &lineLogger{}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	targetRequests := func(rs *recordingServer) int {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		return rs.requests
	}

	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprintf("SDK client refuses %d", code), func(t *testing.T) {
			target := newRecordingServer()
			t.Cleanup(target.srv.Close)
			c := newClient(t, newRedirector(t, code, target.srv.URL), nil)
			c.ReportMessage("redirected", nil)
			if !c.Flush(5 * time.Second) {
				t.Fatal("Flush timed out")
			}
			if s := c.Stats(); s.Delivered != 0 || s.ServerReject != 1 || s.Retries != 0 {
				t.Errorf("stats = %+v, want one ServerReject", s)
			}
			if n := targetRequests(target); n != 0 {
				t.Errorf("redirect target got %d requests", n)
			}
		})
	}
	t.Run("supplied client does not follow either", func(t *testing.T) {
		target := newRecordingServer()
		t.Cleanup(target.srv.Close)
		hc := &http.Client{}
		c := newClient(t, newRedirector(t, 301, target.srv.URL), hc)
		c.ReportMessage("redirected", nil)
		if !c.Flush(5 * time.Second) {
			t.Fatal("Flush timed out")
		}
		if s := c.Stats(); s.Delivered != 0 || s.ServerReject != 1 {
			t.Errorf("stats = %+v, want one ServerReject", s)
		}
		if n := targetRequests(target); n != 0 {
			t.Errorf("redirect target got %d requests", n)
		}
		if hc.CheckRedirect != nil {
			t.Error("the supplied client was modified")
		}
	})
}

func TestSDKOwnedTransport(t *testing.T) {
	errDialed := errors.New("dialed")
	var deadline time.Time
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSHandshakeTimeout = time.Minute
	base.ResponseHeaderTimeout = 0
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		deadline, _ = ctx.Deadline()
		return nil, errDialed
	}
	orig := http.DefaultTransport
	http.DefaultTransport = base
	t.Cleanup(func() { http.DefaultTransport = orig })

	tr := newHTTPTransport(nil, time.Second)
	owned, _ := tr.client.Transport.(*http.Transport)
	if !tr.ownsClient || owned == nil || owned == base || tr.client.CheckRedirect == nil {
		t.Fatalf("SDK client does not use its own clone: %#v", tr.client)
	}
	if owned.TLSHandshakeTimeout != 10*time.Second || owned.ResponseHeaderTimeout != 20*time.Second || owned.Proxy == nil {
		t.Errorf("TLS handshake %v, response header %v, proxy set %v; want 10s, 20s, true",
			owned.TLSHandshakeTimeout, owned.ResponseHeaderTimeout, owned.Proxy != nil)
	}
	before := time.Now()
	if _, err := owned.DialContext(context.Background(), "tcp", "127.0.0.1:1"); err != errDialed {
		t.Fatalf("dial = %v, want the DefaultTransport dialer", err)
	}
	after := time.Now()
	if deadline.Before(before.Add(10*time.Second)) || deadline.After(after.Add(10*time.Second)) {
		t.Errorf("dial deadline in %v, want 10s", deadline.Sub(before))
	}
	if base.TLSHandshakeTimeout != time.Minute || base.ResponseHeaderTimeout != 0 {
		t.Error("http.DefaultTransport was modified")
	}
}

func getReusesConn(t *testing.T, client *http.Client, url string) (reused bool) {
	t.Helper()
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return reused
}

func TestCloseClosesOwnIdleConnections(t *testing.T) {
	c, rs := newTestClient(t, nil)
	c.ReportMessage("own transport", nil)
	flushOne(t, c, rs)
	if !getReusesConn(t, c.transport.client, rs.srv.URL) {
		t.Fatal("SDK transport did not keep its connection idle after a delivery")
	}
	c.Close()
	if getReusesConn(t, c.transport.client, rs.srv.URL) {
		t.Error("Client.Close left the SDK transport's idle connection open")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func holdMessages(t *testing.T, rs *recordingServer, msgs ...string) map[string]func() {
	t.Helper()
	release := map[string]func(){}
	rs.mu.Lock()
	rs.holds = map[string]chan struct{}{}
	for _, m := range msgs {
		ch := make(chan struct{})
		var once sync.Once
		rs.holds[m] = ch
		release[m] = func() { once.Do(func() { close(ch) }) }
		t.Cleanup(release[m])
	}
	rs.mu.Unlock()
	return release
}

func entered(t *testing.T, rs *recordingServer) {
	t.Helper()
	select {
	case <-rs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reached the server")
	}
}

func TestPriorityLaneJumpsQueue(t *testing.T) {
	c, rs := newTestClient(t, nil)
	release := holdMessages(t, rs, "hold-1", "hold-2")

	c.ReportMessage("hold-1", nil)
	entered(t, rs)
	c.ReportMessage("hold-2", nil)
	c.ReportMessage("regular-3", nil)

	done := make(chan bool, 1)
	ctx := timeoutCtx(t, 10*time.Second)
	go func() { done <- c.ReportPanicValueAndFlush(ctx, "urgent", nil) }()
	waitFor(t, "the panic report to be queued", func() bool { return c.Stats().Accepted == 4 })

	release["hold-1"]()
	entered(t, rs)
	if got := rs.messages(); !slices.Equal(got, []string{"hold-1", "urgent"}) {
		t.Errorf("delivery order = %q, want hold-1 then urgent", got)
	}
	select {
	case <-done:
		t.Fatal("ReportPanicValueAndFlush returned before the reports queued ahead of it")
	default:
	}

	release["hold-2"]()
	select {
	case ok := <-done:
		if !ok {
			t.Error("ReportPanicValueAndFlush = false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReportPanicValueAndFlush stuck after the backlog drained")
	}
	if got := rs.messages(); !slices.Equal(got, []string{"hold-1", "urgent", "hold-2", "regular-3"}) {
		t.Errorf("delivery order = %q", got)
	}
}

func TestPriorityBurstLetsRegularReportsThrough(t *testing.T) {
	c, rs := newTestClient(t, nil)
	release := holdMessages(t, rs, "hold")
	c.ReportMessage("hold", nil)
	entered(t, rs)
	c.ReportMessage("regular", nil)

	want := []string{"hold"}
	for i := 0; i < priorityBurst+2; i++ {
		msg := fmt.Sprintf("panic-%d", i)
		in := panicCaptureInput(context.Background(), msg, nil)
		in.lane = lanePriority
		if _, ok := c.capture(nonBlocking, in); !ok {
			t.Fatalf("priority report %d not accepted", i)
		}
		if i == priorityBurst {
			want = append(want, "regular")
		}
		want = append(want, msg)
	}

	release["hold"]()
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if got := rs.messages(); !slices.Equal(got, want) {
		t.Errorf("delivery order = %q, want %q", got, want)
	}
}

func TestRetryResumesBeforeQueuedPriorityReports(t *testing.T) {
	c, rs := newTestClient(t, nil)
	release := holdMessages(t, rs, "retrying")
	rs.mu.Lock()
	rs.statuses = []int{http.StatusServiceUnavailable}
	rs.mu.Unlock()

	c.ReportMessage("retrying", nil)
	entered(t, rs)
	want := []string{"retrying", "retrying"}
	for i := 0; i < 3; i++ {
		msg := fmt.Sprintf("panic-%d", i)
		in := panicCaptureInput(context.Background(), msg, nil)
		in.lane = lanePriority
		if _, ok := c.capture(nonBlocking, in); !ok {
			t.Fatalf("priority report %d not accepted", i)
		}
		want = append(want, msg)
	}

	release["retrying"]()
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if got := rs.messages(); !slices.Equal(got, want) {
		t.Errorf("delivery order = %q, want %q (an elapsed backoff resumes before queued priority reports)", got, want)
	}
}

func TestFlushBarrierCoversBothLanes(t *testing.T) {
	t.Run("waits for an earlier priority report", func(t *testing.T) {
		c, rs := newTestClient(t, nil)
		release := holdMessages(t, rs, "regular", "urgent")
		c.ReportMessage("regular", nil)
		entered(t, rs)
		go c.ReportPanicValueAndFlush(timeoutCtx(t, 10*time.Second), "urgent", nil)
		waitFor(t, "the panic report to be queued", func() bool { return c.Stats().Accepted == 2 })

		flushed := make(chan bool, 1)
		go func() { flushed <- c.Flush(10 * time.Second) }()
		release["regular"]()
		entered(t, rs)
		select {
		case <-flushed:
			t.Fatal("Flush returned before the priority report accepted ahead of it")
		case <-time.After(100 * time.Millisecond):
		}
		release["urgent"]()
		select {
		case ok := <-flushed:
			if !ok {
				t.Error("Flush = false although both lanes drained")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Flush stuck")
		}
	})
	t.Run("ignores a later priority report", func(t *testing.T) {
		c, rs := newTestClient(t, nil)
		release := holdMessages(t, rs, "regular", "urgent")
		c.ReportMessage("regular", nil)
		entered(t, rs)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		targets := c.acceptedTargets()
		flushed := make(chan bool, 1)
		go func() { flushed <- c.flushTargetContext(ctx, targets) }()
		go c.ReportPanicValueAndFlush(timeoutCtx(t, 10*time.Second), "urgent", nil)
		waitFor(t, "the panic report to be queued", func() bool { return c.Stats().Accepted == 2 })

		release["regular"]()
		select {
		case ok := <-flushed:
			if !ok {
				t.Error("Flush = false although its backlog drained")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Flush waited for a priority report accepted after the call")
		}
		release["urgent"]()
	})
}

func TestRetryWaitRunsPriorityReports(t *testing.T) {
	c, rs := newTestClient(t, nil)
	c.transport.backoff = func(int) time.Duration { return time.Minute }
	rs.mu.Lock()
	rs.statuses = []int{http.StatusServiceUnavailable}
	rs.mu.Unlock()

	c.ReportMessage("retrying", nil)
	waitFor(t, "the first attempt", func() bool { return rs.count() == 1 })

	start := time.Now()
	if !c.ReportPanicValueAndFlush(timeoutCtx(t, time.Second), "urgent", nil) {
		t.Fatal("panic report not delivered during the retry wait")
	}
	if time.Since(start) < time.Second {
		t.Error("ReportPanicValueAndFlush returned before its deadline while an older report was still pending")
	}
	if got := rs.messages(); !slices.Equal(got, []string{"retrying", "urgent"}) {
		t.Errorf("delivery order = %q", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if c.CloseContext(ctx) {
		t.Error("CloseContext completed while a report was waiting a minute to retry")
	}
	select {
	case <-c.workerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	if s := c.Stats(); s.Delivered != 1 || s.Closed != 1 || s.ServerReject != 0 || s.Retries != 0 {
		t.Errorf("stats = %+v, want Delivered 1, Closed 1", s)
	}
}

type countingConn struct {
	net.Conn
	written *atomic.Int64
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

func TestExpectContinueSkipsUploadOnRejection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "noise.bin")
	noise := make([]byte, 1<<20)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, noise, 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, handler http.HandlerFunc) (int64, ClientStats) {
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		var written atomic.Int64
		base := http.DefaultTransport.(*http.Transport).Clone()
		base.ExpectContinueTimeout = 5 * time.Second
		base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &countingConn{Conn: conn, written: &written}, nil
		}
		c, err := NewClient(Config{
			Endpoint:        srv.URL,
			Token:           "expect-token",
			HTTPClient:      &http.Client{Transport: base},
			AttachmentPaths: []string{path},
			Logger:          &lineLogger{},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		c.ReportMessage("large", nil)
		if !c.Flush(10 * time.Second) {
			t.Fatal("Flush timed out")
		}
		return written.Load(), c.Stats()
	}

	written, stats := run(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	if written >= expectContinueBytes || stats.ServerReject != 1 {
		t.Errorf("rejected upload wrote %d bytes (stats %+v), want headers only", written, stats)
	}

	written, stats = run(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	})
	if written < 1<<20 || stats.Delivered != 1 {
		t.Errorf("accepted upload wrote %d bytes (stats %+v), want the whole body", written, stats)
	}
}

func TestPriorityReportsStayInOrderAcrossRetries(t *testing.T) {
	c, rs := newTestClient(t, nil)
	c.transport.backoff = func(int) time.Duration { return 300 * time.Millisecond }
	rs.mu.Lock()
	rs.statuses = []int{http.StatusServiceUnavailable}
	rs.mu.Unlock()

	first := make(chan bool, 1)
	ctx := timeoutCtx(t, 10*time.Second)
	go func() { first <- c.ReportPanicValueAndFlush(ctx, "p1", nil) }()
	waitFor(t, "the first attempt", func() bool { return rs.count() == 1 })
	if !c.ReportPanicValueAndFlush(timeoutCtx(t, 10*time.Second), "p2", nil) {
		t.Fatal("second panic report not delivered")
	}
	if !<-first {
		t.Error("first panic report not delivered")
	}
	if got := rs.messages(); !slices.Equal(got, []string{"p1", "p1", "p2"}) {
		t.Errorf("delivery order = %q, want p1 retried before p2", got)
	}
}
