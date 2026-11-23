package bt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAfterParsing(t *testing.T) {
	mk := func(value string) *http.Response {
		h := http.Header{}
		if value != "" {
			h.Set("Retry-After", value)
		}
		return &http.Response{Header: h}
	}

	if d := retryAfter(mk("30")); d != 30*time.Second {
		t.Errorf("seconds form = %v, want 30s", d)
	}
	if d := retryAfter(mk("")); d != rateLimitFallback {
		t.Errorf("missing header = %v, want fallback", d)
	}
	if d := retryAfter(mk("garbage")); d != rateLimitFallback {
		t.Errorf("garbage header = %v, want fallback", d)
	}
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if d := retryAfter(mk(future)); d < 80*time.Second || d > 91*time.Second {
		t.Errorf("http-date form = %v, want ~90s", d)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if d := retryAfter(mk(past)); d != 0 {
		t.Errorf("past http-date = %v, want 0", d)
	}
	// A hostile/malformed Retry-After must not disable reporting forever.
	if d := retryAfter(mk("2147483647")); d != rateLimitMaxPause {
		t.Errorf("huge seconds = %v, want cap %v", d, rateLimitMaxPause)
	}
	farFuture := time.Now().Add(24 * time.Hour * 365).UTC().Format(http.TimeFormat)
	if d := retryAfter(mk(farFuture)); d != rateLimitMaxPause {
		t.Errorf("far-future date = %v, want cap %v", d, rateLimitMaxPause)
	}
}

func TestTransportPause(t *testing.T) {
	tr := newHTTPTransport(nil, time.Second)
	if _, err := tr.pauseState(); err != nil {
		t.Error("fresh transport is paused")
	}
	tr.pause(time.Minute, dropRateLimit, errRateLimited)
	if reason, err := tr.pauseState(); err != errRateLimited || reason != dropRateLimit {
		t.Errorf("pauseState = (%v, %v), want (dropRateLimit, errRateLimited)", reason, err)
	}
	res := tr.send(context.Background(), "http://127.0.0.1:1/unused",
		payload{body: []byte("{}")}, 2, func(time.Duration) bool { return true }, diag{})
	if res.err != errRateLimited || res.reason != dropRateLimit {
		t.Errorf("send while paused = (%v, %v), want (dropRateLimit, errRateLimited)", res.reason, res.err)
	}
}

func TestURLOrigin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://example.com/post?format=json&token=supersecret", "https://example.com"},
		{"https://submit.backtrace.io/universe/secret-token/json", "https://submit.backtrace.io"},
		{"https://user:secretpw@host:6098/x", "https://host:6098"},
		{"https://[::1]:6098/post?token=t", "https://[::1]:6098"},
		{"submit.backtrace.io/universe/secret-token/json", "[invalid URL]"},
		{"https:submit.backtrace.io/universe/secret-token/json", "[invalid URL]"},
		{"http://%zz\x7f", "[invalid URL]"},
	}
	for _, c := range cases {
		if got := urlOrigin(c.in); got != c.want {
			t.Errorf("urlOrigin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSubmissionSecrets(t *testing.T) {
	hexTok := "51cc8e69c5b62fa8c72dc963e730f1e8"
	cases := []struct {
		url, token string
		want       []string
	}{
		{"https://submit.backtrace.io/universe/secret-token/json", "", []string{"secret-token"}},
		{"https://submit.backtrace.io/universe/secret-token", "", []string{"secret-token"}},
		{"https://submit.backtrace.io/universe/secret-token/json/extra", "", []string{"secret-token"}},
		{"https://errors.mycorp.com/universe/sometoken/json", "", []string{"sometoken"}},
		{"https://proxy.corp/backtrace/universe/sometoken/json", "", []string{"sometoken"}},
		{"https://errors.mycorp.com/universe/" + hexTok, "", []string{hexTok}},
		{"https://uni.sp.backtrace.io/post?format=json&token=a%26b", "", []string{"a%26b", "a&b"}},
		{"https://uni.sp.backtrace.io:6098", "cfg-token", []string{"cfg-token"}},
		{"https://uni.sp.backtrace.io/post?token=cfg-token", "cfg-token", []string{"cfg-token"}},
		{"https://submit.backtrace.io/uni/abcdefgh12345678/json", "abcdefgh", []string{"abcdefgh12345678", "abcdefgh"}},
		{"https://uni.sp.backtrace.io/api/post", "", nil},
		{"http://%zz", "cfg-token", []string{"cfg-token"}},
	}
	for _, c := range cases {
		if got := submissionSecrets(c.url, c.token); !slices.Equal(got, c.want) {
			t.Errorf("submissionSecrets(%q, %q) = %q, want %q", c.url, c.token, got, c.want)
		}
	}
}

func TestSanitizeHTTPError(t *testing.T) {
	raw := "https://uni.sp.backtrace.io/post?format=json&token=supersecret"
	err := errors.New(`Post "` + raw + `": context deadline exceeded`)
	out := sanitizeHTTPError(err, raw)
	if strings.Contains(out, "supersecret") {
		t.Errorf("token leaked through sanitized error: %q", out)
	}
	if !strings.Contains(out, "context deadline exceeded") {
		t.Errorf("error cause lost: %q", out)
	}

	pathRaw := "https://submit.backtrace.io/universe/secrettoken123/json"
	pathErr := errors.New(`Post "` + pathRaw + `": connection refused`)
	if out := sanitizeHTTPError(pathErr, pathRaw); strings.Contains(out, "secrettoken123") {
		t.Errorf("path token leaked through sanitized error: %q", out)
	}

	prefixed := "https://proxy.corp/backtrace/universe/prefixedtoken42/json"
	if out := sanitizeHTTPError(errors.New(`Post "`+prefixed+`": connection refused`), prefixed); out != `Post "https://proxy.corp": connection refused` {
		t.Errorf("sanitized error = %q, want only the scheme and host of the URL", out)
	}
	partial := errors.New("POST /backtrace/universe/prefixedtoken42/json: refused")
	if out := sanitizeHTTPError(partial, prefixed); out != "POST /backtrace/universe/REDACTED/json: refused" {
		t.Errorf("sanitized partial path = %q, want the token replaced", out)
	}
	spaced := "https://proxy.corp/back trace/universe/prefixedtoken42/json"
	reencoded := errors.New(`Post "https://proxy.corp/back%20trace/universe/prefixedtoken42/json": refused`)
	if out := sanitizeHTTPError(reencoded, spaced); out != `Post "https://proxy.corp": refused` {
		t.Errorf("sanitized re-encoded URL = %q, want only the scheme and host", out)
	}
}

// TestNoCrossOriginRedirect pins that a report is never replayed to another
// host: endpoint A returns a 307 to endpoint B, and B must see nothing.
func TestNoCrossOriginRedirect(t *testing.T) {
	var bHits atomic.Int32
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer serverB.Close()

	var aHits atomic.Int32
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits.Add(1)
		http.Redirect(w, r, serverB.URL, http.StatusTemporaryRedirect)
	}))
	defer serverA.Close()

	c, err := NewClient(Config{Endpoint: serverA.URL, Token: "redirect-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.ReportMessage("must not be replayed", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	if got := bHits.Load(); got != 0 {
		t.Errorf("redirect target received %d requests; report was replayed cross-origin", got)
	}
	if got := aHits.Load(); got != 1 {
		t.Errorf("origin received %d requests, want 1", got)
	}
	// A 3xx is a non-2xx response: the report is counted as rejected.
	if c.Stats().ServerReject != 1 {
		t.Errorf("redirect not counted as a server rejection: %+v", c.Stats())
	}

	// The transport error says what happened, without naming the target.
	tr := newHTTPTransport(nil, time.Second)
	err = sendOnce(tr, serverA.URL)
	if err == nil || !strings.Contains(err.Error(), "redirects are never followed") {
		t.Errorf("a refused redirect is reported as %v", err)
	}
	if err != nil && strings.Contains(err.Error(), strings.TrimPrefix(serverB.URL, "http://")) {
		t.Errorf("the error names the redirect target: %v", err)
	}
}

