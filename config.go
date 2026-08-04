package bt

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// SourceCodeMode controls how much application source code is embedded in
// reports for the Backtrace debugger's source view.
type SourceCodeMode string

const (
	// SourceCodeContext embeds only ContextLineCount lines around each
	// stack frame. This is the default: it keeps payloads small and avoids
	// shipping whole files off the host.
	SourceCodeContext SourceCodeMode = "context"

	// SourceCodeFile embeds the entire source file referenced by each
	// frame (the SDK's historical behavior). Opt-in.
	SourceCodeFile SourceCodeMode = "file"

	// SourceCodeNone disables source code capture entirely.
	SourceCodeNone SourceCodeMode = "none"
)

// Defaults applied by NewClient when the corresponding Config field is zero.
const (
	// DefaultTimeout is the per-request HTTP timeout.
	DefaultTimeout = 30 * time.Second

	// DefaultQueueSize is the capacity of the in-memory report queue.
	// When the queue is full new reports are dropped (never blocking the
	// caller) and counted; see Client.DroppedReports.
	DefaultQueueSize = 128

	// DefaultContextLineCount is the number of source lines captured
	// above and below a stack frame line in SourceCodeContext mode.
	DefaultContextLineCount = 8

	// DefaultMaxErrorDepth caps how many wrapped errors are walked when
	// capturing an error chain.
	DefaultMaxErrorDepth = 100

	// DefaultMaxBreadcrumbs is the capacity of the breadcrumb ring buffer.
	DefaultMaxBreadcrumbs = 64

	// DefaultTabWidth is reported to the Backtrace UI for source rendering.
	DefaultTabWidth = 8

	// DefaultFlushTimeout is used by panic handlers and the legacy
	// FinishSendingReports to bound how long delivery is awaited.
	DefaultFlushTimeout = 5 * time.Second
)

// Environment variables consulted when the corresponding Config field is empty.
const (
	envEndpoint = "BACKTRACE_ENDPOINT"
	envToken    = "BACKTRACE_TOKEN"
)

// Config configures a Client. The zero value is not usable: Endpoint is
// required (directly or via the BACKTRACE_ENDPOINT environment variable).
type Config struct {
	// Endpoint is the Backtrace submission URL. Two forms are supported:
	//
	//   https://submit.backtrace.io/{universe}/{token}/json   (full URL, leave Token empty)
	//   https://{universe}.sp.backtrace.io                    (paired with Token)
	//
	// When Token is set, "/post?format=json&token=..." is appended.
	// Falls back to the BACKTRACE_ENDPOINT environment variable.
	Endpoint string

	// Token is the project submission token for legacy endpoints. Leave
	// empty when Endpoint is a full submit.backtrace.io URL.
	// Falls back to the BACKTRACE_TOKEN environment variable.
	Token string

	// CaptureAllGoroutines includes every goroutine's stack in reports,
	// not just the calling goroutine's.
	CaptureAllGoroutines bool

	// SourceCode controls source code embedding. Default: SourceCodeContext.
	SourceCode SourceCodeMode

	// ContextLineCount is the number of lines captured above and below a
	// frame's line in SourceCodeContext mode. Default: 8.
	ContextLineCount int

	// TabWidth is reported to the Backtrace UI for source rendering. Default: 8.
	TabWidth int

	// Attributes are added to every report sent by the client.
	Attributes map[string]interface{}

	// SendEnvVars attaches the process environment to every report as an
	// annotation. Values of variables whose names look secret-bearing
	// (TOKEN, SECRET, PASSWORD, KEY, ...) are redacted; see ScrubEnvVars.
	SendEnvVars bool

	// ScrubEnvVars adds case-insensitive substrings to the built-in list
	// of environment variable name patterns whose values are redacted.
	ScrubEnvVars []string

	// AttachmentPaths lists files attached to every report (multipart
	// submission, one "attachment_<basename>" part per file). Unreadable
	// files are skipped with a debug log. Per-report changes can be made
	// in BeforeSend via ReportData.Attachments.
	AttachmentPaths []string

	// SampleRate is the fraction of reports actually sent, in [0.0, 1.0].
	// The zero value means 1.0 (send everything), so an uninitialized
	// Config never silently drops reports.
	SampleRate float64

	// BeforeSend, when set, runs just before a report is serialized.
	// Return the (optionally modified) report to send it, or nil to drop
	// it. Runs on the SDK's worker goroutine; a panic inside the hook is
	// recovered and logged, and the report is sent unmodified.
	BeforeSend func(report *ReportData) *ReportData

	// MaxErrorDepth caps error-chain unwrapping. Default: 100. Negative
	// disables chain capture.
	MaxErrorDepth int

	// MaxBreadcrumbs caps the breadcrumb ring buffer. Default: 64.
	// Negative disables breadcrumbs.
	MaxBreadcrumbs int

	// QueueSize is the report queue capacity. Default: 128.
	QueueSize int

	// Timeout is the per-request HTTP timeout. Default: 30s.
	Timeout time.Duration

	// HTTPClient overrides the HTTP client used for submission. When set,
	// Timeout is not applied to it; configure the client yourself.
	HTTPClient *http.Client

	// DisableMachineAttributes skips the exec-based collection of machine
	// metadata (CPU model, OS version, machine GUID). Useful in minimal
	// containers without a shell.
	DisableMachineAttributes bool

	// Debug enables SDK diagnostic logging (report payloads, delivery
	// errors, drops). The SDK never panics regardless of this setting.
	Debug bool

	// Logger receives diagnostic output when Debug is on.
	// Default: log.New(os.Stderr, "[backtrace] ", log.LstdFlags).
	Logger Logger
}

