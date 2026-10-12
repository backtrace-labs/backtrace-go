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
// The middleware wraps the http.ResponseWriter (to detect uncommitted
// responses). The wrapper implements Flush, Hijack, Push, ReadFrom, and
// Unwrap; handlers that type-assert the writer to a concrete type should
// use http.NewResponseController or Unwrap instead.
package bthttp

import (
	"bufio"
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
		defer h.recoverAndReport(tw, r)
		next.ServeHTTP(tw, r)
	})
}

// HandleFunc wraps next with panic reporting.
func (h *Handler) HandleFunc(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer h.recoverAndReport(tw, r)
		next(tw, r)
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
	if !w.wroteHeader {
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

// trackingWriter records whether a response was committed so a swallowed
// panic can be converted into a 500 when nothing was written. It preserves
// the optional ResponseWriter interfaces via http.NewResponseController
// (Flush/Hijack) and direct assertions (Push/ReadFrom), and exposes Unwrap
// for the controller.
type trackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
	status      int
}

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

func (w *trackingWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *trackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *trackingWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (w *trackingWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

// CloseNotify delegates to the underlying writer for handlers still using
// the deprecated http.CloseNotifier interface.
//
//nolint:staticcheck // deliberate passthrough of the deprecated interface
func (w *trackingWriter) CloseNotify() <-chan bool {
	if cn, ok := w.ResponseWriter.(http.CloseNotifier); ok {
		return cn.CloseNotify()
	}
	return nil
}
