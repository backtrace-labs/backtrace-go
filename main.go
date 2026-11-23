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
// Options.Attributes. SetDefaultClient makes these functions use a Client
// of your own.
//
// # Request scopes
//
// Client attributes and breadcrumbs apply to every report. For data that
// belongs to one request, attach a Scope to the request's context and report
// with that context:
//
//	ctx = bt.WithScope(ctx)
//	bt.ScopeFromContext(ctx).SetAttribute("user.id", id)
//	client.ReportContext(ctx, err, nil)
//
// # What is reported
//
// This SDK reports errors, messages, and recovered panics from within the
// process. It cannot capture crashes that bypass Go panics (native/cgo
// faults, runtime aborts); for those, use the bcd out-of-process tracer
// integration in this package or the Backtrace Coresnap workflow.
package bt

import (
	"context"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
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
	// ReportsPerMinute caps reports per minute; see Config.ReportsPerMinute.
	// Applied when the default client is created (first report).
	ReportsPerMinute int
	// BeforeSend runs before each report is serialized; see Config.BeforeSend.
	BeforeSend func(report *ReportData) *ReportData
	// ScrubEnvVars extends the built-in redaction patterns; see Config.ScrubEnvVars.
	ScrubEnvVars []string
	// EnvVars is an exact allowlist of env var names to capture; see Config.EnvVars.
	EnvVars []string
	// SendMachineID includes a stable machine identifier; see
	// Config.SendMachineID. Default false.
	SendMachineID bool
	// Logger receives diagnostics when DebugBacktrace is on.
	// It also receives the one-time warnings described on Config.Logger.
	Logger Logger
	// HTTPClient overrides the submission HTTP client.
	HTTPClient *http.Client
	// DisableCompression sends request bodies uncompressed; see
	// Config.DisableCompression.
	DisableCompression bool
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
		ReportsPerMinute:         Options.ReportsPerMinute,
		BeforeSend:               Options.BeforeSend,
		ScrubEnvVars:             cloneStringSlice(Options.ScrubEnvVars),
		EnvVars:                  cloneStringSlice(Options.EnvVars),
		SendMachineID:            Options.SendMachineID,
		Logger:                   Options.Logger,
		HTTPClient:               Options.HTTPClient,
		DisableCompression:       Options.DisableCompression,
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
	explicitClient  *Client

	// disabledWarnOnce rate-limits the unconditional misconfiguration
	// warning to a single line per process.
	disabledWarnOnce sync.Once
	disabledDiagBusy atomic.Int32
)

// SetDefaultClient makes the package-level functions report through c
// instead of the client configured by Options. Options is not read while c
// is set. SetDefaultClient(nil) returns to the Options-based client. The
// caller still owns c and closes it.
func SetDefaultClient(c *Client) {
	defaultClientMu.Lock()
	explicitClient = c
	defaultClientMu.Unlock()
}

// DefaultClient returns the client the package-level functions report
// through: the one passed to SetDefaultClient, otherwise a client created
// from Options on first use. It returns nil when that client cannot be
// created, for example while no endpoint is configured or an Options field
// is invalid. The package-level functions are then no-ops.
func DefaultClient() *Client {
	defaultClientMu.Lock()
	if c := explicitClient; c != nil {
		defaultClientMu.Unlock()
		return c
	}
	if defaultClientV != nil {
		c := defaultClientV
		defaultClientMu.Unlock()
		if legacyEndpoint() == "" {
			return nil
		}
		return c
	}
	cfg := optionsToConfig()
	if err := cfg.validate(); err != nil {
		defaultClientMu.Unlock()
		logReportingDisabled(cfg, err)
		return nil
	}
	// The legacy client re-reads Options on every report so historical
	// patterns (mutating bt.Options at runtime) keep working; queue size,
	// timeout, and HTTP client are fixed at creation, and each report
	// snapshots the delivery configuration at capture time.
	c := startClient(optionsToConfig, false)
	defaultClientV = c
	defaultClientMu.Unlock()
	return c
}

// Call without defaultClientMu held. The Logger is application code.
func logReportingDisabled(cfg Config, err error) {
	diag{logger: cfg.Logger, debug: cfg.Debug, busy: &disabledDiagBusy}.logf("reporting disabled: %v", err)
	// An endpoint was configured but rejected: the application
	// clearly intended reporting, so surface the misconfiguration
	// once even without debug mode (an empty endpoint stays
	// silent — that is the documented no-op mode).
	if cfg.Endpoint != "" {
		disabledWarnOnce.Do(func() {
			defaultDiagLogger.Printf("reporting disabled: %v", err)
		})
	}
}

// Report sends an error report through the default client (see
// DefaultClient). object may be an error or any value convertible to a
// string; nil is ignored. extraAttributes are added to this report only.
// Safe no-op while the SDK is unconfigured. Never blocks on network I/O.
func Report(object interface{}, extraAttributes map[string]interface{}) {
	if c := DefaultClient(); c != nil {
		c.Report(object, extraAttributes)
	}
}

