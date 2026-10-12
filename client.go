package bt

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Client is an instance-based Backtrace reporter. Multiple independent
// clients may coexist in one process. All methods are safe for concurrent
// use and never panic — including on a nil *Client (e.g. when a NewClient
// error was ignored), where every method is a no-op.
//
// Regular Report* calls never block on network I/O or queue admission:
// reports are queued to a background worker, and a full queue drops the
// newest report and counts it (see Stats). Bounded synchronous waiting
// exists only where explicitly documented: Flush/FlushContext,
// Close/CloseContext, and ReportPanicValueAndFlush.
type Client struct {
	// cfgFn returns the normalized configuration. For clients created by
	// NewClient it returns a fixed config; the legacy global client
	// re-reads bt.Options so that historical mutate-the-global usage
	// keeps working. Each report snapshots the config at capture time so
	// later global changes cannot reroute queued reports.
	cfgFn func() Config

	// internalDiag is fixed at construction and used on recovery paths,
	// where re-reading configuration could itself fail.
	internalDiag diag

	transport *httpTransport

	// ctx is the SDK root context; cancel aborts in-flight requests on
	// CloseContext deadline expiry.
	ctx    context.Context
	cancel context.CancelFunc

	queue     chan clientJob
	enqueueMu sync.Mutex // guards closed, accepted, and queue admission
	closed    bool
	accepted  uint64
	processed atomic.Uint64

	// progress is a broadcast channel: replaced (and the old one closed)
	// each time the worker finishes a job or the client closes, waking
	// every waiter.
	pmu      sync.Mutex
	progress chan struct{}

	closeOnce  sync.Once
	workerDone chan struct{}

	shutdownTimeout time.Duration

	amu        sync.Mutex // guards attributes
	attributes map[string]interface{}

	crumbs *breadcrumbRing

	stats internalStats

	// logBusy backs the diag re-entrancy guard for this client.
	logBusy atomic.Int32
}

// clientJob is a queued report with its admission sequence number.
type clientJob struct {
	seq  uint64
	data *queuedReport
}

// queuedReport carries everything captured on the caller's goroutine,
// including the delivery/scrubbing configuration in effect at capture time;
// parsing and I/O happen on the worker.
type queuedReport struct {
	cfg         Config
	stack       []byte
	attributes  map[string]interface{}
	annotations map[string]interface{}
	classifiers []string
	reportType  string // SDK-known type for diagnostics (never user data)
	timestamp   int64
}

// nonBlocking is a pre-canceled context: queue admission under it makes one
// attempt and drops immediately, never waiting.
var nonBlocking = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// NewClient creates and starts a reporter with the given configuration.
// It returns an error when the endpoint is missing or unparsable, or when
// an explicitly set option is invalid. The Config's top-level maps and
// slices are cloned; the caller may reuse them afterwards.
func NewClient(cfg Config) (*Client, error) {
	n := cloneConfig(cfg).normalize()
	if err := n.validate(); err != nil {
		return nil, err
	}
	return startClient(func() Config { return n }), nil
}

// startClient wires up a client around a config source and starts its worker.
func startClient(cfgFn func() Config) *Client {
	cfg := cfgFn()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		cfgFn:           cfgFn,
		transport:       newHTTPTransport(cfg.HTTPClient, cfg.Timeout),
		ctx:             ctx,
		cancel:          cancel,
		queue:           make(chan clientJob, cfg.QueueSize),
		progress:        make(chan struct{}),
		workerDone:      make(chan struct{}),
		shutdownTimeout: cfg.ShutdownTimeout,
		attributes:      map[string]interface{}{},
		crumbs:          newBreadcrumbRing(cfg.MaxBreadcrumbs),
	}
	// The re-entrancy guard needs the client's address, so wire the
	// diagnostics after construction.
	c.internalDiag = diag{logger: cfg.Logger, debug: cfg.Debug, busy: &c.logBusy}
	go c.worker()
	return c
}

// Report sends an error report. object may be an error (its message, type
// and unwrap graph are captured) or any value convertible to a string
// (reported as a message). A nil object is ignored. extraAttributes are
// added to this report only; the map is copied, never retained or mutated.
func (c *Client) Report(object interface{}, extraAttributes map[string]interface{}) {
	if c == nil {
		return
	}
	switch v := object.(type) {
	case nil:
		return
	case error:
		c.ReportError(v, extraAttributes)
	default:
		c.ReportMessage(safeSprint(v), extraAttributes)
	}
}