// TestCustomHTTPClientNotMutatedOrClosed pins ownership: the SDK applies its
// redirect policy to a copy and never closes a shared client's connections.
func TestCustomHTTPClientNotMutatedOrClosed(t *testing.T) {
	counting := &countingRoundTripper{next: http.DefaultTransport}
	shared := &http.Client{Transport: counting}
	tr := newHTTPTransport(shared, time.Second)

	if shared.CheckRedirect != nil {
		t.Error("caller's http.Client was mutated with a redirect policy")
	}
	if tr.client == shared {
		t.Error("caller's http.Client was used directly instead of a clone")
	}
	if tr.client.Transport != http.RoundTripper(counting) {
		t.Error("the copy does not send through the caller's transport")
	}
	if tr.ownsClient {
		t.Error("a caller-supplied client must not be owned by the SDK")
	}
	// Shutdown must leave the application's connections alone.
	tr.closeIdleConnections()
	if counting.closed.Load() != 0 {
		t.Error("the SDK closed idle connections of a caller-supplied client")
	}

	owned := newHTTPTransport(nil, time.Second)
	if !owned.ownsClient {
		t.Error("an SDK-created client should be owned")
	}
	if owned.client.CheckRedirect == nil {
		t.Error("SDK-created client has no redirect policy")
	}
}

