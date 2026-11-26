// Package bthttp provides net/http middleware that reports panics in HTTP
// handlers to Backtrace, and an http.RoundTripper that records outbound
// requests as breadcrumbs.
//
//	handler := bthttp.New(bthttp.Options{})
//	http.ListenAndServe(":8080", handler.Handle(mux))
//
// Reports are sent through bt.DefaultClient(), or through Options.Client
// when set. By default only non-identifying request metadata
// (method, protocol, registered route pattern) is attached; raw URL, host,
// remote address, and user agent are gated behind Options.SendDefaultPII.
//
// Each request runs under its own bt.Scope. Attributes and breadcrumbs that
// a handler adds with bt.ScopeFromContext(r.Context()) go on the
// middleware's reports and on reports made with r.Context().
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
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	bt "github.com/backtrace-labs/backtrace-go"
)

// Attribute length bounds; attacker-controlled request fields are truncated.
const (
	maxMethodLength    = 64
	maxRouteLength     = 2048
	maxURLLength       = 4096
	maxHostLength      = 1024
	maxRemoteLength    = 1024
	maxUserAgentLength = 2048
)

const fingerprintAttribute = "_mod_fingerprint"

// Options configures the middleware.
type Options struct {
	// Repanic re-raises the panic after reporting so outer middleware or
	// the net/http server recovery can run. When false the panic is
	// swallowed and, if the handler had not committed a response, the
	// middleware writes 500 Internal Server Error. The 500 keeps only the
	// headers set before the request reached the middleware. On a committed
	// response the middleware panics with http.ErrAbortHandler instead, and
	// the client sees a broken response.
	//
	// A panic with http.ErrAbortHandler is never reported and is always
	// re-raised.
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
	// global reporter. When nil, bt.DefaultClient() is looked up at each
	// report.
	Client *bt.Client

	// SendDefaultPII includes the raw request path, host, remote address,
	// and user agent with the middleware's reports. Default false: these
	// fields can carry personal data, tenant identifiers, or
	// session-bearing path components.
	SendDefaultPII bool

	// RequestAttributes, when set, contributes application-approved
	// request metadata to the middleware's reports. The returned map is
	// copied by the SDK; a panic inside the callback is contained and
	// ignored.
	RequestAttributes func(*http.Request) map[string]interface{}

	// ReportStatus, when set, is called after a handler returns without a
	// panic, with the status it wrote (200 when it wrote none). When it
	// returns true, the middleware reports the message "HTTP <status>
	// <status text>" with the request attributes and "response.status".
	// The "_mod_fingerprint" attribute groups these reports by status and
	// route. RequestAttributes can override it.
	// ReportStatus is not called for hijacked connections. A panic inside it
	// counts as false.
	ReportStatus func(status int, r *http.Request) bool
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

// Handle wraps next with panic reporting. Each request gets a new bt.Scope,
// a child of any scope its context already carries. Outer middleware still
// sees the Pattern and path values that an http.ServeMux inside next
// matched.
func (h *Handler) Handle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, next)
	})
}

// HandleFunc wraps next with panic reporting, as Handle does.
func (h *Handler) HandleFunc(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, next)
	}
}

