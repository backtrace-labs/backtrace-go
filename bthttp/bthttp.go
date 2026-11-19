// Package bthttp provides net/http middleware that reports panics in HTTP
// handlers to Backtrace.
//
//	handler := bthttp.New(bthttp.Options{})
//	http.ListenAndServe(":8080", handler.Handle(mux))
//
// Reports are sent through the client configured via bt.Options, or through
// Options.Client when set. By default only non-identifying request metadata
// (method, protocol, registered route pattern) is attached; raw URL, host,
// remote address, and user agent are gated behind Options.SendDefaultPII.
//
// # The response writer
//
// The wrapped handler receives the middleware's own http.ResponseWriter, so
// that a swallowed panic becomes a 500 only when no response was started,
// and nothing is written to a connection the handler hijacked. That writer
// offers:
//
//   - http.Flusher and http.Hijacker when the writer it wraps supports
//     them, or any writer reachable through that writer's Unwrap method
//     does: the lookup http.ResponseController performs. Flushing or
//     hijacking through an http.ResponseController is tracked as well.
//   - http.Pusher and io.ReaderFrom only when the writer it wraps
//     implements them itself. Reaching past an intermediate writer for
//     these would skip that writer's own Write.
//   - io.StringWriter, and Unwrap for the deadline and full-duplex calls of
//     http.ResponseController.
//
// The deprecated http.CloseNotifier is never offered; use
// Request.Context().Done(). A handler that type-asserts the writer to a
// concrete type should use http.NewResponseController instead. Writing to
// the result of Unwrap goes around the tracking described above.
package bthttp

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
)

// Attribute length bounds; attacker-controlled request fields are truncated.
const (
	maxRouteLength     = 2048
	maxURLLength       = 4096
	maxHostLength      = 1024
	maxRemoteLength    = 1024
	maxUserAgentLength = 2048
)

// Options configures the middleware.
type Options struct {
	// Repanic re-raises the panic after reporting so outer middleware or
	// the net/http server recovery can run. When false the panic is
	// swallowed and, if the handler had not committed a response, the
	// middleware writes 500 Internal Server Error.
	Repanic bool

	// WaitForDelivery blocks the failing request until the report is
	// delivered, bounded by FlushTimeout, instead of returning
	// immediately. Recommended when Repanic is true and the process may
	// terminate.
	WaitForDelivery bool

	// FlushTimeout bounds WaitForDelivery (one deadline covering both
	// capture and delivery). Default: 2s.
	FlushTimeout time.Duration

	// Client sends reports through a specific bt.Client instead of the
	// global reporter.
	Client *bt.Client

	// SendDefaultPII includes the raw request path, host, remote address,
	// and user agent with panic reports. Default false: these fields can
	// carry personal data, tenant identifiers, or session-bearing path
	// components.
	SendDefaultPII bool

	// RequestAttributes, when set, contributes application-approved
	// request metadata to panic reports. The returned map is copied by
	// the SDK; a panic inside the callback is contained and ignored.
	RequestAttributes func(*http.Request) map[string]interface{}
}

// Handler wraps HTTP handlers with panic reporting.
type Handler struct {
	opts Options
}

// New creates a middleware Handler with the given options.
func New(opts Options) *Handler {
	if opts.FlushTimeout <= 0 {
		opts.FlushTimeout = 2 * time.Second
	}
	return &Handler{opts: opts}
}

// Handle wraps next with panic reporting.
func (h *Handler) Handle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		wrapped := wrapWriter(tw)
		defer h.recoverAndReport(tw, r)
		next.ServeHTTP(wrapped, r)
	})
}

// HandleFunc wraps next with panic reporting.
func (h *Handler) HandleFunc(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		wrapped := wrapWriter(tw)
		defer h.recoverAndReport(tw, r)
		next(wrapped, r)
	}
}