// TestOwnedClientHasItsOwnConnections pins that the SDK's client does not
// share http.DefaultTransport: closing the SDK's idle connections at
// shutdown must leave the application's keep-alive connections alone.
func TestOwnedClientHasItsOwnConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	// applicationGet is unrelated application traffic on the process
	// default client; it reports whether its connection was a reused one.
	applicationGet := func() (reused bool) {
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		req, err := http.NewRequestWithContext(
			httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return reused
	}
	applicationGet() // leaves one idle connection in http.DefaultTransport
	if !applicationGet() {
		t.Fatal("test setup: the application's connection is not kept alive")
	}

	owned := newHTTPTransport(nil, time.Second)
	if err := sendOnce(owned, srv.URL); err != nil {
		t.Fatalf("send through the SDK's own client: %v", err)
	}
	if owned.client.Transport == nil || owned.client.Transport == http.DefaultTransport {
		t.Error("the SDK's own client uses the process default transport")
	}
	owned.closeIdleConnections()

	if !applicationGet() {
		t.Error("closing the SDK's idle connections closed the application's")
	}
}

// TestReplacedDefaultTransportIsNotOwned pins the fallback: when the
// application replaced http.DefaultTransport with something that cannot be
// copied, the SDK uses it and does not close its connections.
func TestReplacedDefaultTransportIsNotOwned(t *testing.T) {
	original := http.DefaultTransport
	counting := &countingRoundTripper{next: original}
	http.DefaultTransport = counting
	t.Cleanup(func() { http.DefaultTransport = original })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	tr := newHTTPTransport(nil, time.Second)
	if tr.ownsClient {
		t.Error("a transport shared with the process is reported as owned")
	}
	if err := sendOnce(tr, srv.URL); err != nil {
		t.Fatalf("send through the replaced default transport: %v", err)
	}
	if counting.trips.Load() != 1 {
		t.Errorf("replaced default transport saw %d requests, want 1", counting.trips.Load())
	}
	tr.closeIdleConnections()
	if counting.closed.Load() != 0 {
		t.Error("the SDK closed idle connections of a transport it does not own")
	}
}

type countingRoundTripper struct {
	next   http.RoundTripper
	trips  atomic.Int32
	closed atomic.Int32
}

func (c *countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	c.trips.Add(1)
	return c.next.RoundTrip(r)
}

func (c *countingRoundTripper) CloseIdleConnections() { c.closed.Add(1) }

func sendOnce(tr *httpTransport, url string) error {
	p := payload{body: []byte("{}"), contentType: "application/json"}
	return tr.send(context.Background(), url, p, 0, func(time.Duration) bool { return true }, diag{}).err
}

