package bt

import (
	"context"
	"encoding/json"
	"maps"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
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
// Close/CloseContext, ReportPanicValueAndFlush, and ReportPanic.
type Client struct {
	// cfgFn returns the normalized configuration. For clients created by
	// NewClient it returns a fixed config; the legacy global client
	// re-reads bt.Options so that historical mutate-the-global usage
	// keeps working. Each report snapshots the config at capture time so
	// later global changes cannot reroute queued reports.
	cfgFn func() Config

	// stableConfig is true when cfgFn always returns the same normalised,
	// SDK-owned configuration (clients created by NewClient). The legacy
	// global client rebuilds its configuration from the mutable bt.Options
	// on every call and must detach it per report.
	stableConfig bool

	// internalDiag is fixed at construction and used on recovery paths,
	// where re-reading configuration could itself fail.
	internalDiag diag

	transport *httpTransport

	// ctx is the SDK root context; cancel aborts in-flight requests on
	// CloseContext deadline expiry.
	ctx    context.Context
	cancel context.CancelFunc

	lanes     [laneCount]lane
	enqueueMu sync.Mutex // guards closed, accepted, and queue admission
	closed    bool
	// closedFast mirrors closed for the lock-free rejection at the top of
	// capture; admission still re-checks closed under enqueueMu.
	closedFast atomic.Bool

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

	crumbs         *breadcrumbRing
	maxBreadcrumbs int

	stats internalStats

	// logBusy backs the diag re-entrancy guard for this client.
	logBusy atomic.Int32

	warnedInlineSkip atomic.Bool
}

// clientJob is a queued report with its admission sequence number.
type clientJob struct {
	seq  uint64
	data *queuedReport
}

type laneID int

const (
	laneRegular laneID = iota
	lanePriority
	laneCount
)

// Jobs in a lane must be processed in order. Flush targets compare sequence
// numbers.
type lane struct {
	queue     chan clientJob
	accepted  uint64
	processed atomic.Uint64
}

// queuedReport carries everything captured on the caller's goroutine,
// including the delivery/scrubbing configuration in effect at capture time;
// parsing and I/O happen on the worker.
type queuedReport struct {
	cfg         Config
	stack       []byte
	pcs         []uintptr
	threadName  string
	creator     []byte
	originPCs   []uintptr
	attributes  map[string]interface{}
	annotations map[string]interface{}
	classifiers []string
	breadcrumbs []Breadcrumb
	attachments []string
	kind        reportKind
	timestamp   int64
	err         error
	outcome     *reportOutcome
}

type reportOutcome struct {
	delivered atomic.Bool
}

type reportKind string

const (
	kindError   reportKind = "error"
	kindMessage reportKind = "message"
	kindPanic   reportKind = "panic"
	kindCrash   reportKind = "crash"
)

var flushFallbackTimeout = DefaultFlushTimeout

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
	return startClient(func() Config { return n }, true), nil
}

// startClient wires up a client around a config source and starts its worker.
func startClient(cfgFn func() Config, stable bool) *Client {
	cfg := cfgFn()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		cfgFn:           cfgFn,
		stableConfig:    stable,
		transport:       newHTTPTransport(cfg.HTTPClient, cfg.Timeout),
		ctx:             ctx,
		cancel:          cancel,
		progress:        make(chan struct{}),
		workerDone:      make(chan struct{}),
		shutdownTimeout: cfg.ShutdownTimeout,
		attributes:      map[string]interface{}{},
		crumbs:          newBreadcrumbRing(cfg.MaxBreadcrumbs),
		maxBreadcrumbs:  cfg.MaxBreadcrumbs,
	}
	for i := range c.lanes {
		c.lanes[i].queue = make(chan clientJob, cfg.QueueSize)
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
	c.ReportContext(context.Background(), object, extraAttributes)
}

// ReportContext is Report with the attributes, breadcrumbs and attachments
// of the Scope in ctx and of its parent scopes (see WithScope). The
// cancellation and deadline of ctx do not affect the report. A nil ctx
// carries no scope.
func (c *Client) ReportContext(ctx context.Context, object any, extraAttributes map[string]any) {
	if c == nil {
		return
	}
	switch v := object.(type) {
	case nil:
		return
	case error:
		c.capture(nonBlocking, captureInput{ctx: ctx, value: v, err: v, kind: kindError, extra: extraAttributes})
	default:
		c.capture(nonBlocking, captureInput{ctx: ctx, value: v, kind: kindMessage, extra: extraAttributes})
	}
}