func (h *Handler) recoverAndReport(w *trackingWriter, r *http.Request) {
	v := recover()
	if v == nil {
		return
	}

	attrs := map[string]interface{}{
		"request.method": r.Method,
		"request.proto":  r.Proto,
	}
	if r.Pattern != "" {
		attrs["request.route"] = boundedString(r.Pattern, maxRouteLength)
	}
	if h.opts.SendDefaultPII {
		attrs["request.url"] = boundedString(r.URL.Path, maxURLLength)
		attrs["request.host"] = boundedString(r.Host, maxHostLength)
		attrs["request.remote_addr"] = boundedString(r.RemoteAddr, maxRemoteLength)
		attrs["request.user_agent"] = boundedString(r.UserAgent(), maxUserAgentLength)
	}
	for k, value := range h.safeRequestAttributes(r) {
		attrs[k] = value
	}

	if h.opts.Client != nil {
		if h.opts.WaitForDelivery {
			h.opts.Client.ReportPanicValueAndFlush(v, attrs, h.opts.FlushTimeout)
		} else {
			h.opts.Client.ReportPanicValue(v, attrs)
		}
	} else if h.opts.WaitForDelivery {
		bt.ReportPanicValueAndFlush(v, attrs, h.opts.FlushTimeout)
	} else {
		bt.ReportPanicValue(v, attrs)
	}

	if h.opts.Repanic {
		panic(v)
	}
	// Swallowing the panic means net/http's default handling never runs;
	// an uncommitted response would otherwise become an empty 200 OK.
	// Never write after a hijack — the connection no longer belongs to us.
	if !w.wroteHeader && !w.hijacked {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

// safeRequestAttributes runs the user callback behind panic containment.
func (h *Handler) safeRequestAttributes(r *http.Request) (out map[string]interface{}) {
	if h.opts.RequestAttributes == nil {
		return nil
	}
	defer func() { _ = recover() }()
	produced := h.opts.RequestAttributes(r)
	if produced == nil {
		return nil
	}
	out = make(map[string]interface{}, len(produced))
	for k, v := range produced {
		out[k] = v
	}
	return out
}

func boundedString(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// trackingWriter records whether a response was started (so a swallowed
// panic becomes a 500 only when nothing was sent) and whether the
// connection was hijacked. The optional http.ResponseWriter interfaces are
// added by wrapWriter only where they are really supported, so a handler's
// `w.(http.Flusher)` assertion tells the truth.
//
// The deprecated http.CloseNotifier is intentionally not propagated (it is
// superseded by Request.Context().Done()); returning a nil channel from a
// falsely-advertised CloseNotify would block a handler forever.
type trackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
	status      int
	hijacked    bool
}

// Unwrap lets http.ResponseController reach the wrapped writer for the
// operations the wrapper does not track (deadlines, full duplex).
func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackingWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// WriteString keeps io.WriteString on the wrapped writer's own fast path
// when it has one; otherwise it is the Write above.
func (w *trackingWriter) WriteString(s string) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return io.WriteString(w.ResponseWriter, s)
}

// The optional-method implementations below live on trackingWriter but are
// surfaced only through the combination types selected in wrapWriter, so
// they never widen the advertised interface set on their own.

// flushError flushes through http.ResponseController, which finds the
// flusher wherever it sits behind Unwrap. An attempted flush starts the
// response (an implicit 200) whether or not the write succeeds, so only
// "not supported" leaves it unstarted.
func (w *trackingWriter) flushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if !w.wroteHeader && !errors.Is(err, http.ErrNotSupported) {
		w.wroteHeader = true
		w.status = http.StatusOK
	}
	return err
}

// hijack takes the connection over through http.ResponseController, for
// the same reason.
func (w *trackingWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}

func (w *trackingWriter) push(target string, opts *http.PushOptions) error {
	return w.ResponseWriter.(http.Pusher).Push(target, opts)
}

func (w *trackingWriter) readFrom(r io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.(io.ReaderFrom).ReadFrom(r)
}

// The 2^4 combination types below give the returned wrapper exactly the
// optional-interface method set of the underlying writer. This is verbose
// but is the standard, dependency-free way to preserve interface parity
// (the same shape httpsnoop/gorilla generate).
type (
	twF    struct{ *trackingWriter }
	twH    struct{ *trackingWriter }
	twP    struct{ *trackingWriter }
	twR    struct{ *trackingWriter }
	twFH   struct{ *trackingWriter }
	twFP   struct{ *trackingWriter }
	twFR   struct{ *trackingWriter }
	twHP   struct{ *trackingWriter }
	twHR   struct{ *trackingWriter }
	twPR   struct{ *trackingWriter }
	twFHP  struct{ *trackingWriter }
	twFHR  struct{ *trackingWriter }
	twFPR  struct{ *trackingWriter }
	twHPR  struct{ *trackingWriter }
	twFHPR struct{ *trackingWriter }
)

func (w twF) Flush()    { _ = w.flushError() }
func (w twFH) Flush()   { _ = w.flushError() }
func (w twFP) Flush()   { _ = w.flushError() }
func (w twFR) Flush()   { _ = w.flushError() }
func (w twFHP) Flush()  { _ = w.flushError() }
func (w twFHR) Flush()  { _ = w.flushError() }
func (w twFPR) Flush()  { _ = w.flushError() }
func (w twFHPR) Flush() { _ = w.flushError() }

// FlushError is the method http.ResponseController looks for first. With
// it a flush made through a controller is tracked, and its error returned.
func (w twF) FlushError() error    { return w.flushError() }
func (w twFH) FlushError() error   { return w.flushError() }
func (w twFP) FlushError() error   { return w.flushError() }
func (w twFR) FlushError() error   { return w.flushError() }
func (w twFHP) FlushError() error  { return w.flushError() }
func (w twFHR) FlushError() error  { return w.flushError() }
func (w twFPR) FlushError() error  { return w.flushError() }
func (w twFHPR) FlushError() error { return w.flushError() }

