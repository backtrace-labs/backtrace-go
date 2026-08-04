package bt

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Client is an instance-based Backtrace reporter. Multiple independent
// clients may coexist in one process. All methods are safe for concurrent
// use, never block the caller on network I/O, and never panic.
//
// Reports are queued to a background worker; when the queue is full new
// reports are dropped and counted (see DroppedReports) instead of blocking.
// Call Flush to wait for delivery of queued reports and Close to shut the
// client down.
type Client struct {
	// cfgFn returns the normalized configuration. For clients created by
	// NewClient it returns a fixed config; the legacy global client
	// re-reads bt.Options so that historical mutate-the-global usage
	// keeps working.
	cfgFn func() Config

	transport *httpTransport

	queue      chan clientJob
	qmu        sync.RWMutex // guards closed + sends into queue
	closed     bool
	workerDone chan struct{}

	amu        sync.Mutex // guards attributes
	attributes map[string]interface{}

	crumbs *breadcrumbRing

	dropped atomic.Uint64
}

// clientJob is either a queued report (data != nil) or a flush marker.
type clientJob struct {
	data  *queuedReport
	flush chan struct{}
}

// queuedReport carries everything captured on the caller's goroutine;
// parsing and I/O happen on the worker.
type queuedReport struct {
	stack       []byte
	attributes  map[string]interface{}
	annotations map[string]interface{}
	classifiers []string
	timestamp   int64
}

// NewClient creates and starts a reporter with the given configuration.
// It returns an error when the endpoint is missing or unparsable.
func NewClient(cfg Config) (*Client, error) {
	n := cfg.normalize()
	if err := n.validate(); err != nil {
		return nil, err
	}
	return startClient(func() Config { return n }), nil
}

// startClient wires up a client around a config source and starts its worker.
func startClient(cfgFn func() Config) *Client {
	cfg := cfgFn()
	c := &Client{
		cfgFn:      cfgFn,
		transport:  newHTTPTransport(cfg.HTTPClient, cfg.Timeout),
		queue:      make(chan clientJob, cfg.QueueSize),
		workerDone: make(chan struct{}),
		attributes: map[string]interface{}{},
		crumbs:     newBreadcrumbRing(cfg.MaxBreadcrumbs),
	}
	go c.worker()
	return c
}

func (c *Client) diag() diag {
	cfg := c.cfgFn()
	return diag{logger: cfg.Logger, debug: cfg.Debug}
}

// Report sends an error report. object may be an error (its message,
// type and unwrap chain are captured) or any value convertible to a string
// (reported as a message). A nil object is ignored. extraAttributes are
// added to this report only; the map is not retained or mutated.
func (c *Client) Report(object interface{}, extraAttributes map[string]interface{}) {
	switch v := object.(type) {
	case nil:
		return
	case error:
		c.ReportError(v, extraAttributes)
	default:
		c.ReportMessage(fmt.Sprint(v), extraAttributes)
	}
}

// ReportError sends a report for err, capturing its type and unwrap chain.
func (c *Client) ReportError(err error, extraAttributes map[string]interface{}) {
	if err == nil {
		return
	}
	c.capture(captureInput{
		message:    err.Error(),
		err:        err,
		classifier: "error",
		reportType: "error",
		extra:      extraAttributes,
	})
}

// ReportMessage sends a plain message report.
func (c *Client) ReportMessage(msg string, extraAttributes map[string]interface{}) {
	c.capture(captureInput{
		message:    msg,
		classifier: "message",
		reportType: "message",
		extra:      extraAttributes,
	})
}

// ReportPanicValue sends a report for a recovered panic value. It does not
// call recover itself and does not re-panic; it is intended for middleware
// and custom panic handlers. Call Flush afterwards when the process (or
// goroutine) is about to die.
//
// Unlike regular reports, a panic report retries a full queue for up to
// DefaultFlushTimeout before being dropped: it is likely the process's
// last report.
func (c *Client) ReportPanicValue(value interface{}, extraAttributes map[string]interface{}) {
	if value == nil {
		return
	}
	in := captureInput{
		message:     fmt.Sprint(value),
		classifier:  "panic",
		reportType:  "panic",
		extra:       extraAttributes,
		enqueueWait: DefaultFlushTimeout,
	}
	if err, ok := value.(error); ok {
		in.err = err
	}
	c.capture(in)
}