// ReportError sends a report for err, capturing its type and unwrap graph.
// Its error.type attribute is "Exception" unless an attribute sets it.
func (c *Client) ReportError(err error, extraAttributes map[string]interface{}) {
	if c == nil || err == nil {
		return
	}
	c.capture(nonBlocking, captureInput{
		value: err,
		err:   err,
		kind:  kindError,
		extra: extraAttributes,
	})
}

// ReportMessage sends a plain message report.
// Its error.type attribute is "Message" unless an attribute sets it.
func (c *Client) ReportMessage(msg string, extraAttributes map[string]interface{}) {
	if c == nil {
		return
	}
	c.capture(nonBlocking, captureInput{
		value: msg,
		kind:  kindMessage,
		extra: extraAttributes,
	})
}

// ReportPanicValue sends a report for a recovered panic value without
// blocking: a full queue drops the report (counted in Stats). Use
// ReportPanicValueAndFlush when the goroutine or process is about to die
// and delivery must be awaited.
// Its error.type attribute is "Unhandled exception" unless an attribute sets
// it. Panic reports are never sampled out.
func (c *Client) ReportPanicValue(value interface{}, extraAttributes map[string]interface{}) {
	c.ReportPanicValueContext(context.Background(), value, extraAttributes)
}

// ReportPanicValueContext is ReportPanicValue with the Scope data of ctx, as
// described on ReportContext.
func (c *Client) ReportPanicValueContext(ctx context.Context, value any, extraAttributes map[string]any) {
	if c == nil || value == nil {
		return
	}
	c.capture(nonBlocking, panicCaptureInput(ctx, value, extraAttributes))
}

// ReportPanicValueAndFlush captures a recovered panic value and waits for
// its delivery under ONE deadline covering queue admission and flushing.
// It reports whether the report was accepted and processed in time.
//
// ctx sets the deadline. Without one, the call waits up to
// DefaultFlushTimeout. The panic report goes ahead of reports waiting in
// the queue. The call also waits for the reports queued before it, but
// they do not change the result. The report carries the Scope data of
// ctx, as described on ReportContext.
func (c *Client) ReportPanicValueAndFlush(ctx context.Context, value any, extraAttributes map[string]any) bool {
	processed, _ := c.reportPanicAndWait(ctx, value, extraAttributes)
	return processed
}

func (c *Client) reportPanicAndWait(ctx context.Context, value any, extraAttributes map[string]any) (processed, delivered bool) {
	if c == nil || value == nil {
		return true, false
	}
	captured := false
	defer func() {
		// Caller-supplied context methods can panic outside capture.
		if recover() != nil {
			if !captured {
				c.stats.drop(dropInternal)
			}
			processed, delivered = false, false
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, flushFallbackTimeout)
		defer cancel()
	}

	targets := c.acceptedTargets()
	in := panicCaptureInput(ctx, value, extraAttributes)
	in.lane = lanePriority
	in.outcome = new(reportOutcome)
	captured = true
	seq, accepted := c.capture(ctx, in)
	if !accepted {
		c.flushTargetContext(ctx, targets)
		return false, false
	}
	targets[lanePriority] = seq
	c.flushTargetContext(ctx, targets)
	processed = c.lanes[lanePriority].processed.Load() >= seq
	return processed, in.outcome.delivered.Load()
}

// ReportPanic reports a panic and re-panics with the original value, after
// waiting up to DefaultFlushTimeout as ReportPanicValueAndFlush does. Defer
// it directly:
//
//	defer client.ReportPanic(nil)
//
// When the report was delivered, MonitorCrashes does not report the
// resulting crash again (see MonitorCrashes). On a nil *Client it does not
// recover, and the panic proceeds unchanged.
func (c *Client) ReportPanic(extraAttributes map[string]any) {
	if c == nil {
		return
	}
	v := recover()
	if v == nil {
		return
	}
	if _, delivered := c.reportPanicAndWait(context.Background(), v, extraAttributes); delivered {
		markDeliveredPanic(v)
	}
	panic(v)
}

