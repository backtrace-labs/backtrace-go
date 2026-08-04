// Package bt is the Backtrace error reporting SDK for Go, plus an
// integration with out-of-process tracers (see bcd.go).
//
// # Modern API
//
// Create a Client with NewClient and report errors, messages, and recovered
// panics through it:
//
//	client, err := bt.NewClient(bt.Config{
//		Endpoint: "https://submit.backtrace.io/{universe}/{token}/json",
//	})
//	if err != nil { ... }
//	defer client.Close()
//	client.Report(err, nil)
//
// # Legacy global API
//
// The package-level functions (Report, ReportPanic, ReportAndRecoverPanic,
// FinishSendingReports) operate on a default client configured through the
// global Options variable and remain fully supported:
//
//	bt.Options.Endpoint = "https://submit.backtrace.io/{universe}/{token}/json"
//	bt.Report(err, nil)
//
// Configure Options before the first report. For runtime attribute changes
// use SetAttribute (safe for concurrent use) instead of mutating
// Options.Attributes.
package bt

import (
	"net/http"
	"sync"
	"time"
)

// OptionsStruct configures the legacy global reporter. New code should
// prefer NewClient with a Config.
type OptionsStruct struct {
	// Endpoint is the Backtrace submission URL; see Config.Endpoint.
	Endpoint string
	// Token is the project token for legacy endpoints; see Config.Token.
	Token string

	// SendEnvVars attaches the process environment (secret-looking values
	// redacted) to every report. Default false.
	SendEnvVars bool

	// CaptureAllGoroutines includes every goroutine's stack in reports.
	CaptureAllGoroutines bool
	// TabWidth is reported to the Backtrace UI for source rendering.
	TabWidth int
	// ContextLineCount limits the source lines captured around each frame.
	ContextLineCount int
	// Attributes are added to every report. Prefer SetAttribute for
	// changes made while the application is running.
	Attributes map[string]interface{}
	// DebugBacktrace enables SDK diagnostic logging. Unlike historical
	// versions, the SDK never panics: failures are logged instead.
	DebugBacktrace bool

	// SourceCode controls source embedding; see Config.SourceCode.
	// Default: SourceCodeContext (historical behavior was whole files;
	// opt back in with SourceCodeFile).
	SourceCode SourceCodeMode
	// SampleRate is the fraction of reports sent; zero means 1.0.
	SampleRate float64
	// BeforeSend runs before each report is serialized; see Config.BeforeSend.
	BeforeSend func(report *ReportData) *ReportData
	// ScrubEnvVars extends the built-in redaction patterns; see Config.ScrubEnvVars.
	ScrubEnvVars []string
	// Logger receives diagnostics when DebugBacktrace is on.
	Logger Logger
	// HTTPClient overrides the submission HTTP client.
	HTTPClient *http.Client
	// Timeout is the per-request HTTP timeout (default 30s). Applied when
	// the default client is created (first report).
	Timeout time.Duration
	// QueueSize is the report queue capacity (default 128). Applied when
	// the default client is created (first report).
	QueueSize int
	// MaxErrorDepth caps error-chain unwrapping; see Config.MaxErrorDepth.
	MaxErrorDepth int
	// MaxBreadcrumbs caps the breadcrumb buffer; applied at first report.
	MaxBreadcrumbs int
	// AttachmentPaths lists files attached to every report.
	AttachmentPaths []string
	// DisableMachineAttributes skips exec-based machine metadata collection.
	DisableMachineAttributes bool
}

// Options configures the legacy global reporter. Set fields before the
// first report; concurrent mutation while reporting is not synchronized
// (use SetAttribute / SetAttributes for runtime attribute updates).
var Options OptionsStruct

func init() {
	// Historical behavior: Options.Attributes is usable at import time,
	// so existing `bt.Options.Attributes[k] = v` call sites keep working.
	Options.Attributes = map[string]interface{}{}
}

// legacyAttrMu guards Options.Attributes for callers using SetAttribute
// alongside the legacy global API.
var legacyAttrMu sync.Mutex

// optionsToConfig snapshots the global Options into a Config.
func optionsToConfig() Config {
	legacyAttrMu.Lock()
	attrs := make(map[string]interface{}, len(Options.Attributes))
	for k, v := range Options.Attributes {
		attrs[k] = v
	}
	legacyAttrMu.Unlock()

	return Config{
		Endpoint:                 Options.Endpoint,
		Token:                    Options.Token,
		SendEnvVars:              Options.SendEnvVars,
		CaptureAllGoroutines:     Options.CaptureAllGoroutines,
		TabWidth:                 Options.TabWidth,
		ContextLineCount:         Options.ContextLineCount,
		Attributes:               attrs,
		Debug:                    Options.DebugBacktrace,
		SourceCode:               Options.SourceCode,
		SampleRate:               Options.SampleRate,
		BeforeSend:               Options.BeforeSend,
		ScrubEnvVars:             Options.ScrubEnvVars,
		Logger:                   Options.Logger,
		HTTPClient:               Options.HTTPClient,
		Timeout:                  Options.Timeout,
		QueueSize:                Options.QueueSize,
		MaxErrorDepth:            Options.MaxErrorDepth,
		MaxBreadcrumbs:           Options.MaxBreadcrumbs,
		AttachmentPaths:          Options.AttachmentPaths,
		DisableMachineAttributes: Options.DisableMachineAttributes,
	}.normalize()
}