// SetAttribute sets a client-wide attribute included in every subsequent
// report. Safe for concurrent use.
func (c *Client) SetAttribute(key string, value interface{}) {
	c.amu.Lock()
	defer c.amu.Unlock()
	c.attributes[key] = value
}

// SetAttributes sets multiple client-wide attributes atomically.
func (c *Client) SetAttributes(attrs map[string]interface{}) {
	c.amu.Lock()
	defer c.amu.Unlock()
	for k, v := range attrs {
		c.attributes[k] = v
	}
}

// AddBreadcrumb records a breadcrumb attached to every subsequent report as
// part of the "breadcrumbs" annotation. Safe for concurrent use.
func (c *Client) AddBreadcrumb(b Breadcrumb) {
	c.crumbs.add(b)
}

// DroppedReports returns the number of reports dropped because the queue was
// full, the client was closed, or delivery failed.
func (c *Client) DroppedReports() uint64 {
	return c.dropped.Load()
}

// flushPollInterval paces retries when the queue is too full to accept a
// flush marker or a panic report immediately.
const flushPollInterval = 10 * time.Millisecond

// Flush blocks until all reports queued at the time of the call have been
// processed, or until timeout elapses. It reports whether the drain
// completed in time. Unlike the legacy FinishSendingReports, Flush never
// stops the worker: the client remains fully usable afterwards.
func (c *Client) Flush(timeout time.Duration) bool {
	marker := make(chan struct{})
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		c.qmu.RLock()
		if c.closed {
			c.qmu.RUnlock()
			// Close drains the queue; wait for the worker to
			// finish, bounded by the timeout.
			select {
			case <-c.workerDone:
				return true
			case <-timer.C:
				return false
			}
		}
		// Non-blocking attempt only: holding qmu across a blocking
		// send would stall every Report() caller behind a queued
		// Close (RWMutex writer preference).
		select {
		case c.queue <- clientJob{flush: marker}:
			c.qmu.RUnlock()
			select {
			case <-marker:
				return true
			case <-timer.C:
				return false
			}
		default:
		}
		c.qmu.RUnlock()

		select {
		case <-timer.C:
			return false
		case <-time.After(flushPollInterval):
		}
	}
}

// Close drains the queue, stops the worker, and releases the client.
// Subsequent reports are dropped (and counted). Close is idempotent.
// Call Flush first if you need a bounded wait; Close waits for the full
// drain (each send is bounded by the configured timeout).
func (c *Client) Close() {
	c.qmu.Lock()
	if !c.closed {
		c.closed = true
		close(c.queue)
	}
	c.qmu.Unlock()
	<-c.workerDone
}

// captureInput bundles the per-call capture parameters.
type captureInput struct {
	message    string
	err        error
	classifier string
	reportType string
	extra      map[string]interface{}
	// enqueueWait bounds how long a full queue is retried before the
	// report is dropped; zero means drop immediately (never block).
	enqueueWait time.Duration
}

// capture assembles everything that must be observed on the caller's
// goroutine (stack, attribute snapshot) and enqueues the report. It never
// blocks on the queue and never panics.
func (c *Client) capture(in captureInput) {
	defer c.recoverInternal("capture")

	cfg := c.cfgFn()

	if cfg.SampleRate < 1 && rand.Float64() >= cfg.SampleRate {
		c.diag().logf("report sampled out (SampleRate=%v)", cfg.SampleRate)
		return
	}

	attributes := map[string]interface{}{}
	for k, v := range staticAttributes() {
		attributes[k] = v
	}
	updateAttrsWithProcMemInfo(attributes, c.diag())
	runtimeAttributes(attributes)

	// Config-level attributes (treated as read-only after NewClient).
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
		chain := unwrapErrorChain(in.err, cfg.MaxErrorDepth)
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

	c.enqueue(clientJob{data: &queuedReport{
		stack:       captureStack(cfg.CaptureAllGoroutines),
		attributes:  attributes,
		annotations: annotations,
		classifiers: classifiers,
		timestamp:   time.Now().Unix(),
	}}, in.enqueueWait)
}