// ReportAndRecoverPanic reports a panic and swallows it, without waiting for
// delivery. Defer it directly:
//
//	defer client.ReportAndRecoverPanic(nil)
//
// On a nil *Client it does not recover, and the panic proceeds unchanged.
func (c *Client) ReportAndRecoverPanic(extraAttributes map[string]any) {
	if c == nil {
		return
	}
	if v := recover(); v != nil {
		c.ReportPanicValue(v, extraAttributes)
	}
}

func panicCaptureInput(ctx context.Context, value interface{}, extra map[string]interface{}) captureInput {
	in := captureInput{
		ctx:   ctx,
		value: value,
		kind:  kindPanic,
		extra: extra,
	}
	if err, ok := value.(error); ok {
		in.err = err
	}
	return in
}

// SetAttribute sets a client-wide attribute included in every subsequent
// report. Safe for concurrent use. The value is normalised into a detached
// copy before the call returns (see ReportData for the value model), so the
// caller may keep changing its own maps and slices afterwards.
// A nil value removes the attribute from reports, including SDK defaults
// such as "hostname".
func (c *Client) SetAttribute(key string, value interface{}) {
	if c == nil {
		return
	}
	// Freeze on the caller's goroutine so a value the caller keeps
	// mutating cannot later race the worker.
	frozen := freezeValue(value, c.cfgFn().valueLimits())
	c.amu.Lock()
	defer c.amu.Unlock()
	c.attributes[key] = frozen
}

// SetAttributes sets multiple client-wide attributes atomically. The map is
// copied.
func (c *Client) SetAttributes(attrs map[string]interface{}) {
	if c == nil {
		return
	}
	frozen := freezeAttributes(attrs, c.cfgFn().valueLimits())
	c.amu.Lock()
	defer c.amu.Unlock()
	for k, v := range frozen {
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
// A panic report from ReportPanicValueAndFlush or ReportPanic made after the
// call is sent first and can lengthen the wait.
func (c *Client) FlushContext(ctx context.Context) bool {
	if c == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return c.flushTargetContext(ctx, c.acceptedTargets())
}

func (c *Client) acceptedTargets() [laneCount]uint64 {
	var targets [laneCount]uint64
	c.enqueueMu.Lock()
	for i := range c.lanes {
		targets[i] = c.lanes[i].accepted
	}
	c.enqueueMu.Unlock()
	return targets
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

// flushTargetContext waits until the worker has processed every lane up to
// its sequence number target. The broadcast channel is grabbed before each
// re-check so an advance signaled in between cannot be lost.
func (c *Client) flushTargetContext(ctx context.Context, targets [laneCount]uint64) bool {
	for {
		wait := c.progressCh()
		if c.reached(targets) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-wait:
		case <-c.workerDone:
			return c.reached(targets)
		}
	}
}

func (c *Client) reached(targets [laneCount]uint64) bool {
	for i := range c.lanes {
		if c.lanes[i].processed.Load() < targets[i] {
			return false
		}
	}
	return true
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
		c.closedFast.Store(true)
		for i := range c.lanes {
			close(c.lanes[i].queue)
		}
		c.enqueueMu.Unlock()
		c.signalProgress()
	})
}

