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
//
// # Scope
//
// This SDK reports errors, messages, and recovered panics from within the
// process. It cannot capture crashes that bypass Go panics (native/cgo
// faults, runtime aborts); for those, use the bcd out-of-process tracer
// integration in this package or the Backtrace Coresnap workflow.
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

	// SendEnvVars attaches the process environment to every report.
	// Default false. See Config.SendEnvVars for the redaction rules.
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
	// DebugBacktrace enables SDK diagnostic logging (payload-free
	// summaries). Unlike historical versions, the SDK never panics:
	// failures are logged instead.
	DebugBacktrace bool

	// SourceCode controls source embedding; see Config.SourceCode.
	// Default: SourceCodeMetadata (path/line only). Historical behavior
	// (whole files) is available with SourceCodeFile.
	SourceCode SourceCodeMode
	// SourceRoots restricts source text reads; see Config.SourceRoots.
	SourceRoots []string
	// SampleRate is the fraction of reports sent; zero means 1.0.
	SampleRate float64
	// BeforeSend runs before each report is serialized; see Config.BeforeSend.
	BeforeSend func(report *ReportData) *ReportData
	// ScrubEnvVars extends the built-in redaction patterns; see Config.ScrubEnvVars.
	ScrubEnvVars []string
	// SendMachineID includes a stable machine identifier; see
	// Config.SendMachineID. Default false.
	SendMachineID bool
	// Logger receives diagnostics when DebugBacktrace is on.
	Logger Logger
	// HTTPClient overrides the submission HTTP client.
	HTTPClient *http.Client
	// Timeout is the per-request submission deadline (default 30s).
	// Applied when the default client is created (first report).
	Timeout time.Duration
	// ShutdownTimeout bounds client shutdown; see Config.ShutdownTimeout.
	ShutdownTimeout time.Duration
	// QueueSize is the report queue capacity (default 128). Applied when
	// the default client is created (first report).
	QueueSize int
	// MaxErrorDepth caps error-graph unwrapping; see Config.MaxErrorDepth.
	MaxErrorDepth int
	// MaxBreadcrumbs caps the breadcrumb buffer; applied at first report.
	MaxBreadcrumbs int
	// AttachmentPaths lists files attached to every report.
	AttachmentPaths []string
	// DisableMachineAttributes skips machine metadata collection.
	DisableMachineAttributes bool
}

// Options configures the legacy global reporter. Set fields before the
// first report; concurrent mutation while reporting is not synchronized
// (use SetAttribute / SetAttributes for runtime attribute updates). The
// delivery configuration is snapshotted when each report is captured, so
// later changes cannot reroute already-queued reports.
var Options OptionsStruct

func init() {
	// Historical behavior: Options.Attributes is usable at import time,
	// so existing `bt.Options.Attributes[k] = v` call sites keep working.
	Options.Attributes = map[string]interface{}{}
}

// legacyAttrMu guards Options.Attributes for callers using SetAttribute
// alongside the legacy global API.
var legacyAttrMu sync.Mutex

// optionsToConfig snapshots the global Options into a Config, cloning every
// collection so queued reports cannot observe later mutations.
func optionsToConfig() Config {
	legacyAttrMu.Lock()
	attrs := cloneAnyMap(Options.Attributes)
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
		SourceRoots:              cloneStringSlice(Options.SourceRoots),
		SampleRate:               Options.SampleRate,
		BeforeSend:               Options.BeforeSend,
		ScrubEnvVars:             cloneStringSlice(Options.ScrubEnvVars),
		SendMachineID:            Options.SendMachineID,
		Logger:                   Options.Logger,
		HTTPClient:               Options.HTTPClient,
		Timeout:                  Options.Timeout,
		ShutdownTimeout:          Options.ShutdownTimeout,
		QueueSize:                Options.QueueSize,
		MaxErrorDepth:            Options.MaxErrorDepth,
		MaxBreadcrumbs:           Options.MaxBreadcrumbs,
		AttachmentPaths:          cloneStringSlice(Options.AttachmentPaths),
		DisableMachineAttributes: Options.DisableMachineAttributes,
	}.normalize()
}

var (
	defaultClientMu sync.Mutex
	defaultClientV  *Client

	// disabledWarnOnce rate-limits the unconditional misconfiguration
	// warning to a single line per process.
	disabledWarnOnce sync.Once
)

