package bt

import (
	"errors"
	"fmt"
	"math"
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
	// SourceCodeMetadata reports file path and line metadata for each
	// frame but embeds no source text. This is the production-safe
	// default: no application source ever leaves the host.
	SourceCodeMetadata SourceCodeMode = "metadata"

	// SourceCodeContext embeds ContextLineCount lines around each stack
	// frame. Opt-in; consider SourceRoots to constrain which files may
	// be read.
	SourceCodeContext SourceCodeMode = "context"

	// SourceCodeFile embeds the entire source file referenced by each
	// frame (the SDK's historical behavior). Opt-in.
	SourceCodeFile SourceCodeMode = "file"

	// SourceCodeNone disables source references entirely.
	SourceCodeNone SourceCodeMode = "none"
)

// Defaults applied by NewClient when the corresponding Config field is zero.
const (
	// DefaultTimeout is the per-request submission deadline. It is
	// enforced with an SDK-owned context even when a custom HTTPClient
	// is supplied.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxRetries is the number of extra submission attempts after
	// a transient failure.
	DefaultMaxRetries = 2

	// DefaultShutdownTimeout bounds Close.
	DefaultShutdownTimeout = 5 * time.Second

	// DefaultQueueSize is the capacity of the in-memory report queue.
	// When the queue is full new reports are dropped (never blocking the
	// caller) and counted; see Client.Stats.
	DefaultQueueSize = 128

	// DefaultContextLineCount is the number of source lines captured
	// above and below a stack frame line in SourceCodeContext mode.
	DefaultContextLineCount = 8

	// DefaultMaxErrorDepth caps how deep wrapped-error graphs are walked.
	DefaultMaxErrorDepth = 32

	// DefaultMaxErrorNodes caps how many errors one report's error graph
	// may contain (errors.Join fan-out included).
	DefaultMaxErrorNodes = 100

	// DefaultMaxBreadcrumbs is the capacity of the breadcrumb ring buffer.
	DefaultMaxBreadcrumbs = 64

	// DefaultTabWidth is reported to the Backtrace UI for source rendering.
	DefaultTabWidth = 8

	// DefaultFlushTimeout bounds how long ReportPanic waits for capture plus
	// delivery before re-panicking. ReportPanicValueAndFlush uses it when
	// its context has no deadline. ReportAndRecoverPanic does not wait for
	// delivery.
	DefaultFlushTimeout = 5 * time.Second

	// DefaultMaxStackBytes caps the raw goroutine dump size.
	DefaultMaxStackBytes = 4 << 20

	// DefaultMaxReportBytes caps the serialized JSON report.
	DefaultMaxReportBytes = 8 << 20

	// DefaultMaxAttachments caps the number of attachments per report.
	DefaultMaxAttachments = 16

	// DefaultMaxEnvVars caps environment variables captured under SendEnvVars.
	DefaultMaxEnvVars = 1024

	// DefaultMaxEnvBytes caps the total bytes of captured environment
	// variable names and values.
	DefaultMaxEnvBytes = 256 << 10

	// Upper ceilings on explicitly-set resource limits. These exist so an
	// extreme configuration value cannot exhaust memory or crash NewClient
	// (e.g. make([]Breadcrumb, huge)) before a single report is sent. They
	// are generous relative to realistic backend ingestion limits.
	maxQueueSize          = 1 << 20
	maxBreadcrumbsCeiling = 100_000
	maxErrorNodesCeiling  = 100_000
	maxErrorDepthCeiling  = 4096
	maxAttachmentsCeiling = 4096
	maxStackBytesCeiling  = 256 << 20
	maxReportBytesCeiling = 256 << 20
	maxSourceBytesCeiling = 256 << 20
)

// Byte budgets for source and attachment capture.
const (
	// DefaultMaxSourceFileBytes caps a single source file read.
	DefaultMaxSourceFileBytes int64 = 2 << 20

	// DefaultMaxSourceBytes caps the total source text embedded in one
	// report.
	DefaultMaxSourceBytes int64 = 4 << 20

	// DefaultMaxAttachmentBytes caps one attachment.
	DefaultMaxAttachmentBytes int64 = 10 << 20

	// DefaultMaxTotalAttachmentBytes caps the aggregate attachment bytes
	// of one report.
	DefaultMaxTotalAttachmentBytes int64 = 25 << 20
)

// Environment variables consulted when the corresponding Config field is empty.
const (
	envEndpoint = "BACKTRACE_ENDPOINT"
	envToken    = "BACKTRACE_TOKEN"
)