// captureInput bundles the per-call capture parameters.
type captureInput struct {
	ctx     context.Context
	value   any
	err     error
	kind    reportKind
	extra   map[string]interface{}
	lane    laneID
	outcome *reportOutcome
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
	if ctx == nil {
		ctx = nonBlocking
	}

	// Cheap rejections first: a closed client and sampled-out reports must
	// not pay for normalising attributes, /proc, runtime, error-graph and
	// stack capture only to be dropped. Neither check takes a lock.
	if c.closedFast.Load() {
		c.stats.drop(dropClosed)
		c.internalDiag.logf("report dropped: client closed")
		return 0, false
	}
	if q := c.lanes[in.lane].queue; ctx.Err() != nil && len(q) == cap(q) {
		c.stats.drop(dropQueueFull)
		c.internalDiag.logf("report dropped: queue full (capacity %d)", cap(q))
		return 0, false
	}

	cfg := c.cfgFn()

	if !c.stableConfig {
		if err := cfg.validate(); err != nil {
			logReportingDisabled(cfg, err)
			return 0, false
		}
	}

	if in.kind != kindPanic && cfg.SampleRate < 1 && rand.Float64() >= cfg.SampleRate {
		c.stats.drop(dropSampled)
		c.internalDiag.logf("report sampled out (SampleRate=%v)", cfg.SampleRate)
		return 0, false
	}

	if !c.stableConfig {
		// Legacy path: cfg was just built from the mutable bt.Options, so
		// its attributes may still alias caller memory. Detach them now.
		cfg = cloneConfig(cfg)
	}

	limits := cfg.valueLimits()

	attributes := map[string]interface{}{}
	for k, v := range staticAttributes() {
		attributes[k] = v
	}
	updateAttrsWithProcMemInfo(attributes, c.internalDiag)
	runtimeAttributes(attributes)

	// Config-level attributes: normalised at NewClient (or just above for
	// the legacy path), so the values are SDK-owned and immutable.
	for k, v := range cfg.Attributes {
		attributes[k] = v
	}
	// Client-wide attributes set via SetAttribute (normalised at store time).
	c.amu.Lock()
	for k, v := range c.attributes {
		attributes[k] = v
	}
	c.amu.Unlock()
	scopes := scopeChain(in.ctx)
	for _, s := range scopes {
		s.copyAttributes(attributes)
	}

	message, isString := in.value.(string)
	if in.err != nil {
		message = safeErrorString(in.err)
	} else if !isString {
		message = printValue(in.value)
	}
	attributes["error.message"] = message
	attributes["report_type"] = "error"
	if in.kind == kindPanic {
		attributes["report_type"] = "panic"
	}

	crumbs := mergeBreadcrumbs(c.crumbs.snapshot(), scopes, c.maxBreadcrumbs)
	if len(crumbs) > 0 {
		attributes["breadcrumbs.lastId"] = maxBreadcrumbID(crumbs)
	}

	classifier := "panic"
	if in.kind == kindMessage {
		classifier = "message"
	}
	annotations := map[string]interface{}{}

	var originPCs []uintptr
	if in.err != nil {
		classifier = errorClassifier(in.err)
		nodes := walkErrorGraph(in.err, cfg.MaxErrorDepth, cfg.MaxErrorNodes)
		if chain := errorChainLinks(nodes); len(chain) > 0 {
			annotations["Error Chain"] = chain
		}
		originPCs = errorOriginPCs(nodes)
	}

	// Per-call attributes win over everything. They are normalised here, on
	// the caller's goroutine and under one shared budget, so the queued
	// report can never share a live map or slice with the caller (which
	// would race the worker's json.Marshal).
	for k, v := range freezeAttributes(in.extra, limits) {
		attributes[k] = v
	}
	if _, set := attributes["error.type"]; !set {
		attributes["error.type"] = defaultErrorTypes[in.kind]
	}

	if len(cfg.EnvVars) > 0 || cfg.SendEnvVars {
		annotations["Environment Variables"] = getEnvVars(envCaptureOptions{
			allowlist:     cfg.EnvVars,
			extraPatterns: cfg.ScrubEnvVars,
			maxVars:       cfg.MaxEnvVars,
			maxBytes:      cfg.MaxEnvBytes,
		})
	}

	qr := &queuedReport{
		cfg:         cfg,
		originPCs:   originPCs,
		attributes:  attributes,
		annotations: annotations,
		classifiers: []string{classifier},
		breadcrumbs: crumbs,
		attachments: scopeAttachments(scopes),
		kind:        in.kind,
		timestamp:   time.Now().Unix(),
		err:         in.err,
		outcome:     in.outcome,
	}
	if cfg.CaptureAllGoroutines {
		qr.stack = captureStack(true, cfg.MaxStackBytes)
	} else {
		qr.pcs = callerPCs()
		qr.threadName, qr.creator = callerGoroutine()
	}
	return c.enqueueContext(ctx, in.lane, clientJob{data: qr})
}

var defaultErrorTypes = map[reportKind]string{
	kindError:   "Exception",
	kindMessage: "Message",
	kindPanic:   "Unhandled exception",
}

