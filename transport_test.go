package bt

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