// reportingConfigured decides whether the deferred panic helpers should act
// WITHOUT instantiating the default client — the helpers run on every
// deferred return, and client startup belongs on the (rare) panic path.
func reportingConfigured() bool {
	if explicitDefaultClient() != nil {
		return true
	}
	if currentDefaultClient() != nil {
		return legacyEndpoint() != ""
	}
	return optionsToConfig().validate() == nil
}

func legacyEndpoint() string {
	if Options.Endpoint != "" {
		return Options.Endpoint
	}
	return os.Getenv(envEndpoint)
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
	if c := DefaultClient(); c != nil {
		if _, delivered := c.reportPanicAndWait(context.Background(), v, extraAttributes); delivered {
			markDeliveredPanic(v)
		}
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
	if c := DefaultClient(); c != nil {
		c.ReportPanicValue(v, extraAttributes)
		return
	}
	// Configuration became invalid between the check and client creation
	// (rare race): keep the panic alive rather than silently swallow it.
	panic(v)
}

// ReportPanicValue reports an already-recovered panic value through the
// default client without re-panicking or blocking. Intended for
// middleware and custom recover() handlers.
func ReportPanicValue(value interface{}, extraAttributes map[string]interface{}) {
	if c := DefaultClient(); c != nil {
		c.ReportPanicValue(value, extraAttributes)
	}
}

// ReportPanicValueAndFlush reports an already-recovered panic value and
// waits for its delivery under one deadline, as
// Client.ReportPanicValueAndFlush does. Returns false when the SDK is
// unconfigured or delivery did not complete in time.
func ReportPanicValueAndFlush(ctx context.Context, value any, extraAttributes map[string]any) bool {
	if c := DefaultClient(); c != nil {
		return c.ReportPanicValueAndFlush(ctx, value, extraAttributes)
	}
	return false
}

// SetAttribute sets a global attribute included in every subsequent report.
// Safe for concurrent use; prefer this over mutating Options.Attributes.
// A nil value removes the attribute from reports, including SDK defaults
// such as "hostname". While a client is set with SetDefaultClient, the
// attribute is set on that client.
//
// The value is normalised and copied before it is stored, so changing a map
// or slice after the call returns does not change later reports.
func SetAttribute(key string, value interface{}) {
	if c := explicitDefaultClient(); c != nil {
		c.SetAttribute(key, value)
		return
	}
	// Normalise before taking the lock: it may run the value's own
	// MarshalJSON / MarshalText / Error methods, which are application code.
	frozen := freezeValue(value, legacyValueLimits())
	legacyAttrMu.Lock()
	if Options.Attributes == nil {
		Options.Attributes = map[string]interface{}{}
	}
	Options.Attributes[key] = frozen
	legacyAttrMu.Unlock()
}

// SetAttributes sets multiple global attributes atomically.
// Values are normalised and copied before they are stored. While a client is
// set with SetDefaultClient, they are set on that client.
func SetAttributes(attrs map[string]interface{}) {
	if c := explicitDefaultClient(); c != nil {
		c.SetAttributes(attrs)
		return
	}
	frozen := freezeAttributes(attrs, legacyValueLimits())
	legacyAttrMu.Lock()
	if Options.Attributes == nil {
		Options.Attributes = map[string]interface{}{}
	}
	for k, v := range frozen {
		Options.Attributes[k] = v
	}
	legacyAttrMu.Unlock()
}

// legacyValueLimits is the normalisation budget for values stored through
// the package-level SetAttribute(s). It deliberately does not read
// bt.Options: those fields are not synchronised. Scopes use it too.
func legacyValueLimits() valueLimits {
	return valueLimits{maxTotalBytes: DefaultMaxReportBytes}
}

// AddBreadcrumb records a breadcrumb on the default client (see
// DefaultClient).
func AddBreadcrumb(b Breadcrumb) {
	if c := DefaultClient(); c != nil {
		c.AddBreadcrumb(b)
	}
}

// Flush blocks until reports queued at the time of the call are processed
// or timeout elapses, reporting whether the drain completed. The reporter
// stays fully usable afterwards. Flushing proves local processing and send
// completion, not backend acceptance. While a client is set with
// SetDefaultClient, Flush also waits for the Options-based client, under the
// same timeout.
func Flush(timeout time.Duration) bool {
	var clients []*Client
	for _, c := range []*Client{explicitDefaultClient(), currentDefaultClient()} {
		if c != nil {
			clients = append(clients, c)
		}
	}
	targets := make([][laneCount]uint64, len(clients))
	for i, c := range clients {
		targets[i] = c.acceptedTargets()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	drained := true
	for i, c := range clients {
		drained = c.flushTargetContext(ctx, targets[i]) && drained
	}
	return drained
}

// FinishSendingReports blocks until queued reports are sent (bounded by the
// configured submission deadline per report, 30s overall). Unlike
// historical versions it does NOT stop the reporter: reporting continues to
// work afterwards. Kept for backward compatibility — new code should use
// Flush.
func FinishSendingReports() {
	Flush(DefaultTimeout)
}

// currentDefaultClient returns the Options-based client without creating one.
func currentDefaultClient() *Client {
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	return defaultClientV
}

func explicitDefaultClient() *Client {
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	return explicitClient
}