// enqueueContext admits a job to the queue under the enqueue lock, assigning
// its sequence number. A full queue is retried on worker progress until ctx
// is done (the nonBlocking sentinel makes exactly one attempt). The lock is
// never held across a blocking operation.
func (c *Client) enqueueContext(ctx context.Context, id laneID, j clientJob) (uint64, bool) {
	if ctx == nil {
		ctx = nonBlocking
	}
	l := &c.lanes[id]
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
		next := l.accepted + 1
		j.seq = next
		select {
		case l.queue <- j:
			l.accepted = next
			c.enqueueMu.Unlock()
			c.stats.accepted.Add(1)
			return next, true
		default:
		}
		c.enqueueMu.Unlock()

		select {
		case <-ctx.Done():
			c.stats.drop(dropQueueFull)
			c.internalDiag.logf("report dropped: queue full (capacity %d)", cap(l.queue))
			return 0, false
		case <-wait:
		case <-c.workerDone:
			c.stats.drop(dropClosed)
			return 0, false
		}
	}
}

const priorityBurst = 8

// worker is the single consumer of the queues. It exits when Close closes the
// queues, after draining remaining jobs. Every processed job broadcasts
// progress to flushers and blocked panic enqueuers. Priority jobs go first,
// with one waiting regular job after every priorityBurst priority jobs.
func (c *Client) worker() {
	defer close(c.workerDone)
	defer c.signalProgress()
	var queues [laneCount]chan clientJob
	for i := range c.lanes {
		queues[i] = c.lanes[i].queue
	}
	streak := 0
	for queues[laneRegular] != nil || queues[lanePriority] != nil {
		first := lanePriority
		if streak >= priorityBurst {
			first = laneRegular
		}
		var (
			id laneID
			j  clientJob
			ok bool
		)
		select {
		case j, ok = <-queues[first]:
			id = first
		default:
			select {
			case j, ok = <-queues[lanePriority]:
				id = lanePriority
			case j, ok = <-queues[laneRegular]:
				id = laneRegular
			}
		}
		if !ok {
			queues[id] = nil
			continue
		}
		c.run(id, j)
		if id == lanePriority {
			streak++
		} else {
			streak = 0
		}
	}
}

func (c *Client) run(id laneID, j clientJob) {
	c.processAndSend(j.data, id)
	c.lanes[id].processed.Store(j.seq)
	c.signalProgress()
}

// Only a regular report's retry wait may run priority jobs. Running them
// anywhere else breaks lane order.
func (c *Client) retryWait(id laneID) func(time.Duration) bool {
	return func(d time.Duration) bool {
		timer := time.NewTimer(d)
		defer timer.Stop()
		var urgent chan clientJob
		if id == laneRegular {
			urgent = c.lanes[lanePriority].queue
		}
		for {
			// A fired timer wins over waiting priority jobs.
			select {
			case <-timer.C:
				return true
			case <-c.ctx.Done():
				return false
			default:
			}
			select {
			case <-timer.C:
				return true
			case <-c.ctx.Done():
				return false
			case j, ok := <-urgent:
				if !ok {
					urgent = nil
					continue
				}
				c.run(lanePriority, j)
			}
		}
	}
}