// ReportError sends a report for err, capturing its type and unwrap graph.
func (c *Client) ReportError(err error, extraAttributes map[string]interface{}) {
	if c == nil || err == nil {
		return
	}
	c.capture(nonBlocking, captureInput{
		message:    safeErrorString(err),
		err:        err,
		classifier: "error",
		reportType: "error",
		extra:      extraAttributes,
	})
}

// ReportMessage sends a plain message report.
func (c *Client) ReportMessage(msg string, extraAttributes map[string]interface{}) {
	if c == nil {
		return
	}
	c.capture(nonBlocking, captureInput{
		message:    msg,
		classifier: "message",
		reportType: "message",
		extra:      extraAttributes,
	})
}

// ReportPanicValue sends a report for a recovered panic value without
// blocking: a full queue drops the report (counted in Stats). Use
// ReportPanicValueAndFlush when the goroutine or process is about to die
// and delivery must be awaited.
func (c *Client) ReportPanicValue(value interface{}, extraAttributes map[string]interface{}) {
	if c == nil || value == nil {
		return
	}
	c.capture(nonBlocking, panicCaptureInput(value, extraAttributes))
}

// ReportPanicValueAndFlush captures a recovered panic value and waits for
// its delivery under ONE deadline covering queue admission and flushing.
// It reports whether the report was accepted and processed in time.
func (c *Client) ReportPanicValueAndFlush(value interface{}, extraAttributes map[string]interface{}, timeout time.Duration) bool {
	if c == nil || value == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	seq, accepted := c.capture(ctx, panicCaptureInput(value, extraAttributes))
	return accepted && c.flushTargetContext(ctx, seq)
}

func panicCaptureInput(value interface{}, extra map[string]interface{}) captureInput {
	in := captureInput{
		message:    safeSprint(value),
		classifier: "panic",
		reportType: "panic",
		extra:      extra,
	}
	if err, ok := value.(error); ok {
		in.err = err
		in.message = safeErrorString(err)
	}
	return in
}

// SetAttribute sets a client-wide attribute included in every subsequent
// report. Safe for concurrent use.
func (c *Client) SetAttribute(key string, value interface{}) {
	if c == nil {
		return
	}
	c.amu.Lock()
	defer c.amu.Unlock()
	c.attributes[key] = value
}

// SetAttributes sets multiple client-wide attributes atomically. The map is
// copied.
func (c *Client) SetAttributes(attrs map[string]interface{}) {
	if c == nil {
		return
	}
	c.amu.Lock()
	defer c.amu.Unlock()
	for k, v := range attrs {
		c.attributes[k] = v
	}
}

// AddBreadcrumb records a breadcrumb attached to every subsequent report.
// The breadcrumb's attribute map is copied. Safe for concurrent use.
func (c *Client) AddBreadcrumb(b Breadcrumb) {
	if c == nil {
		return
	}
	c.crumbs.add(b)
}

// Stats returns an immutable snapshot of the client's report accounting.
func (c *Client) Stats() ClientStats {
	if c == nil {
		return ClientStats{}
	}
	return c.stats.snapshot()
}

// DroppedReports returns the total number of reports discarded for any
// reason (queue full, closed, sampling, BeforeSend, serialization, size
// budgets, rate limiting, network or server failure, internal error).
// See Stats for the per-reason breakdown.
func (c *Client) DroppedReports() uint64 {
	if c == nil {
		return 0
	}
	return c.stats.droppedTotal()
}

// FlushContext blocks until every report accepted BEFORE the call has been
// processed, or until ctx is done. Reports enqueued after the call do not
// extend the wait (strict capture-time barrier). It returns true when the
// pre-call backlog was processed. Flushing proves local processing and
// send completion, not backend acceptance.
func (c *Client) FlushContext(ctx context.Context) bool {
	if c == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.enqueueMu.Lock()
	target := c.accepted
	c.enqueueMu.Unlock()
	return c.flushTargetContext(ctx, target)
}

// Flush is FlushContext with a timeout. The client stays fully usable
// afterwards.
func (c *Client) Flush(timeout time.Duration) bool {
	if c == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.FlushContext(ctx)
}

