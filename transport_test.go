package bt

import (
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
	if err := tr.send("http://127.0.0.1:1/unused", []byte("{}"), nil, nil, diag{}); err != errRateLimited {
		t.Errorf("send while paused = %v, want errRateLimited", err)
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
	// Other tokenless URLs pass through unchanged.
	if got := redactURL("https://uni.sp.backtrace.io/api/post"); got != "https://uni.sp.backtrace.io/api/post" {
		t.Errorf("tokenless URL modified: %q", got)
	}
}
