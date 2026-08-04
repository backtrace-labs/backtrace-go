package bt

import (
	"strings"
	"testing"
	"time"
)

func TestConfigNormalizeDefaults(t *testing.T) {
	cfg := Config{Endpoint: "http://example.com"}.normalize()

	if cfg.SourceCode != SourceCodeContext {
		t.Errorf("SourceCode = %q, want context", cfg.SourceCode)
	}
	if cfg.ContextLineCount != DefaultContextLineCount {
		t.Errorf("ContextLineCount = %d", cfg.ContextLineCount)
	}
	if cfg.TabWidth != DefaultTabWidth {
		t.Errorf("TabWidth = %d", cfg.TabWidth)
	}
	if cfg.SampleRate != 1.0 {
		t.Errorf("SampleRate = %v, want 1.0", cfg.SampleRate)
	}
	if cfg.QueueSize != DefaultQueueSize {
		t.Errorf("QueueSize = %d", cfg.QueueSize)
	}
	if cfg.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v", cfg.Timeout)
	}
	if cfg.MaxErrorDepth != DefaultMaxErrorDepth {
		t.Errorf("MaxErrorDepth = %d", cfg.MaxErrorDepth)
	}
	if cfg.MaxBreadcrumbs != DefaultMaxBreadcrumbs {
		t.Errorf("MaxBreadcrumbs = %d", cfg.MaxBreadcrumbs)
	}
}

func TestConfigNormalizePreservesExplicit(t *testing.T) {
	cfg := Config{
		Endpoint:         "http://example.com",
		ContextLineCount: 3,
		SampleRate:       0.5,
		QueueSize:        7,
		Timeout:          time.Second,
		MaxErrorDepth:    -1,
		MaxBreadcrumbs:   -1,
	}.normalize()

	if cfg.ContextLineCount != 3 || cfg.SampleRate != 0.5 || cfg.QueueSize != 7 ||
		cfg.Timeout != time.Second {
		t.Errorf("explicit values overridden: %+v", cfg)
	}
	if cfg.MaxErrorDepth != -1 {
		t.Errorf("MaxErrorDepth -1 (disabled) not preserved: %d", cfg.MaxErrorDepth)
	}
	if cfg.MaxBreadcrumbs != -1 {
		t.Errorf("MaxBreadcrumbs -1 (disabled) not preserved: %d", cfg.MaxBreadcrumbs)
	}
}

func TestConfigEnvFallback(t *testing.T) {
	t.Setenv(envEndpoint, "http://env.example.com")
	t.Setenv(envToken, "env-token")

	cfg := Config{}.normalize()
	if cfg.Endpoint != "http://env.example.com" || cfg.Token != "env-token" {
		t.Errorf("env fallback not applied: %+v", cfg)
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("validate after env fallback: %v", err)
	}
}

func TestSubmissionURLForms(t *testing.T) {
	// Neutralize any BACKTRACE_* variables from the outer environment.
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")

	// Full submit URL: used verbatim.
	cfg := Config{Endpoint: "https://submit.backtrace.io/universe/tok123/json"}.normalize()
	if got := cfg.submissionURL(); got != "https://submit.backtrace.io/universe/tok123/json" {
		t.Errorf("full URL form = %q", got)
	}

	// A stray token (e.g. BACKTRACE_TOKEN in the environment) must not
	// corrupt a complete submit.backtrace.io URL.
	cfg = Config{Endpoint: "https://submit.backtrace.io/universe/tok123/json", Token: "stray"}.normalize()
	if got := cfg.submissionURL(); got != "https://submit.backtrace.io/universe/tok123/json" {
		t.Errorf("submit URL corrupted by stray token: %q", got)
	}

	// Legacy endpoint + token: token is query-escaped.
	cfg = Config{Endpoint: "https://uni.sp.backtrace.io", Token: "a&b #c"}.normalize()
	got := cfg.submissionURL()
	if !strings.HasPrefix(got, "https://uni.sp.backtrace.io/post?") {
		t.Errorf("legacy form = %q", got)
	}
	if strings.Contains(got, "a&b") || !strings.Contains(got, "format=json") {
		t.Errorf("token not escaped or format missing: %q", got)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).validate(); err == nil {
		t.Error("empty endpoint accepted")
	}
	if err := (Config{Endpoint: "not a url\x7f"}).validate(); err == nil {
		t.Error("unparsable endpoint accepted")
	}
	if err := (Config{Endpoint: "ftp://x"}).validate(); err == nil {
		t.Error("non-http scheme accepted")
	}
	if err := (Config{Endpoint: "https://submit.backtrace.io/u/t/json"}).validate(); err != nil {
		t.Errorf("valid endpoint rejected: %v", err)
	}
}

func TestUnwrapErrorChainDepthCap(t *testing.T) {
	err := error(&testWrapErr{msg: "0"})
	for i := 1; i < 10; i++ {
		err = &testWrapErr{msg: string(rune('0' + i)), inner: err}
	}
	if got := len(unwrapErrorChain(err, 3)); got != 3 {
		t.Errorf("depth-capped chain = %d, want 3", got)
	}
	if got := unwrapErrorChain(err, -1); got != nil {
		t.Errorf("negative depth should disable capture, got %d", len(got))
	}
	if got := unwrapErrorChain(nil, 10); got != nil {
		t.Error("nil error should produce no chain")
	}
}

type testWrapErr struct {
	msg   string
	inner error
}

func (e *testWrapErr) Error() string { return e.msg }
func (e *testWrapErr) Unwrap() error { return e.inner }