// multipartParts builds a submission and returns its attachment parts in
// the order they appear, as name -> content.
func multipartParts(t *testing.T, files []string, inline []inlinePart, limits multipartLimits) (names []string, content map[string]string) {
	t.Helper()
	body, contentType, err := buildMultipart([]byte(`{}`), files, inline, limits, new(atomic.Bool), diag{})
	if err != nil {
		t.Fatalf("buildMultipart: %v", err)
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("content type %q: %v", contentType, err)
	}
	content = map[string]string{}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading multipart body: %v", err)
		}
		data, _ := io.ReadAll(part)
		if part.FormName() == "upload_file" {
			continue
		}
		names = append(names, part.FormName())
		content[part.FormName()] = string(data)
	}
	return names, content
}

// TestInlinePartsYieldToFiles pins the budget order: the files the
// application attached are admitted first, and the SDK's own breadcrumb
// part only while the budgets allow.
func TestInlinePartsYieldToFiles(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(logPath, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	crumbs := []inlinePart{{name: "bt-breadcrumbs-0", data: []byte(`[{"id":0}]`)}}
	roomy := multipartLimits{maxAttachments: 16, maxAttachmentBytes: 1 << 20, maxTotalBytes: 1 << 20}

	names, _ := multipartParts(t, []string{logPath}, crumbs, roomy)
	if len(names) != 2 || names[0] != "attachment_app.log" || names[1] != "attachment_bt-breadcrumbs-0" {
		t.Errorf("parts with room for both = %v", names)
	}

	// Room for one part: the application's file, not the SDK's.
	oneSlot := roomy
	oneSlot.maxAttachments = 1
	names, _ = multipartParts(t, []string{logPath}, crumbs, oneSlot)
	if len(names) != 1 || names[0] != "attachment_app.log" {
		t.Errorf("with one slot the parts are %v, want the application's file only", names)
	}

	// Bytes for the file only: same outcome through the aggregate budget.
	tightBytes := roomy
	tightBytes.maxTotalBytes = 12
	names, _ = multipartParts(t, []string{logPath}, crumbs, tightBytes)
	if len(names) != 1 || names[0] != "attachment_app.log" {
		t.Errorf("with a 12-byte budget the parts are %v, want the application's file only", names)
	}

	// Without files the breadcrumb part still fits in one slot.
	names, _ = multipartParts(t, nil, crumbs, oneSlot)
	if len(names) != 1 || names[0] != "attachment_bt-breadcrumbs-0" {
		t.Errorf("breadcrumb part alone = %v", names)
	}
}

// TestInlinePartKeepsItsName pins that a file sharing the name of an SDK
// part is the one renamed: the server finds breadcrumbs by part name.
func TestInlinePartKeepsItsName(t *testing.T) {
	dir := t.TempDir()
	clash := filepath.Join(dir, "bt-breadcrumbs-0")
	if err := os.WriteFile(clash, []byte("a file, not breadcrumbs"), 0o644); err != nil {
		t.Fatal(err)
	}
	crumbs := []inlinePart{{name: "bt-breadcrumbs-0", data: []byte(`[{"id":0}]`)}}
	limits := multipartLimits{maxAttachments: 16, maxAttachmentBytes: 1 << 20, maxTotalBytes: 1 << 20}

	_, content := multipartParts(t, []string{clash}, crumbs, limits)
	if content["attachment_bt-breadcrumbs-0"] != `[{"id":0}]` {
		t.Errorf("the breadcrumb part was displaced: %v", content)
	}
	if content["attachment_bt-breadcrumbs-0_1"] != "a file, not breadcrumbs" {
		t.Errorf("the clashing file was not kept under a new name: %v", content)
	}
}

func breadcrumbPartLines(logs *lineLogger) []string {
	var out []string
	for _, line := range logs.all() {
		if strings.Contains(line, "bt-breadcrumbs-0") {
			out = append(out, line)
		}
	}
	return out
}

func reportWithBreadcrumb(t *testing.T, c *Client) {
	t.Helper()
	c.AddBreadcrumb(Breadcrumb{Message: "checkout started"})
	c.Report(errors.New("checkout failed"), nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("report not delivered")
	}
}

func writeAttachment(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInlinePartBudgetSkipWarnsOnce(t *testing.T) {
	dir := t.TempDir()
	small := writeAttachment(t, dir, "app.log", 10)
	large := writeAttachment(t, dir, "dump.bin", 1000)
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"files fill MaxAttachments", func(cfg *Config) {
			cfg.MaxAttachments = 1
			cfg.AttachmentPaths = []string{small}
		}},
		{"part alone exceeds MaxAttachmentBytes", func(cfg *Config) {
			cfg.MaxAttachmentBytes = 16
		}},
		{"files use up MaxTotalAttachmentBytes", func(cfg *Config) {
			cfg.MaxAttachmentBytes = 1024
			cfg.MaxTotalAttachmentBytes = 1024
			cfg.AttachmentPaths = []string{large}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lineLogger{}
			c, rs := newTestClient(t, func(cfg *Config) {
				cfg.Logger = logs
				tc.mutate(cfg)
			})

			reportWithBreadcrumb(t, c)
			lines := breadcrumbPartLines(logs)
			if len(lines) != 1 || !strings.HasSuffix(lines[0], "Later skips are logged only with Debug.") {
				t.Fatalf("warnings after the first report = %q, want one", lines)
			}

			reportWithBreadcrumb(t, c)
			if lines := breadcrumbPartLines(logs); len(lines) != 1 {
				t.Errorf("warnings after the second report = %q, want the first one only", lines)
			}
			if _, ok := rs.lastAttachments()["attachment_bt-breadcrumbs-0"]; ok {
				t.Error("the breadcrumb part was sent despite the budget")
			}
		})
	}
}