// processAndSend turns a queued report into the wire payload and delivers
// it, using the configuration snapshot taken at capture time. Runs on the
// worker goroutine; all failure modes degrade to a debug log plus a stats
// counter.
func (c *Client) processAndSend(qr *queuedReport, id laneID) {
	defer c.recoverInternal("processAndSend")

	// Forced shutdown: the deadline expired and the root context was
	// cancelled. Skip the remaining local work (metadata, source, hooks,
	// serialization) instead of preparing a report the transport will
	// immediately fail.
	if c.ctx.Err() != nil {
		c.stats.drop(dropShutdown)
		return
	}

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
			qr.annotations["Dependencies"] = dependencyAnnotation(modules)
		}
	}

	threads, sourceCode, mainThread := reportThreads(qr, sourceOptions{
		mode:         cfg.SourceCode,
		contextLines: cfg.ContextLineCount,
		tabWidth:     cfg.TabWidth,
		roots:        cfg.SourceRoots,
		maxFileBytes: cfg.MaxSourceFileBytes,
		maxTotal:     cfg.MaxSourceBytes,
	})

	// eventID is the SDK-private identity of this report: diagnostics
	// always use it, so a BeforeSend hook cannot make log lines refer to
	// a UUID that was never submitted.
	eventID := uuid4()

	report := &ReportData{
		UUID:        eventID,
		Timestamp:   qr.timestamp,
		Classifiers: qr.classifiers,
		Attributes:  qr.attributes,
		Annotations: qr.annotations,
		Threads:     threads,
		SourceCode:  sourceCode,
		MainThread:  mainThread,
		Breadcrumbs: qr.breadcrumbs,
		Error:       qr.err,
	}
	ensureMainThread(report)
	threads, mainThread = report.Threads, report.MainThread
	// A negative MaxAttachments disables attachments entirely (documented);
	// don't even open the files.
	if cfg.MaxAttachments >= 0 {
		report.Attachments = append([]string(nil), cfg.AttachmentPaths...)
		for _, p := range qr.attachments {
			if !slices.Contains(report.Attachments, p) {
				report.Attachments = append(report.Attachments, p)
			}
		}
	}

	if cfg.BeforeSend != nil {
		// Values that came from Config.Attributes, SetAttribute or a scope
		// are shared between reports. Give the hook its
		// own copy, so "mutating maps and slices in place is safe" holds
		// for this report only. The copy also widens the SDK's own plain
		// numbers, so the hook sees value-model types throughout.
		report.Attributes = copyModelMap(report.Attributes)
		report.Classifiers = slices.Clone(report.Classifiers)
		// A hook may clear r.Threads in place. threads must keep the originals.
		report.Threads = maps.Clone(report.Threads)

		modified := c.runBeforeSend(cfg.BeforeSend, report, d)
		if modified == nil {
			// Counts both deliberate drops (hook returned nil) and
			// panicking hooks (fail closed).
			c.stats.drop(dropBeforeSend)
			d.logf("report %s dropped by BeforeSend", eventID)
			return
		}
		report = modified
		enforceReportInvariants(report, eventID, qr, threads, mainThread)
		if cfg.MaxAttachments < 0 {
			report.Attachments = nil
		}
	}

	normalizeAttributes(report.Attributes, report.Annotations)
	crumbs := report.Breadcrumbs[:0:0]
	for _, b := range report.Breadcrumbs {
		if b, ok := prepareBreadcrumb(b); ok {
			crumbs = append(crumbs, b)
		}
	}
	report.Breadcrumbs = crumbs
	body, err := json.Marshal(report.toWire())
	if err == nil && len(body) > cfg.MaxReportBytes {
		size := len(body)
		var steps []string
		body, steps, err = shrinkReport(report, cfg.MaxReportBytes)
		if err == nil && body == nil {
			c.stats.drop(dropOversize)
			d.logf("report %s dropped: %d bytes exceeds the %d-byte report limit",
				eventID, size, cfg.MaxReportBytes)
			return
		}
		d.logf("report %s trimmed from %d bytes (%s)", eventID, size, strings.Join(steps, ","))
	}
	if err != nil {
		c.stats.drop(dropSerialization)
		d.logf("report %s dropped: serialization failed (%T)", eventID, err)
		return
	}

	// Breadcrumbs travel as the bt-breadcrumbs-0 attachment, the file
	// the Backtrace UI's breadcrumb view reads (same format as the other
	// Backtrace SDKs). Sourced from report.Breadcrumbs so a BeforeSend hook
	// that scrubbed or removed breadcrumbs is respected.
	var inline []inlinePart
	if len(report.Breadcrumbs) > 0 {
		if data, err := json.Marshal(report.Breadcrumbs); err == nil {
			inline = append(inline, inlinePart{name: "bt-breadcrumbs-0", data: data})
		}
	}

	p, err := buildPayload(body, report.Attachments, inline, cfg.attachmentLimits(), !cfg.DisableCompression,
		&c.warnedInlineSkip, d)
	if err != nil {
		c.stats.drop(dropSerialization)
		d.logf("report %s dropped: %v", eventID, err)
		return
	}

	// Diagnostics are payload-free by design: report contents are never
	// logged (debug logging is often enabled during incidents, exactly
	// when secrets are most likely to be present).
	d.logf("sending report %s (type=%s, json_bytes=%d, attachments=%d, destination=%s)",
		eventID, qr.kind, len(body),
		len(report.Attachments)+len(inline), urlOrigin(cfg.submissionURL()))

	res := c.transport.send(c.ctx, cfg.submissionURL(), p, cfg.MaxRetries, c.retryWait(id), d)
	c.stats.retries.Add(uint64(res.retries))
	if res.err != nil {
		c.stats.drop(res.reason)
		d.logf("report %s dropped: %v", eventID, res.err)
		return
	}
	c.stats.delivered.Add(1)
	if qr.outcome != nil {
		qr.outcome.delivered.Store(true)
	}
	d.logf("report %s delivered", eventID)
}