func (w twH) Hijack() (net.Conn, *bufio.ReadWriter, error)    { return w.hijack() }
func (w twFH) Hijack() (net.Conn, *bufio.ReadWriter, error)   { return w.hijack() }
func (w twHP) Hijack() (net.Conn, *bufio.ReadWriter, error)   { return w.hijack() }
func (w twHR) Hijack() (net.Conn, *bufio.ReadWriter, error)   { return w.hijack() }
func (w twFHP) Hijack() (net.Conn, *bufio.ReadWriter, error)  { return w.hijack() }
func (w twFHR) Hijack() (net.Conn, *bufio.ReadWriter, error)  { return w.hijack() }
func (w twHPR) Hijack() (net.Conn, *bufio.ReadWriter, error)  { return w.hijack() }
func (w twFHPR) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.hijack() }

func (w twP) Push(t string, o *http.PushOptions) error    { return w.push(t, o) }
func (w twFP) Push(t string, o *http.PushOptions) error   { return w.push(t, o) }
func (w twHP) Push(t string, o *http.PushOptions) error   { return w.push(t, o) }
func (w twPR) Push(t string, o *http.PushOptions) error   { return w.push(t, o) }
func (w twFHP) Push(t string, o *http.PushOptions) error  { return w.push(t, o) }
func (w twFPR) Push(t string, o *http.PushOptions) error  { return w.push(t, o) }
func (w twHPR) Push(t string, o *http.PushOptions) error  { return w.push(t, o) }
func (w twFHPR) Push(t string, o *http.PushOptions) error { return w.push(t, o) }

func (w twR) ReadFrom(r io.Reader) (int64, error)    { return w.readFrom(r) }
func (w twFR) ReadFrom(r io.Reader) (int64, error)   { return w.readFrom(r) }
func (w twHR) ReadFrom(r io.Reader) (int64, error)   { return w.readFrom(r) }
func (w twPR) ReadFrom(r io.Reader) (int64, error)   { return w.readFrom(r) }
func (w twFHR) ReadFrom(r io.Reader) (int64, error)  { return w.readFrom(r) }
func (w twFPR) ReadFrom(r io.Reader) (int64, error)  { return w.readFrom(r) }
func (w twHPR) ReadFrom(r io.Reader) (int64, error)  { return w.readFrom(r) }
func (w twFHPR) ReadFrom(r io.Reader) (int64, error) { return w.readFrom(r) }

// maxUnwrapDepth bounds the capability lookup, so that a writer whose
// Unwrap returns itself cannot stall every request.
const maxUnwrapDepth = 32

// supportsFlush and supportsHijack follow Unwrap exactly as
// http.ResponseController does, so the wrapper offers what a controller
// could reach on the wrapped writer.
func supportsFlush(rw http.ResponseWriter) bool {
	for i := 0; i < maxUnwrapDepth; i++ {
		switch t := rw.(type) {
		case interface{ FlushError() error }, http.Flusher:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			rw = t.Unwrap()
		default:
			return false
		}
	}
	return false
}

func supportsHijack(rw http.ResponseWriter) bool {
	for i := 0; i < maxUnwrapDepth; i++ {
		switch t := rw.(type) {
		case http.Hijacker:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			rw = t.Unwrap()
		default:
			return false
		}
	}
	return false
}

// wrapWriter returns an http.ResponseWriter with exactly the optional
// methods that work on the wrapped writer: Flusher and Hijacker anywhere
// behind Unwrap, Pusher and ReaderFrom on the writer itself.
func wrapWriter(tw *trackingWriter) http.ResponseWriter {
	var mask int
	if supportsFlush(tw.ResponseWriter) {
		mask |= 1
	}
	if supportsHijack(tw.ResponseWriter) {
		mask |= 2
	}
	if _, ok := tw.ResponseWriter.(http.Pusher); ok {
		mask |= 4
	}
	if _, ok := tw.ResponseWriter.(io.ReaderFrom); ok {
		mask |= 8
	}
	switch mask {
	case 0:
		return tw
	case 1:
		return twF{tw}
	case 2:
		return twH{tw}
	case 4:
		return twP{tw}
	case 8:
		return twR{tw}
	case 1 | 2:
		return twFH{tw}
	case 1 | 4:
		return twFP{tw}
	case 1 | 8:
		return twFR{tw}
	case 2 | 4:
		return twHP{tw}
	case 2 | 8:
		return twHR{tw}
	case 4 | 8:
		return twPR{tw}
	case 1 | 2 | 4:
		return twFHP{tw}
	case 1 | 2 | 8:
		return twFHR{tw}
	case 1 | 4 | 8:
		return twFPR{tw}
	case 2 | 4 | 8:
		return twHPR{tw}
	default: // 1|2|4|8
		return twFHPR{tw}
	}
}