// flushTargetContext waits until the worker has processed the report with
// sequence number target. The broadcast channel is grabbed before each
// re-check so an advance signaled in between cannot be lost.
func (c *Client) flushTargetContext(ctx context.Context, target uint64) bool {
	for {
		wait := c.progressCh()
		if c.processed.Load() >= target {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-wait:
		case <-c.workerDone:
			return c.processed.Load() >= target
		}
	}
}

// progressCh returns the current broadcast generation channel.
func (c *Client) progressCh() <-chan struct{} {
	c.pmu.Lock()
	ch := c.progress
	c.pmu.Unlock()
	return ch
}

// signalProgress wakes every waiter (flushers and blocked panic enqueuers).
func (c *Client) signalProgress() {
	c.pmu.Lock()
	close(c.progress)
	c.progress = make(chan struct{})
	c.pmu.Unlock()
}

// CloseContext drains the queue, stops the worker, and releases the client.
// If ctx expires first, in-flight and queued submissions are cancelled and
// CloseContext returns false. Subsequent reports are dropped (and counted).
// Safe to call multiple times.
//
// Close and Flush must not be called from inside a BeforeSend hook: the
// hook runs on the worker goroutine those calls wait on.
func (c *Client) CloseContext(ctx context.Context) bool {
	if c == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.beginClose()

	select {
	case <-c.workerDone:
		c.cancel()
		c.transport.closeIdleConnections()
		return true
	case <-ctx.Done():
		// Abort the current request and cause queued requests to fail
		// quickly; the worker still exits on its own.
		c.cancel()
		c.transport.closeIdleConnections()
		return false
	}
}

// Close drains the queue and stops the worker, bounded by the configured
// ShutdownTimeout (default 5s). Call Flush first if you need a distinct
// delivery guarantee. Close is idempotent.
func (c *Client) Close() {
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.shutdownTimeout)
	defer cancel()
	_ = c.CloseContext(ctx)
}

func (c *Client) beginClose() {
	c.closeOnce.Do(func() {
		c.enqueueMu.Lock()
		c.closed = true
		close(c.queue)
		c.enqueueMu.Unlock()
		c.signalProgress()
	})
}

// captureInput bundles the per-call capture parameters.
type captureInput struct {
	message    string
	err        error
	classifier string
	reportType string
	extra      map[string]interface{}
}

// capture assembles everything that must be observed on the caller's
// goroutine (stack, attribute snapshot, delivery config) and enqueues the
// report. With a nil ctx it never blocks; with a ctx it retries queue
// admission until the context is done. It never panics.
func (c *Client) capture(ctx context.Context, in captureInput) (seq uint64, accepted bool) {
	defer func() {
		if r := recover(); r != nil {
			// Type-only: the panic value may carry user data.
			c.stats.drop(dropInternal)
			c.internalDiag.logf("internal error while capturing report (please report to backtrace-labs/backtrace-go): %T", r)
			seq, accepted = 0, false
		}
	}()

	cfg := cloneConfig(c.cfgFn())

	if cfg.SampleRate < 1 && rand.Float64() >= cfg.SampleRate {
		c.stats.drop(dropSampled)
		c.internalDiag.logf("report sampled out (SampleRate=%v)", cfg.SampleRate)
		return 0, false
	}

	attributes := map[string]interface{}{}
	for k, v := range staticAttributes() {
		attributes[k] = v
	}
	updateAttrsWithProcMemInfo(attributes, c.internalDiag)
	runtimeAttributes(attributes)

	// Config-level attributes (cloned at capture; safe to iterate).
	for k, v := range cfg.Attributes {
		attributes[k] = v
	}
	// Client-wide attributes set via SetAttribute.
	c.amu.Lock()
	for k, v := range c.attributes {
		attributes[k] = v
	}
	c.amu.Unlock()

	attributes["error.message"] = in.message
	attributes["report_type"] = in.reportType

	classifiers := []string{in.classifier}
	annotations := map[string]interface{}{}

	if in.err != nil {
		chain := unwrapErrorChain(in.err, cfg.MaxErrorDepth, cfg.MaxErrorNodes)
		if len(chain) > 0 {
			attributes["error.type"] = chain[0].Type
			annotations["Error Chain"] = chain
		}
	}

	// Per-call attributes win over everything; the caller's map is copied,
	// never retained or mutated.
	for k, v := range in.extra {
		attributes[k] = v
	}

	if cfg.SendEnvVars {
		annotations["Environment Variables"] = getEnvVars(cfg.ScrubEnvVars)
	}
	if crumbs := c.crumbs.snapshot(); len(crumbs) > 0 {
		annotations["breadcrumbs"] = crumbs
	}

	return c.enqueueContext(ctx, clientJob{data: &queuedReport{
		cfg:         cfg,
		stack:       captureStack(cfg.CaptureAllGoroutines, cfg.MaxStackBytes),
		attributes:  attributes,
		annotations: annotations,
		classifiers: classifiers,
		reportType:  in.reportType,
		timestamp:   time.Now().Unix(),
	}})
}