var (
	defaultClientMu sync.Mutex
	defaultClientV  *Client
)

// defaultClient lazily creates the client backing the legacy global API.
// Returns nil while Options.Endpoint (and BACKTRACE_ENDPOINT) are unset:
// the global API is then a safe no-op.
func defaultClient() *Client {
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	if defaultClientV != nil {
		return defaultClientV
	}
	cfg := optionsToConfig()
	if err := cfg.validate(); err != nil {
		diag{logger: Options.Logger, debug: Options.DebugBacktrace}.logf("reporting disabled: %v", err)
		return nil
	}
	// The legacy client re-reads Options on every report so historical
	// patterns (mutating bt.Options at runtime) keep working; queue size,
	// timeout, and HTTP client are fixed at creation.
	defaultClientV = startClient(optionsToConfig)
	return defaultClientV
}

// Report sends an error report through the legacy global reporter. object
// may be an error or any value convertible to a string; nil is ignored.
// extraAttributes are added to this report only. Safe no-op while the SDK
// is unconfigured. Never blocks on network I/O.
func Report(object interface{}, extraAttributes map[string]interface{}) {
	if c := defaultClient(); c != nil {
		c.Report(object, extraAttributes)
	}
}

// ReportPanic reports a panic and re-panics with the original value, after
// waiting up to DefaultFlushTimeout for delivery. Use with defer:
//
//	defer bt.ReportPanic(nil)
func ReportPanic(extraAttributes map[string]interface{}) {
	v := recover()
	if v == nil {
		return
	}
	if c := defaultClient(); c != nil {
		c.ReportPanicValue(v, extraAttributes)
		c.Flush(DefaultFlushTimeout)
	}
	panic(v)
}

// ReportAndRecoverPanic reports a panic and swallows it; the goroutine
// lives on. Use with defer.
func ReportAndRecoverPanic(extraAttributes map[string]interface{}) {
	v := recover()
	if v == nil {
		return
	}
	if c := defaultClient(); c != nil {
		c.ReportPanicValue(v, extraAttributes)
	}
}

// ReportPanicValue reports an already-recovered panic value through the
// legacy global reporter without re-panicking. Intended for middleware and
// custom recover() handlers.
func ReportPanicValue(value interface{}, extraAttributes map[string]interface{}) {
	if c := defaultClient(); c != nil {
		c.ReportPanicValue(value, extraAttributes)
	}
}

// SetAttribute sets a global attribute included in every subsequent report.
// Safe for concurrent use; prefer this over mutating Options.Attributes.
func SetAttribute(key string, value interface{}) {
	legacyAttrMu.Lock()
	if Options.Attributes == nil {
		Options.Attributes = map[string]interface{}{}
	}
	Options.Attributes[key] = value
	legacyAttrMu.Unlock()
}

// SetAttributes sets multiple global attributes atomically.
func SetAttributes(attrs map[string]interface{}) {
	legacyAttrMu.Lock()
	if Options.Attributes == nil {
		Options.Attributes = map[string]interface{}{}
	}
	for k, v := range attrs {
		Options.Attributes[k] = v
	}
	legacyAttrMu.Unlock()
}

// AddBreadcrumb records a breadcrumb on the legacy global reporter.
func AddBreadcrumb(b Breadcrumb) {
	if c := defaultClient(); c != nil {
		c.AddBreadcrumb(b)
	}
}

// Flush blocks until reports queued at the time of the call are processed
// or timeout elapses, reporting whether the drain completed. The reporter
// stays fully usable afterwards.
func Flush(timeout time.Duration) bool {
	c := currentDefaultClient()
	if c == nil {
		return true
	}
	return c.Flush(timeout)
}

// FinishSendingReports blocks until queued reports are sent (bounded by the
// configured HTTP timeout per report, 30s overall). Unlike historical
// versions it does NOT stop the reporter: reporting continues to work
// afterwards. Kept for backward compatibility — new code should use Flush.
func FinishSendingReports() {
	Flush(DefaultTimeout)
}

// currentDefaultClient returns the default client without creating one.
func currentDefaultClient() *Client {
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	return defaultClientV
}