// defaultClient lazily creates the client backing the legacy global API.
// Returns nil while Options.Endpoint (and BACKTRACE_ENDPOINT) are unset or
// invalid: the global API is then a safe no-op.
func defaultClient() *Client {
	defaultClientMu.Lock()
	if defaultClientV != nil {
		c := defaultClientV
		defaultClientMu.Unlock()
		return c
	}
	cfg := optionsToConfig()
	if err := cfg.validate(); err != nil {
		defaultClientMu.Unlock()
		// Log outside the lock: the logger is application code.
		diag{logger: cfg.Logger, debug: cfg.Debug}.logf("reporting disabled: %v", err)
		// An endpoint was configured but rejected: the application
		// clearly intended reporting, so surface the misconfiguration
		// once even without debug mode (an empty endpoint stays
		// silent — that is the documented no-op mode).
		if cfg.Endpoint != "" {
			disabledWarnOnce.Do(func() {
				defaultDiagLogger.Printf("reporting disabled: %v", err)
			})
		}
		return nil
	}
	// The legacy client re-reads Options on every report so historical
	// patterns (mutating bt.Options at runtime) keep working; queue size,
	// timeout, and HTTP client are fixed at creation, and each report
	// snapshots the delivery configuration at capture time.
	c := startClient(optionsToConfig)
	defaultClientV = c
	defaultClientMu.Unlock()
	return c
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

// reportingConfigured decides whether the deferred panic helpers should act
// WITHOUT instantiating the default client — the helpers run on every
// deferred return, and client startup belongs on the (rare) panic path.
func reportingConfigured() bool {
	if currentDefaultClient() != nil {
		return true
	}
	return optionsToConfig().validate() == nil
}

// ReportPanic reports a panic and re-panics with the original value, after
// waiting up to DefaultFlushTimeout (one deadline covering capture and
// delivery). Use with defer:
//
//	defer bt.ReportPanic(nil)
//
// While the SDK is unconfigured this function does NOT recover: the
// original panic proceeds exactly as if the handler were absent.
func ReportPanic(extraAttributes map[string]interface{}) {
	if !reportingConfigured() {
		// Crucial: do not call recover. The original panic continues
		// exactly as it did before SDK configuration.
		return
	}
	v := recover()
	if v == nil {
		return
	}
	if c := defaultClient(); c != nil {
		_ = c.ReportPanicValueAndFlush(v, extraAttributes, DefaultFlushTimeout)
	}
	panic(v)
}

// ReportAndRecoverPanic reports a panic and swallows it; the goroutine
// lives on. Use with defer. While the SDK is unconfigured this function
// does NOT recover: the original panic proceeds unchanged.
func ReportAndRecoverPanic(extraAttributes map[string]interface{}) {
	if !reportingConfigured() {
		return // no recover; preserve the application's panic
	}
	v := recover()
	if v == nil {
		return
	}
	if c := defaultClient(); c != nil {
		c.ReportPanicValue(v, extraAttributes)
		return
	}
	// Configuration became invalid between the check and client creation
	// (rare race): keep the panic alive rather than silently swallow it.
	panic(v)
}

// ReportPanicValue reports an already-recovered panic value through the
// legacy global reporter without re-panicking or blocking. Intended for
// middleware and custom recover() handlers.
func ReportPanicValue(value interface{}, extraAttributes map[string]interface{}) {
	if c := defaultClient(); c != nil {
		c.ReportPanicValue(value, extraAttributes)
	}
}

// ReportPanicValueAndFlush reports an already-recovered panic value and
// waits for its delivery under one deadline. Returns false when the SDK is
// unconfigured or delivery did not complete in time.
func ReportPanicValueAndFlush(value interface{}, extraAttributes map[string]interface{}, timeout time.Duration) bool {
	if c := defaultClient(); c != nil {
		return c.ReportPanicValueAndFlush(value, extraAttributes, timeout)
	}
	return false
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
// stays fully usable afterwards. Flushing proves local processing and send
// completion, not backend acceptance.
func Flush(timeout time.Duration) bool {
	c := currentDefaultClient()
	if c == nil {
		return true
	}
	return c.Flush(timeout)
}

// FinishSendingReports blocks until queued reports are sent (bounded by the
// configured submission deadline per report, 30s overall). Unlike
// historical versions it does NOT stop the reporter: reporting continues to
// work afterwards. Kept for backward compatibility — new code should use
// Flush.
func FinishSendingReports() {
	Flush(DefaultTimeout)
}

// currentDefaultClient returns the default client without creating one.
func currentDefaultClient() *Client {
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	return defaultClientV
}