// enqueueContext admits a job to the queue under the enqueue lock, assigning
// its sequence number. A full queue is retried on worker progress until ctx
// is done (the nonBlocking sentinel makes exactly one attempt). The lock is
// never held across a blocking operation.
func (c *Client) enqueueContext(ctx context.Context, j clientJob) (uint64, bool) {
	if ctx == nil {
		ctx = nonBlocking
	}
	for {
		// Grab the broadcast channel BEFORE the admission attempt: any
		// queue space freed after the attempt comes from a completion
		// that closes exactly this channel, so the wake-up cannot be
		// lost in between.
		wait := c.progressCh()

		c.enqueueMu.Lock()
		if c.closed {
			c.enqueueMu.Unlock()
			c.stats.drop(dropClosed)
			c.internalDiag.logf("report dropped: client closed")
			return 0, false
		}
		next := c.accepted + 1
		j.seq = next
		select {
		case c.queue <- j:
			c.accepted = next
			c.enqueueMu.Unlock()
			c.stats.accepted.Add(1)
			return next, true
		default:
		}
		c.enqueueMu.Unlock()

		select {
		case <-ctx.Done():
			c.stats.drop(dropQueueFull)
			c.internalDiag.logf("report dropped: queue full (capacity %d)", cap(c.queue))
			return 0, false
		case <-wait:
		case <-c.workerDone:
			c.stats.drop(dropClosed)
			return 0, false
		}
	}
}

// worker is the single consumer of the queue. It exits when Close closes the
// queue, after draining remaining jobs. Every processed job broadcasts
// progress to flushers and blocked panic enqueuers.
func (c *Client) worker() {
	defer close(c.workerDone)
	defer c.signalProgress()
	for j := range c.queue {
		c.processAndSend(j.data)
		c.processed.Store(j.seq)
		c.signalProgress()
	}
}