// Config configures a Client. The zero value is not usable: Endpoint is
// required (directly or via the BACKTRACE_ENDPOINT environment variable).
//
// NewClient clones every top-level map and slice, so the caller may reuse or
// mutate the Config afterwards.
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

	// SourceCode controls source embedding. Default: SourceCodeMetadata
	// (path/line only — no source text leaves the host). Embedding
	// source text is opt-in via SourceCodeContext or SourceCodeFile.
	SourceCode SourceCodeMode

	// SourceRoots, when non-empty, restricts source text reads (context
	// and file modes) to files under the listed directory roots.
	SourceRoots []string

	// ContextLineCount is the number of lines captured above and below a
	// frame's line in SourceCodeContext mode. Default: 8.
	ContextLineCount int

	// TabWidth is reported to the Backtrace UI for source rendering. Default: 8.
	TabWidth int

	// Attributes are added to every report sent by the client. Like every
	// value handed to the SDK they are normalised into a detached copy
	// (see ReportData for the value model), so the map may be reused
	// after NewClient returns.
	// Values are converted for the backend as described on
	// ReportData.Attributes.
	Attributes map[string]interface{}

	// EnvVars is an exact allowlist of environment variable names to
	// attach to every report (as the "Environment Variables" annotation).
	// This is the recommended, predictable way to capture environment:
	// only the listed names are read. Name/shape scrubbing (see
	// SendEnvVars) still applies as defense in depth.
	// Crash reports from MonitorCrashes carry no environment.
	EnvVars []string

	// SendEnvVars attaches the ENTIRE process environment to every report,
	// except crash reports from MonitorCrashes.
	// Best-effort only: values are redacted when the variable name
	// contains any of TOKEN, SECRET, PASS, KEY, CREDENTIAL, AUTH, DSN,
	// COOKIE, SESSION, SIGNATURE, BEARER, CONN (case-insensitive) or when
	// the value embeds URL credentials — but this cannot catch every
	// secret (generically named vars, JWTs).
	// The client's own Token and the token in its Endpoint, when at least
	// 8 bytes long, become "REDACTED" in every value. In URL values, query
	// values with secret-looking names, such as password, token, signature
	// or api_key, become "REDACTED".
	// Prefer EnvVars, or scrub with BeforeSend. Ignored when EnvVars is
	// set.
	SendEnvVars bool

	// ScrubEnvVars adds case-insensitive substrings to the built-in list
	// of environment variable name patterns whose values are redacted.
	ScrubEnvVars []string

	// MaxEnvVars caps how many environment variables are captured under
	// SendEnvVars. Default: 1024. Ignored for the EnvVars allowlist.
	MaxEnvVars int

	// MaxEnvBytes caps the total bytes of captured environment variable
	// names and values. Default: 256 KiB.
	MaxEnvBytes int

	// SendMachineID includes a stable machine identifier (the "guid"
	// attribute) with every report. Default false: stable hardware
	// identifiers are privacy-sensitive and opt-in.
	SendMachineID bool

	// AttachmentPaths lists files attached to every report (multipart
	// submission, one "attachment_<basename>" part per file). Unreadable
	// or non-regular files and files over the per-file/aggregate budgets
	// are skipped with a debug log. Per-report changes can be made in
	// BeforeSend via ReportData.Attachments.
	AttachmentPaths []string

	// SampleRate is the fraction of reports actually sent, in [0.0, 1.0].
	// The zero value means 1.0 (send everything), so an uninitialized
	// Config never silently drops reports. Explicit values outside
	// [0, 1], NaN, and infinities are rejected by NewClient.
	// Panic reports are never sampled out.
	SampleRate float64

	// ReportsPerMinute caps the reports the client accepts in any 60-second
	// window. 0 means no limit. Negative values are rejected by NewClient.
	// Panic reports count too. Dropped reports are counted in
	// ClientStats.ClientRateLimited.
	ReportsPerMinute int

	// BeforeSend, when set, runs just before a report is serialized.
	// Return the (optionally modified) report to send it, or nil to drop
	// it. Runs on the SDK's worker goroutine — do not call Flush or Close
	// from inside the hook. A panic inside the hook is recovered and the
	// report is DROPPED (never sent half-scrubbed) and counted in Stats.
	// ReportData.Error holds the reported error, for errors.Is and errors.As.
	//
	// The hook edits its own copy of the report, attribute values have
	// the types listed under ReportData ("Value model"), and what it
	// removes stays removed; see ReportData for the full contract.
	BeforeSend func(report *ReportData) *ReportData

	// MaxErrorDepth caps error-graph unwrapping depth. Default: 32.
	// Negative disables chain capture and the error origin thread.
	MaxErrorDepth int

	// MaxErrorNodes caps the total number of errors captured from one
	// error graph (relevant for errors.Join trees). Default: 100.
	MaxErrorNodes int

	// MaxBreadcrumbs caps the breadcrumb ring buffer. Default: 64.
	// Negative disables breadcrumbs.
	// It also caps the client and Scope breadcrumbs merged into one report.
	MaxBreadcrumbs int

	// QueueSize is the report queue capacity. Default: 128.
	// Panic reports that wait for delivery have a second queue of this size.
	QueueSize int

	// Timeout is the per-request submission deadline, enforced with an
	// SDK-owned request context (it applies to custom HTTPClients too).
	// Default: 30s.
	Timeout time.Duration

	// MaxRetries is the number of extra attempts for a report after a
	// network error before the request body was sent, or after a 408, 500,
	// 502, 503 or 504 response. Default: 2. Negative disables retries. The
	// waits between attempts add up to at most 60s per report.
	//
	// A 429 response, or 5 network failures in a row, pauses submissions.
	// Reports are dropped during the pause.
	MaxRetries int

	// DisableCompression sends request bodies uncompressed. By default they
	// are gzip-compressed.
	DisableCompression bool

	// ShutdownTimeout bounds Close. Default: 5s.
	ShutdownTimeout time.Duration

	// HTTPClient overrides the HTTP client used for submission. Requests
	// still carry the SDK's per-request context deadline (Timeout).
	// Cancellation is cooperative: a custom Transport/RoundTripper that
	// ignores the request context cannot be forcefully terminated by the
	// SDK and can hold the worker (and Close) past its deadline.
	//
	// When nil, the SDK's own client uses 10s dial and TLS handshake
	// timeouts and a 20s response header timeout.
	HTTPClient *http.Client

	// Resource budgets; zero values use the documented defaults.
	MaxStackBytes           int   // raw goroutine dump cap (default 4 MiB)
	MaxSourceFileBytes      int64 // single source file read cap (default 2 MiB)
	MaxSourceBytes          int64 // total embedded source per report (default 4 MiB)
	MaxReportBytes          int   // serialized JSON report cap (default 8 MiB); larger reports are trimmed first
	MaxAttachments          int   // attachments per report, breadcrumbs included and counted last (default 16); negative disables attachments
	MaxAttachmentBytes      int64 // one attachment (default 10 MiB)
	MaxTotalAttachmentBytes int64 // aggregate attachments per report (default 25 MiB)

	// DisableMachineAttributes skips collection of machine metadata
	// (CPU model, OS version).
	DisableMachineAttributes bool

	// Debug enables SDK diagnostic logging: compact, payload-free
	// summaries (event ID, type, byte counts, redacted destination,
	// outcome). Report payloads are never logged. The SDK never panics
	// regardless of this setting.
	Debug bool

	// Logger receives diagnostic output when Debug is on.
	// Without Debug it still receives one-time warnings, for example when
	// the server rejects reports with a status that retrying cannot fix.
	// Default: log.New(os.Stderr, "[backtrace] ", log.LstdFlags).
	// A panicking Logger is contained and cannot crash the application.
	// A Logger that calls back into the SDK (a logging-adapter pattern)
	// is also contained: while a diagnostic line is being written, nested
	// diagnostics for the same client are suppressed to break recursion.
	//
	// When the Logger writes to a btslog handler on the calling goroutine,
	// SDK diagnostics go to the wrapped handler only. They never become
	// reports or breadcrumbs. This does not hold for a Logger that passes
	// its lines to another goroutine first. With Debug on, such a Logger
	// reports its own diagnostics in a loop.
	Logger Logger
}