func TestInlinePartSkipStaysQuiet(t *testing.T) {
	path := writeAttachment(t, t.TempDir(), "app.log", 10)
	cases := []struct {
		name   string
		mutate func(*Config)
		sent   bool
	}{
		{"attachments disabled", func(cfg *Config) { cfg.MaxAttachments = -1 }, false},
		{"room for everything", func(*Config) {}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lineLogger{}
			c, rs := newTestClient(t, func(cfg *Config) {
				cfg.Logger = logs
				cfg.AttachmentPaths = []string{path}
				tc.mutate(cfg)
			})
			reportWithBreadcrumb(t, c)
			if lines := breadcrumbPartLines(logs); len(lines) != 0 {
				t.Errorf("logged without Debug: %q", lines)
			}
			if _, ok := rs.lastAttachments()["attachment_bt-breadcrumbs-0"]; ok != tc.sent {
				t.Errorf("breadcrumb part sent = %t, want %t", ok, tc.sent)
			}
		})
	}
}

func TestInlinePartLaterSkipsLogWithDebug(t *testing.T) {
	logs := &lineLogger{}
	d := diag{logger: logs, debug: true}
	warned := new(atomic.Bool)
	crumbs := []inlinePart{{name: "bt-breadcrumbs-0", data: []byte(`[{"id":0}]`)}}
	limits := multipartLimits{maxAttachments: 16, maxAttachmentBytes: 4, maxTotalBytes: 1 << 20}
	for range 2 {
		if _, _, err := buildMultipart([]byte(`{}`), nil, crumbs, limits, warned, d); err != nil {
			t.Fatal(err)
		}
	}
	lines := logs.all()
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "Later skips are logged only with Debug.") ||
		lines[1] != `inline attachment "bt-breadcrumbs-0" skipped: attachment budget exhausted` {
		t.Errorf("lines = %q, want the one-time warning then a plain Debug line", lines)
	}
}

func TestSafeMultipartName(t *testing.T) {
	// Only separator-free basenames and portable paths: filepath.Base
	// treats `\` as a separator on Windows, so embedded-backslash
	// expectations would be platform-dependent.
	cases := []struct{ in, want string }{
		{"/var/log/app.log", "app.log"},
		{"evil\r\nname", "evil__name"},
		{"quote\"file", "quote_file"},
		{"/", "attachment"},
		{".", "attachment"},
		{"..", "attachment"},
	}
	for _, c := range cases {
		if got := safeMultipartName(c.in); got != c.want {
			t.Errorf("safeMultipartName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