// normalize applies defaults and environment fallbacks. It does not mutate c.
func (c Config) normalize() Config {
	if c.Endpoint == "" {
		c.Endpoint = os.Getenv(envEndpoint)
	}
	if c.Token == "" {
		c.Token = os.Getenv(envToken)
	}
	if c.SourceCode == "" {
		c.SourceCode = SourceCodeContext
	}
	if c.ContextLineCount <= 0 {
		c.ContextLineCount = DefaultContextLineCount
	}
	if c.TabWidth <= 0 {
		c.TabWidth = DefaultTabWidth
	}
	if c.SampleRate <= 0 {
		// Zero value means "send everything" so that a Config that never
		// mentions sampling behaves as expected.
		c.SampleRate = 1.0
	}
	if c.SampleRate > 1 {
		c.SampleRate = 1.0
	}
	if c.MaxErrorDepth == 0 {
		c.MaxErrorDepth = DefaultMaxErrorDepth
	}
	if c.MaxBreadcrumbs == 0 {
		c.MaxBreadcrumbs = DefaultMaxBreadcrumbs
	}
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultQueueSize
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	return c
}

// validate checks that the endpoint is usable. Called with a normalized config.
func (c Config) validate() error {
	if c.Endpoint == "" {
		return errors.New("bt: Config.Endpoint is required (or set BACKTRACE_ENDPOINT)")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("bt: invalid Config.Endpoint: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("bt: Config.Endpoint must be an http(s) URL, got %q", c.Endpoint)
	}
	return nil
}

// submissionURL builds the URL reports are POSTed to.
//
// With a Token, the legacy form {endpoint}/post?format=json&token={token} is
// used; without one the endpoint is assumed to be a complete submission URL
// (e.g. https://submit.backtrace.io/{universe}/{token}/json). A
// submit.backtrace.io endpoint already embeds its token in the path and is
// always used verbatim, so a stray Token (e.g. BACKTRACE_TOKEN in the
// environment) cannot corrupt it.
func (c Config) submissionURL() string {
	if c.Token == "" {
		return c.Endpoint
	}
	if u, err := url.Parse(c.Endpoint); err == nil &&
		strings.EqualFold(u.Hostname(), "submit.backtrace.io") {
		return c.Endpoint
	}
	v := url.Values{}
	v.Set("format", "json")
	v.Set("token", c.Token)
	return fmt.Sprintf("%s/post?%s", c.Endpoint, v.Encode())
}
