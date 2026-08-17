package bt

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestConfigNormalizeDefaults(t *testing.T) {
	cfg := Config{Endpoint: "http://example.com"}.normalize()

	if cfg.SourceCode != SourceCodeMetadata {
		t.Errorf("SourceCode = %q, want metadata (production-safe default)", cfg.SourceCode)
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

	// Trailing slash (the form users paste from a browser) must not
	// produce "//post".
	cfg = Config{Endpoint: "https://uni.sp.backtrace.io/", Token: "tok"}.normalize()
	if got := cfg.submissionURL(); strings.Contains(got, "//post") {
		t.Errorf("trailing slash produced double slash: %q", got)
	}
}

func TestConfigValidateRejectsQueryWithToken(t *testing.T) {
	err := (Config{Endpoint: "https://host:6098?x=1", Token: "tok"}).validate()
	if err == nil {
		t.Error("endpoint with query + token accepted; submissionURL would be malformed")
	}
	// Without a token the endpoint is used verbatim, so a query is fine.
	if err := (Config{Endpoint: "https://host/post?format=json&token=t"}).normalize().validate(); err != nil {
		t.Errorf("verbatim endpoint with query rejected: %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	valid := func(mutate func(*Config)) Config {
		c := Config{Endpoint: "https://submit.backtrace.io/u/t/json"}.normalize()
		if mutate != nil {
			mutate(&c)
		}
		return c
	}

	if err := (Config{}).validate(); err == nil {
		t.Error("empty endpoint accepted")
	}
	if err := valid(func(c *Config) { c.Endpoint = "not a url\x7f" }).validate(); err == nil {
		t.Error("unparsable endpoint accepted")
	}
	if err := valid(func(c *Config) { c.Endpoint = "ftp://x" }).validate(); err == nil {
		t.Error("non-http scheme accepted")
	}
	if err := valid(nil).validate(); err != nil {
		t.Errorf("valid endpoint rejected: %v", err)
	}

	// Strict validation of explicit values (no silent repair).
	if err := valid(func(c *Config) { c.SampleRate = -0.5 }).validate(); err == nil {
		t.Error("negative SampleRate accepted")
	}
	if err := valid(func(c *Config) { c.SampleRate = 1.5 }).validate(); err == nil {
		t.Error("SampleRate > 1 accepted")
	}
	if err := valid(func(c *Config) { c.SampleRate = math.NaN() }).validate(); err == nil {
		t.Error("NaN SampleRate accepted")
	}
	if err := valid(func(c *Config) { c.QueueSize = -1 }).validate(); err == nil {
		t.Error("negative QueueSize accepted")
	}
	if err := valid(func(c *Config) { c.SourceCode = "everything" }).validate(); err == nil {
		t.Error("unknown SourceCode mode accepted")
	}
	if err := valid(func(c *Config) { c.Endpoint = "http://" }).validate(); err == nil {
		t.Error("host-less endpoint accepted")
	}
	if err := valid(func(c *Config) { c.Endpoint = "https://user:pw@host/x" }).validate(); err == nil {
		t.Error("endpoint with userinfo accepted")
	}
	if err := valid(func(c *Config) { c.Endpoint = "https://host/x#frag" }).validate(); err == nil {
		t.Error("endpoint with fragment accepted")
	}
	if err := valid(func(c *Config) { c.Timeout = -time.Second }).validate(); err == nil {
		t.Error("negative Timeout accepted")
	}
}

// TestValidateNeverLeaksToken pins the credential-redaction contract: even
// unparsable endpoints (where url.Error — or its inner error — would quote
// raw URL fragments) must not leak an embedded token through NewClient
// errors.
func TestValidateNeverLeaksToken(t *testing.T) {
	cases := []string{
		"https://uni.sp.backtrace.io/post?token=SUPERSECRETTOKEN\x7f",
		"https://uni.sp.backtrace.io/post?token=SUPERSECRETTOKEN\x00",
		"ht tp://x/post?token=SUPERSECRETTOKEN",
		"https://host:SUPERSECRETTOKEN/post", // token-like text in port position
	}
	for _, endpoint := range cases {
		_, err := NewClient(Config{Endpoint: endpoint})
		if err == nil {
			t.Errorf("endpoint %q accepted", endpoint)
			continue
		}
		if strings.Contains(err.Error(), "SUPERSECRETTOKEN") {
			t.Errorf("token leaked through NewClient error: %v", err)
		}
	}
}

func TestUnwrapErrorChainDepthCap(t *testing.T) {
	err := error(&testWrapErr{msg: "0"})
	for i := 1; i < 10; i++ {
		err = &testWrapErr{msg: string(rune('0' + i)), inner: err}
	}
	// Depth 3 admits the root plus three unwrap levels.
	if got := len(unwrapErrorChain(err, 3, DefaultMaxErrorNodes)); got != 4 {
		t.Errorf("depth-capped chain = %d, want 4", got)
	}
	if got := len(unwrapErrorChain(err, DefaultMaxErrorDepth, 2)); got != 2 {
		t.Errorf("node-capped chain = %d, want 2", got)
	}
	if got := unwrapErrorChain(err, -1, DefaultMaxErrorNodes); got != nil {
		t.Errorf("negative depth should disable capture, got %d", len(got))
	}
	if got := unwrapErrorChain(nil, 10, 10); got != nil {
		t.Error("nil error should produce no chain")
	}
}

func TestUnwrapErrorChainJoinAndCycles(t *testing.T) {
	// errors.Join fan-out is captured with parent/source metadata.
	a := errors.New("a")
	b := errors.New("b")
	joined := errors.Join(a, b)
	wrapped := fmt.Errorf("outer: %w", joined)

	chain := unwrapErrorChain(wrapped, DefaultMaxErrorDepth, DefaultMaxErrorNodes)
	if len(chain) != 4 {
		t.Fatalf("join graph nodes = %d, want 4 (outer, join, a, b)", len(chain))
	}
	if chain[2].Source != "errors[0]" || chain[3].Source != "errors[1]" {
		t.Errorf("join sources = %q, %q", chain[2].Source, chain[3].Source)
	}

	// A cyclic error graph terminates without exhausting the caps.
	cyclic := &testWrapErr{msg: "cycle"}
	cyclic.inner = cyclic
	got := unwrapErrorChain(cyclic, DefaultMaxErrorDepth, DefaultMaxErrorNodes)
	if len(got) != 1 {
		t.Errorf("cyclic graph nodes = %d, want 1", len(got))
	}
}

type testWrapErr struct {
	msg   string
	inner error
}

func (e *testWrapErr) Error() string { return e.msg }
func (e *testWrapErr) Unwrap() error { return e.inner }
