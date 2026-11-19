package bt

import (
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
	if tr.rateLimited() {
		t.Error("fresh transport is rate limited")
	}
	tr.pause(time.Minute)
	if !tr.rateLimited() {
		t.Error("pause not applied")
	}
	reason, err := tr.send(context.Background(), "http://127.0.0.1:1/unused",
		[]byte("{}"), nil, nil, multipartLimits{}, diag{})
	if err != errRateLimited || reason != dropRateLimit {
		t.Errorf("send while paused = (%v, %v), want (dropRateLimit, errRateLimited)", reason, err)
	}
}

func TestRedactURL(t *testing.T) {
	in := "https://example.com/post?format=json&token=supersecret"
	out := redactURL(in)
	if out == in {
		t.Error("token not redacted")
	}
	if want := "token=REDACTED"; !strings.Contains(out, want) {
		t.Errorf("redacted URL %q missing %q", out, want)
	}
	// submit.backtrace.io embeds the token as the second path segment.
	if got := redactURL("https://submit.backtrace.io/universe/secret-token/json"); got != "https://submit.backtrace.io/universe/REDACTED/json" {
		t.Errorf("submit path token not redacted: %q", got)
	}
	// Two-segment form (no trailing /json) still carries the token.
	if got := redactURL("https://submit.backtrace.io/universe/secret-token"); got != "https://submit.backtrace.io/universe/REDACTED" {
		t.Errorf("2-segment submit path token not redacted: %q", got)
	}
	// Self-hosted/aliased gateways with the submit path shape: format suffix.
	if got := redactURL("https://errors.mycorp.com/universe/sometoken/json"); got != "https://errors.mycorp.com/universe/REDACTED/json" {
		t.Errorf("aliased submit path (format suffix) not redacted: %q", got)
	}
	// ... or a hex-shaped token.
	hexTok := "51cc8e69c5b62fa8c72dc963e730f1e8"
	if got := redactURL("https://errors.mycorp.com/universe/" + hexTok); got != "https://errors.mycorp.com/universe/REDACTED" {
		t.Errorf("aliased submit path (hex token) not redacted: %q", got)
	}
	// Plain API paths never match the token shape.
	if got := redactURL("https://uni.sp.backtrace.io/api/post"); got != "https://uni.sp.backtrace.io/api/post" {
		t.Errorf("tokenless URL modified: %q", got)
	}
	// URL userinfo is redacted.
	if got := redactURL("https://user:secretpw@host/x"); strings.Contains(got, "secretpw") {
		t.Errorf("userinfo not redacted: %q", got)
	}
	// Unparsable input degrades to a fixed placeholder, never the raw string.
	if got := redactURL("http://%zz\x7f"); got != "[REDACTED URL]" {
		t.Errorf("unparsable URL leaked: %q", got)
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
	_, err = tr.send(context.Background(), serverA.URL, []byte("{}"), nil, nil, multipartLimits{}, diag{})
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
	if _, err := owned.send(context.Background(), srv.URL, []byte("{}"), nil, nil, multipartLimits{}, diag{}); err != nil {
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
	if _, err := tr.send(context.Background(), srv.URL, []byte("{}"), nil, nil, multipartLimits{}, diag{}); err != nil {
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

// multipartParts builds a submission and returns its attachment parts in
// the order they appear, as name -> content.
func multipartParts(t *testing.T, files []string, inline []inlinePart, limits multipartLimits) (names []string, content map[string]string) {
	t.Helper()
	body, contentType, err := buildMultipart([]byte(`{}`), files, inline, limits, diag{})
	if err != nil {
		t.Fatalf("buildMultipart: %v", err)
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("content type %q: %v", contentType, err)
	}
	content = map[string]string{}
	reader := multipart.NewReader(body, params["boundary"])
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