// enqueue queues a job without ever holding qmu across a blocking send.
// With wait <= 0 a full queue drops the report immediately (regular
// reports never block the caller). A positive wait — used for panic
// reports, which are the process's last words — retries for up to that
// duration before dropping.
func (c *Client) enqueue(j clientJob, wait time.Duration) {
	deadline := time.Now().Add(wait)
	for {
		c.qmu.RLock()
		if c.closed {
			c.qmu.RUnlock()
			c.dropped.Add(1)
			c.diag().logf("report dropped: client closed")
			return
		}
		select {
		case c.queue <- j:
			c.qmu.RUnlock()
			return
		default:
		}
		c.qmu.RUnlock()

		if wait <= 0 || !time.Now().Before(deadline) {
			c.dropped.Add(1)
			c.diag().logf("report dropped: queue full (capacity %d)", cap(c.queue))
			return
		}
		time.Sleep(flushPollInterval)
	}
}

// worker is the single consumer of the queue. It exits when Close closes the
// queue, after draining remaining jobs.
func (c *Client) worker() {
	defer close(c.workerDone)
	for j := range c.queue {
		if j.flush != nil {
			close(j.flush)
			continue
		}
		c.processAndSend(j.data)
	}
}

// processAndSend turns a queued report into the wire payload and delivers
// it. Runs on the worker goroutine; all failure modes degrade to a debug log
// plus the dropped counter.
func (c *Client) processAndSend(qr *queuedReport) {
	defer c.recoverInternal("processAndSend")

	cfg := c.cfgFn()
	d := diag{logger: cfg.Logger, debug: cfg.Debug}

	// Machine and build metadata are gathered lazily (never at import
	// time) and merged without overriding caller-provided values.
	if !cfg.DisableMachineAttributes {
		for k, v := range machineAttributes(d) {
			if _, exists := qr.attributes[k]; !exists {
				qr.attributes[k] = v
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
		Attachments: append([]string(nil), cfg.AttachmentPaths...),
	}

	if cfg.BeforeSend != nil {
		if modified := c.runBeforeSend(cfg.BeforeSend, report, d); modified == nil {
			d.logf("report %s dropped by BeforeSend", report.UUID)
			return
		} else {
			report = modified
		}
	}

	body, err := json.Marshal(report.toWire())
	if err != nil {
		c.dropped.Add(1)
		d.logf("report %s dropped: marshal failed: %v", report.UUID, err)
		return
	}

	if cfg.Debug {
		pretty, _ := json.MarshalIndent(report.toWire(), "", "  ")
		d.logf("sending report %s to %s\n%s", report.UUID, redactURL(cfg.submissionURL()), pretty)
	}

	if err := c.transport.send(cfg.submissionURL(), body, report.Attachments, d); err != nil {
		c.dropped.Add(1)
		d.logf("report %s dropped: %v", report.UUID, err)
	}
}

// runBeforeSend isolates user hook panics from the worker.
func (c *Client) runBeforeSend(hook func(*ReportData) *ReportData, report *ReportData, d diag) (out *ReportData) {
	defer func() {
		if r := recover(); r != nil {
			d.logf("BeforeSend panicked (%v); sending report unmodified", r)
			out = report
		}
	}()
	return hook(report)
}

// recoverInternal is the last line of defense: an SDK bug must never crash
// the host application.
func (c *Client) recoverInternal(where string) {
	if r := recover(); r != nil {
		buf := make([]byte, 4096)
		n := runtime.Stack(buf, false)
		c.diag().logf("internal error in %s (please report to backtrace-labs/backtrace-go): %v\n%s", where, r, buf[:n])
	}
}

// captureStack returns the formatted stack trace of the calling goroutine,
// or of all goroutines when all is true.
func captureStack(all bool) []byte {
	buf := make([]byte, 1024)
	for {
		n := runtime.Stack(buf, all)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}