func (h *Handler) recoverAndReport(w *trackingWriter, r *http.Request, st *requestState) {
	v := recover()
	if v == nil {
		return
	}
	if v == http.ErrAbortHandler {
		panic(v)
	}

	attrs := h.requestAttributes(r, st)
	if c := h.client(); c != nil {
		if h.opts.WaitForDelivery {
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), h.opts.FlushTimeout)
			c.ReportPanicValueAndFlush(flushCtx, v, attrs)
			cancel()
		} else {
			c.ReportPanicValueContext(r.Context(), v, attrs)
		}
	}

	if h.opts.Repanic {
		panic(v)
	}
	if w.wroteHeader && !w.hijacked {
		panic(http.ErrAbortHandler)
	}
	// Swallowing the panic means net/http's default handling never runs;
	// an uncommitted response would otherwise become an empty 200 OK.
	// Never write after a hijack — the connection no longer belongs to us.
	if !w.wroteHeader && !w.hijacked {
		hdr := w.Header()
		clear(hdr)
		maps.Copy(hdr, w.outerHeader)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

func (h *Handler) reportStatus(w *trackingWriter, r *http.Request, st *requestState) {
	if h.opts.ReportStatus == nil || w.hijacked {
		return
	}
	status := http.StatusOK
	if w.wroteHeader {
		status = w.status
	}
	if !h.safeReportStatus(status, r) {
		return
	}
	c := h.client()
	if c == nil {
		return
	}
	attrs := h.requestAttributes(r, st)
	attrs["response.status"] = status
	if _, set := attrs[fingerprintAttribute]; !set {
		attrs[fingerprintAttribute] = strings.TrimSpace("bthttp status " + strconv.Itoa(status) + " " + requestRoute(r, st))
	}
	message := strings.TrimSpace("HTTP " + strconv.Itoa(status) + " " + http.StatusText(status))
	c.ReportContext(r.Context(), message, attrs)
}

func (h *Handler) safeReportStatus(status int, r *http.Request) bool {
	defer func() { _ = recover() }()
	return h.opts.ReportStatus(status, r)
}

func (h *Handler) requestAttributes(r *http.Request, st *requestState) map[string]any {
	attrs := map[string]interface{}{
		"request.method": boundedString(r.Method, maxMethodLength),
		"request.proto":  r.Proto,
	}
	if route := requestRoute(r, st); route != "" {
		attrs["request.route"] = route
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
	return attrs
}

func requestRoute(r *http.Request, st *requestState) string {
	route := r.Pattern
	if route == "" {
		if p := st.route.Load(); p != nil {
			route = *p
		}
	}
	return boundedString(route, maxRouteLength)
}

// SetRoute sets the "request.route" attribute of the middleware's reports
// for the request that ctx belongs to. Use it in routers that serve a copy
// of the request, such as one made with r.WithContext. The Pattern set on
// such a copy does not reach the middleware. A Pattern the middleware sees
// still wins. SetRoute does nothing outside a request served by a Handler.
// The route is capped at 2048 bytes.
func SetRoute(ctx context.Context, pattern string) {
	if ctx == nil {
		return
	}
	if st, ok := ctx.Value(requestStateKey{}).(*requestState); ok {
		st.route.Store(&pattern)
	}
}

type requestStateKey struct{}

type requestState struct {
	route atomic.Pointer[string]
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	st := &requestState{}
	inner := r.WithContext(context.WithValue(bt.WithScope(r.Context()), requestStateKey{}, st))
	defer copyRouteMatch(r, inner)
	tw := &trackingWriter{ResponseWriter: w}
	if hdr := w.Header(); len(hdr) > 0 {
		tw.outerHeader = hdr.Clone()
	}
	defer h.recoverAndReport(tw, inner, st)
	next.ServeHTTP(wrapWriter(tw), inner)
	h.reportStatus(tw, inner, st)
}

func copyRouteMatch(dst, src *http.Request) {
	if src.Pattern == "" || src.Pattern == dst.Pattern {
		return
	}
	dst.Pattern = src.Pattern
	rest := src.Pattern
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			return
		}
		rest = rest[open+1:]
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			return
		}
		if name := strings.TrimSuffix(rest[:end], "..."); name != "$" {
			dst.SetPathValue(name, src.PathValue(name))
		}
		rest = rest[end+1:]
	}
}

func (h *Handler) client() *bt.Client {
	if h.opts.Client != nil {
		return h.opts.Client
	}
	return bt.DefaultClient()
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
		for i := 0; i < utf8.UTFMax && max > 0 && !utf8.RuneStart(s[max]); i++ {
			max--
		}
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
	outerHeader http.Header
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
	w.ResponseWriter.WriteHeader(code)
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		return
	}
	w.wroteHeader = true
	w.status = code
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