// cloneConfig detaches all top-level collections from caller ownership.
// Config.Attributes is deep-frozen (not just shallow-cloned) so nested
// caller-owned maps/slices cannot later race the worker's serialization.
func cloneConfig(c Config) Config {
	c.Attributes = freezeAttributes(c.Attributes, c.valueLimits())
	c.ScrubEnvVars = cloneStringSlice(c.ScrubEnvVars)
	c.AttachmentPaths = cloneStringSlice(c.AttachmentPaths)
	c.SourceRoots = cloneStringSlice(c.SourceRoots)
	c.EnvVars = cloneStringSlice(c.EnvVars)
	return c
}

// valueLimits derives the freezing budget from the report byte cap.
func (c Config) valueLimits() valueLimits {
	return valueLimits{maxTotalBytes: c.MaxReportBytes}
}

// normalize applies defaults to zero values only. It does not mutate c.
func (c Config) normalize() Config {
	if c.Endpoint == "" {
		c.Endpoint = os.Getenv(envEndpoint)
	}
	if c.Token == "" {
		c.Token = os.Getenv(envToken)
	}
	if c.SourceCode == "" {
		c.SourceCode = SourceCodeMetadata
	}
	if c.ContextLineCount == 0 {
		c.ContextLineCount = DefaultContextLineCount
	}
	if c.TabWidth == 0 {
		c.TabWidth = DefaultTabWidth
	}
	if c.SampleRate == 0 {
		// Zero value means "send everything" so that a Config that never
		// mentions sampling behaves as expected.
		c.SampleRate = 1.0
	}
	if c.MaxErrorDepth == 0 {
		c.MaxErrorDepth = DefaultMaxErrorDepth
	}
	if c.MaxErrorNodes == 0 {
		c.MaxErrorNodes = DefaultMaxErrorNodes
	}
	if c.MaxBreadcrumbs == 0 {
		c.MaxBreadcrumbs = DefaultMaxBreadcrumbs
	}
	if c.QueueSize == 0 {
		c.QueueSize = DefaultQueueSize
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.MaxStackBytes == 0 {
		c.MaxStackBytes = DefaultMaxStackBytes
	}
	if c.MaxSourceFileBytes == 0 {
		c.MaxSourceFileBytes = DefaultMaxSourceFileBytes
	}
	if c.MaxSourceBytes == 0 {
		c.MaxSourceBytes = DefaultMaxSourceBytes
	}
	if c.MaxReportBytes == 0 {
		c.MaxReportBytes = DefaultMaxReportBytes
	}
	if c.MaxAttachments == 0 {
		c.MaxAttachments = DefaultMaxAttachments
	}
	if c.MaxAttachmentBytes == 0 {
		c.MaxAttachmentBytes = DefaultMaxAttachmentBytes
	}
	if c.MaxTotalAttachmentBytes == 0 {
		c.MaxTotalAttachmentBytes = DefaultMaxTotalAttachmentBytes
	}
	if c.MaxEnvVars == 0 {
		c.MaxEnvVars = DefaultMaxEnvVars
	}
	if c.MaxEnvBytes == 0 {
		c.MaxEnvBytes = DefaultMaxEnvBytes
	}
	return c
}

// validate rejects invalid explicit values rather than silently repairing
// them. Called with a normalized config.
func (c Config) validate() error {
	if c.Endpoint == "" {
		return errors.New("bt: Config.Endpoint is required (or set BACKTRACE_ENDPOINT)")
	}
	if math.IsNaN(c.SampleRate) || math.IsInf(c.SampleRate, 0) ||
		c.SampleRate < 0 || c.SampleRate > 1 {
		return fmt.Errorf("bt: Config.SampleRate must be finite and in [0,1], got %v", c.SampleRate)
	}
	if c.ReportsPerMinute < 0 {
		return fmt.Errorf("bt: Config.ReportsPerMinute must be >= 0, got %d", c.ReportsPerMinute)
	}
	if c.QueueSize < 1 || c.QueueSize > maxQueueSize {
		return fmt.Errorf("bt: Config.QueueSize must be in [1,%d], got %d", maxQueueSize, c.QueueSize)
	}
	if c.Timeout <= 0 || c.ShutdownTimeout <= 0 {
		return errors.New("bt: Config.Timeout and Config.ShutdownTimeout must be positive")
	}
	if c.ContextLineCount < 0 || c.TabWidth < 1 {
		return errors.New("bt: Config.ContextLineCount must be >= 0 and Config.TabWidth >= 1")
	}
	// MaxErrorDepth is deliberately absent: a negative value is the
	// documented way to disable error-chain capture.
	if c.MaxErrorNodes < 1 || c.MaxStackBytes < 1 || c.MaxReportBytes < 1 ||
		c.MaxSourceFileBytes < 1 || c.MaxSourceBytes < 1 ||
		c.MaxAttachmentBytes < 1 || c.MaxTotalAttachmentBytes < 1 ||
		c.MaxEnvVars < 1 || c.MaxEnvBytes < 1 {
		return errors.New("bt: one or more resource limits are invalid (must be positive)")
	}
	// Reject extreme values that could exhaust memory or crash NewClient
	// before any report is sent.
	switch {
	case c.MaxBreadcrumbs > maxBreadcrumbsCeiling:
		return fmt.Errorf("bt: Config.MaxBreadcrumbs exceeds %d", maxBreadcrumbsCeiling)
	case c.MaxErrorNodes > maxErrorNodesCeiling:
		return fmt.Errorf("bt: Config.MaxErrorNodes exceeds %d", maxErrorNodesCeiling)
	case c.MaxErrorDepth > maxErrorDepthCeiling:
		return fmt.Errorf("bt: Config.MaxErrorDepth exceeds %d", maxErrorDepthCeiling)
	case c.MaxAttachments > maxAttachmentsCeiling:
		return fmt.Errorf("bt: Config.MaxAttachments exceeds %d", maxAttachmentsCeiling)
	case c.MaxStackBytes > maxStackBytesCeiling:
		return fmt.Errorf("bt: Config.MaxStackBytes exceeds %d", maxStackBytesCeiling)
	case c.MaxReportBytes > maxReportBytesCeiling:
		return fmt.Errorf("bt: Config.MaxReportBytes exceeds %d", maxReportBytesCeiling)
	case c.MaxSourceBytes > maxSourceBytesCeiling || c.MaxSourceFileBytes > maxSourceBytesCeiling:
		return fmt.Errorf("bt: source byte limit exceeds %d", maxSourceBytesCeiling)
	}
	if c.MaxAttachmentBytes > c.MaxTotalAttachmentBytes {
		return errors.New("bt: Config.MaxAttachmentBytes exceeds MaxTotalAttachmentBytes")
	}
	switch c.SourceCode {
	case SourceCodeMetadata, SourceCodeContext, SourceCodeFile, SourceCodeNone:
	default:
		return fmt.Errorf("bt: unknown Config.SourceCode mode %q", c.SourceCode)
	}

	u, err := url.Parse(c.Endpoint)
	if err != nil {
		// Do not wrap err: *url.Error quotes the complete raw URL,
		// which may embed a token, and NewClient errors flow into
		// user logs. Surface only the inner reason, and only when it
		// carries no quoted input fragment (net/url embeds offending
		// substrings, e.g. invalid ports, inside double quotes).
		msg := "unparsable URL"
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			if inner := ue.Err.Error(); !strings.Contains(inner, `"`) {
				msg = inner
			}
		}
		return fmt.Errorf("bt: invalid Config.Endpoint (%s)", msg)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("bt: Config.Endpoint must be an http(s) URL")
	}
	if u.Host == "" || u.Opaque != "" {
		return errors.New("bt: Config.Endpoint must be an absolute URL with a host")
	}
	if u.User != nil {
		return errors.New("bt: Config.Endpoint must not contain URL userinfo")
	}
	if strings.Contains(c.Endpoint, "#") {
		return errors.New("bt: Config.Endpoint must not contain a fragment")
	}
	if c.Token != "" && (u.RawQuery != "" || u.ForceQuery) &&
		!strings.EqualFold(u.Hostname(), "submit.backtrace.io") {
		return errors.New("bt: Config.Endpoint must not carry a query string when Token is set")
	}

	// Structural validation of the documented hosted forms, so a common
	// misconfiguration fails at NewClient instead of silently POSTing to
	// the wrong path. Self-hosted/custom gateways (any other host) are
	// unconstrained.
	host := strings.ToLower(u.Hostname())
	trimmedPath := strings.Trim(u.Path, "/")
	switch {
	case host == "submit.backtrace.io":
		// Complete submission URL: {universe}/{token}/{format}.
		if segs := strings.Split(trimmedPath, "/"); len(segs) < 3 || segs[0] == "" || segs[1] == "" || segs[2] == "" {
			return fmt.Errorf("bt: submit.backtrace.io Config.Endpoint must be of the form " +
				"https://submit.backtrace.io/{universe}/{token}/{format}")
		}
	case strings.HasSuffix(host, ".sp.backtrace.io"):
		// The {universe}.sp.backtrace.io form pairs the host with a
		// Token; without one (and without an explicit submission path)
		// the client would POST to the host root.
		if c.Token == "" && trimmedPath == "" {
			return errors.New("bt: a *.sp.backtrace.io Config.Endpoint requires Config.Token " +
				"(or set BACKTRACE_TOKEN)")
		}
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
	// Trim trailing slashes (the form users paste from a browser) so the
	// appended path never produces "//post".
	return fmt.Sprintf("%s/post?%s", strings.TrimRight(c.Endpoint, "/"), v.Encode())
}

// multipartLimits carries the attachment budgets into the transport.
type multipartLimits struct {
	maxAttachments     int
	maxAttachmentBytes int64
	maxTotalBytes      int64
}

func (c Config) attachmentLimits() multipartLimits {
	return multipartLimits{
		maxAttachments:     c.MaxAttachments,
		maxAttachmentBytes: c.MaxAttachmentBytes,
		maxTotalBytes:      c.MaxTotalAttachmentBytes,
	}
}