// processAndSend turns a queued report into the wire payload and delivers
// it, using the configuration snapshot taken at capture time. Runs on the
// worker goroutine; all failure modes degrade to a debug log plus a stats
// counter.
func (c *Client) processAndSend(qr *queuedReport) {
	defer c.recoverInternal("processAndSend")

	cfg := qr.cfg
	d := diag{logger: cfg.Logger, debug: cfg.Debug, busy: &c.logBusy}

	// Machine and build metadata are gathered lazily (never at import
	// time) and merged without overriding caller-provided values.
	if !cfg.DisableMachineAttributes {
		for k, v := range machineAttributes(d) {
			if _, exists := qr.attributes[k]; !exists {
				qr.attributes[k] = v
			}
		}
		// The stable machine identifier is opt-in, and its probe only
		// runs once someone opts in.
		if cfg.SendMachineID {
			if guid := machineGUID(d); guid != "" {
				if _, exists := qr.attributes["guid"]; !exists {
					qr.attributes["guid"] = guid
				}
			}
		}
	}
	biAttrs, modules := buildInfoAttributes()
	for k, v := range biAttrs {
		if _, exists := qr.attributes[k]; !exists {
			qr.attributes[k] = v
		}
	}
	if len(modules) > 0 {
		if _, exists := qr.annotations["Dependencies"]; !exists {
			qr.annotations["Dependencies"] = modules
		}
	}

	threads, sourceCode, mainThread := buildThreads(qr.stack, sourceOptions{
		mode:         cfg.SourceCode,
		contextLines: cfg.ContextLineCount,
		tabWidth:     cfg.TabWidth,
		roots:        cfg.SourceRoots,
		maxFileBytes: cfg.MaxSourceFileBytes,
		maxTotal:     cfg.MaxSourceBytes,
	})

	report := &ReportData{
		UUID:        uuid4(),
		Timestamp:   qr.timestamp,
		Classifiers: qr.classifiers,
		Attributes:  qr.attributes,
		Annotations: qr.annotations,
		Threads:     threads,
		SourceCode:  sourceCode,
		MainThread:  mainThread,
	}
	// A negative MaxAttachments disables attachments entirely (documented);
	// don't even open the files.
	if cfg.MaxAttachments >= 0 {
		report.Attachments = append([]string(nil), cfg.AttachmentPaths...)
	}

	if cfg.BeforeSend != nil {
		if modified := c.runBeforeSend(cfg.BeforeSend, report, d); modified == nil {
			// Counts both deliberate drops (hook returned nil) and
			// panicking hooks (fail closed).
			c.stats.drop(dropBeforeSend)
			d.logf("report %s dropped by BeforeSend", report.UUID)
			return
		} else {
			report = modified
		}
	}

	body, err := json.Marshal(report.toWire())
	if err != nil {
		c.stats.drop(dropSerialization)
		d.logf("report %s dropped: serialization failed (%T)", report.UUID, err)
		return
	}
	if len(body) > cfg.MaxReportBytes {
		c.stats.drop(dropOversize)
		d.logf("report %s dropped: %d bytes exceeds the %d-byte report limit",
			report.UUID, len(body), cfg.MaxReportBytes)
		return
	}

	// Breadcrumbs also travel as the bt-breadcrumbs-0 attachment, the file
	// the Backtrace UI's breadcrumb view reads (same format as the other
	// Backtrace SDKs). Sourced from the annotation so a BeforeSend hook
	// that scrubbed or removed breadcrumbs is respected.
	var inline []inlinePart
	if crumbs, ok := report.Annotations["breadcrumbs"].([]Breadcrumb); ok && len(crumbs) > 0 {
		if data, err := json.Marshal(crumbs); err == nil {
			inline = append(inline, inlinePart{name: "bt-breadcrumbs-0", data: data})
		}
	}

	// Diagnostics are payload-free by design: report contents are never
	// logged (debug logging is often enabled during incidents, exactly
	// when secrets are most likely to be present).
	d.logf("sending report %s (type=%s, json_bytes=%d, attachments=%d, destination=%s)",
		report.UUID, qr.reportType, len(body),
		len(report.Attachments)+len(inline), redactURL(cfg.submissionURL()))

	reason, err := c.transport.send(c.ctx, cfg.submissionURL(), body,
		report.Attachments, inline, cfg.attachmentLimits(), d)
	if err != nil {
		c.stats.drop(reason)
		d.logf("report %s dropped: %v", report.UUID, err)
		return
	}
	c.stats.delivered.Add(1)
	d.logf("report %s delivered", report.UUID)
}

// runBeforeSend isolates user hook panics from the worker. A panicking hook
// drops the report (fail closed): hooks exist to scrub sensitive data, and a
// report in a half-scrubbed state must never leave the process.
func (c *Client) runBeforeSend(hook func(*ReportData) *ReportData, report *ReportData, d diag) (out *ReportData) {
	defer func() {
		if r := recover(); r != nil {
			// The caller's nil branch performs the stats accounting.
			// Log only the TYPE of the panic value: the hook exists to
			// scrub data, so its panic payload may itself be sensitive.
			d.logf("BeforeSend panicked (%T); dropping report %s", r, report.UUID)
			out = nil
		}
	}()
	return hook(report)
}

// recoverInternal is the last line of defense: an SDK bug must never crash
// the host application. It uses the fixed construction-time diagnostics and
// never re-enters configuration code.
func (c *Client) recoverInternal(where string) {
	if r := recover(); r != nil {
		// Type-only for the panic value: json.Marshal re-panics user
		// MarshalJSON panics, whose contents may be sensitive. The stack
		// is SDK frames and is needed for bug reports.
		c.stats.drop(dropInternal)
		buf := make([]byte, 4096)
		n := runtime.Stack(buf, false)
		c.internalDiag.logf("internal error in %s (please report to backtrace-labs/backtrace-go): %T\n%s", where, r, buf[:n])
	}
}

// captureStack returns the formatted stack trace of the calling goroutine,
// or of all goroutines when all is true, capped at maxBytes.
func captureStack(all bool, maxBytes int) []byte {
	if maxBytes < 1024 {
		maxBytes = 1024
	}
	size := 64 << 10
	if size > maxBytes {
		size = maxBytes
	}
	for {
		buf := make([]byte, size)
		n := runtime.Stack(buf, all)
		if n < len(buf) || size == maxBytes {
			return buf[:n]
		}
		size *= 2
		if size > maxBytes {
			size = maxBytes
		}
	}
}