const originThread = "origin"

func reportThreads(qr *queuedReport, opts sourceOptions) (map[string]Thread, map[string]SourceCode, string) {
	sources := newSourceBuilder(opts)
	defer sources.close()
	var (
		threads    map[string]Thread
		mainThread string
	)
	if qr.cfg.CaptureAllGoroutines || qr.kind == kindCrash {
		threads, mainThread = parseStackText(qr.stack, sources)
	} else {
		mainThread = "0"
		frames := framesFromPCs(qr.pcs, sources)
		creator, key := parseStackText(qr.creator, sources)
		threads = map[string]Thread{mainThread: {
			Name:   qr.threadName,
			Fault:  true,
			Stacks: append(frames, creator[key].Stacks...),
		}}
	}
	if qr.kind == kindPanic || qr.kind == kindCrash {
		markPanicFrame(threads[mainThread].Stacks)
	}
	if len(qr.originPCs) > 0 {
		if frames := framesFromPCs(qr.originPCs, sources); len(frames) > 0 {
			if t, ok := threads[mainThread]; ok {
				t.Fault = false
				threads[mainThread] = t
			}
			threads[originThread] = Thread{Name: "error origin", Fault: true, Stacks: frames}
			mainThread = originThread
		}
	}
	return threads, sources.result(), mainThread
}

// enforceReportInvariants turns whatever a BeforeSend hook returned into a
// valid submission WITHOUT undoing the hook's edits. The hook exists to
// scrub, so removing source text, frames or whole threads must stick; only
// the report identity is SDK-owned. What is enforced is what the wire format
// requires: an id, a timestamp, at least one thread, a main thread that names
// one of them, and no frame pointing at a source entry that is gone.
func enforceReportInvariants(report *ReportData, eventID string, qr *queuedReport, threads map[string]Thread, mainThread string) {
	report.UUID = eventID
	if report.Timestamp <= 0 {
		report.Timestamp = qr.timestamp
	}
	report.Classifiers = slices.DeleteFunc(slices.Clone(report.Classifiers), func(c string) bool { return c == "" })
	if len(report.Classifiers) == 0 {
		report.Classifiers = qr.classifiers
	}
	if report.Attributes == nil {
		report.Attributes = map[string]interface{}{}
	}
	if report.Annotations == nil {
		report.Annotations = map[string]interface{}{}
	}

	if len(report.Threads) == 0 {
		// A report without threads is rejected by the server.
		report.Threads = threads
		report.MainThread = mainThread
	}
	if _, ok := report.Threads[report.MainThread]; !ok {
		report.MainThread = pickMainThread(report.Threads, mainThread)
	}

	for id, thread := range report.Threads {
		var frames []StackFrame // copied only when a frame has to change
		for i, frame := range thread.Stacks {
			if frame.SourceCodeID == "" {
				continue
			}
			if _, ok := report.SourceCode[frame.SourceCodeID]; ok {
				continue
			}
			if frames == nil {
				frames = append([]StackFrame(nil), thread.Stacks...)
			}
			frames[i].SourceCodeID = ""
		}
		if frames != nil {
			thread.Stacks = frames
			report.Threads[id] = thread
		}
	}
}

// pickMainThread chooses the main thread among the threads a hook left:
// the original one when it survived, else the faulting one, else the
// smallest key (so the choice is deterministic).
func pickMainThread(threads map[string]Thread, original string) string {
	if _, ok := threads[original]; ok {
		return original
	}
	best := ""
	for id, thread := range threads {
		if thread.Fault {
			return id
		}
		if best == "" || id < best {
			best = id
		}
	}
	return best
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
