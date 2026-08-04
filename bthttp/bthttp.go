// Package bthttp provides net/http middleware that reports panics in HTTP
// handlers to Backtrace, enriched with request attributes.
//
//	handler := bthttp.New(bthttp.Options{Repanic: true}).Handle(mux)
//	http.ListenAndServe(":8080", handler)
//
// Reports are sent through the client configured via bt.Options, or through
// Options.Client when set.
package bthttp

import (
	"net/http"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
)

// Options configures the middleware.
type Options struct {
	// Repanic re-raises the panic after reporting so outer middleware or
	// the net/http server recovery can run. When false the panic is
	// swallowed and the connection is left to net/http's default
	// handling of an aborted handler.
	Repanic bool

	// WaitForDelivery blocks the failing request until the report is
	// delivered (bounded by FlushTimeout) instead of returning
	// immediately. Recommended when Repanic is true and the process may
	// terminate.
	WaitForDelivery bool

	// FlushTimeout bounds WaitForDelivery. Default: 2s.
	FlushTimeout time.Duration

	// Client sends reports through a specific bt.Client instead of the
	// global reporter.
	Client *bt.Client
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
		defer h.recoverAndReport(r)
		next.ServeHTTP(w, r)
	})
}

// HandleFunc wraps next with panic reporting.
func (h *Handler) HandleFunc(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer h.recoverAndReport(r)
		next(w, r)
	}
}

func (h *Handler) recoverAndReport(r *http.Request) {
	v := recover()
	if v == nil {
		return
	}

	attrs := map[string]interface{}{
		"request.url":         r.URL.Path,
		"request.method":      r.Method,
		"request.host":        r.Host,
		"request.remote_addr": r.RemoteAddr,
		"request.user_agent":  r.UserAgent(),
		"request.proto":       r.Proto,
	}

	if h.opts.Client != nil {
		h.opts.Client.ReportPanicValue(v, attrs)
		if h.opts.WaitForDelivery {
			h.opts.Client.Flush(h.opts.FlushTimeout)
		}
	} else {
		bt.ReportPanicValue(v, attrs)
		if h.opts.WaitForDelivery {
			bt.Flush(h.opts.FlushTimeout)
		}
	}

	if h.opts.Repanic {
		panic(v)
	}
}
