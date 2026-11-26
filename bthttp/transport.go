package bthttp

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	bt "github.com/backtrace-labs/backtrace-go"
)

const (
	maxBreadcrumbHostLength  = 256
	maxBreadcrumbFieldLength = 1024
)

// TransportOptions configures NewTransport.
type TransportOptions struct {
	// SendDefaultPII adds the request path and raw query to the
	// breadcrumbs, each capped at 1024 bytes. Default false: paths and
	// queries can carry personal data, tokens, or tenant identifiers.
	SendDefaultPII bool
}

// NewTransport returns an http.RoundTripper that sends each request through
// base and records an "http" breadcrumb on the bt.Scope of the request's
// context. Requests without a scope get no breadcrumb. In a handler served
// by a Handler, pass r.Context() to outbound requests. The breadcrumb holds
// the method, host, status or error, and duration.
//
// A nil base stands for the value http.DefaultTransport has when
// NewTransport is called. The result can replace http.DefaultTransport:
//
//	http.DefaultTransport = bthttp.NewTransport(nil, bthttp.TransportOptions{})
func NewTransport(base http.RoundTripper, opts TransportOptions) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, opts: opts}
}

type transport struct {
	base http.RoundTripper
	opts TransportOptions
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	scope := bt.ScopeFromContext(req.Context())
	if scope == nil {
		return t.base.RoundTrip(req)
	}
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	scope.AddBreadcrumb(t.breadcrumb(req, resp, err, time.Since(start)))
	return resp, err
}

func (t *transport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

func (t *transport) breadcrumb(req *http.Request, resp *http.Response, err error, d time.Duration) bt.Breadcrumb {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	method = boundedString(method, maxMethodLength)
	host, path, query := req.Host, "", ""
	if req.URL != nil {
		if req.URL.Host != "" {
			host = req.URL.Host
		}
		path, query = req.URL.Path, req.URL.RawQuery
	}
	host = boundedString(host, maxBreadcrumbHostLength)

	attrs := map[string]any{
		"method":      method,
		"host":        host,
		"duration_ms": d.Milliseconds(),
	}
	target := host
	if t.opts.SendDefaultPII {
		path = boundedString(path, maxBreadcrumbFieldLength)
		attrs["path"] = path
		attrs["query"] = boundedString(query, maxBreadcrumbFieldLength)
		target += path
	}

	b := bt.Breadcrumb{Type: bt.BreadcrumbTypeHTTP, Level: bt.BreadcrumbInfo, Attributes: attrs}
	switch {
	case err != nil:
		text := boundedString(t.errorText(err), maxBreadcrumbFieldLength)
		attrs["error"] = text
		b.Level = bt.BreadcrumbWarning
		b.Message = method + " " + target + " failed: " + text
	case resp != nil:
		attrs["status"] = resp.StatusCode
		b.Message = method + " " + target + " " + strconv.Itoa(resp.StatusCode)
	default:
		b.Message = method + " " + target
	}
	return b
}

func (t *transport) errorText(err error) (text string) {
	defer func() {
		if recover() != nil {
			text = "error text unavailable"
		}
	}()
	var ue *url.Error
	if !t.opts.SendDefaultPII && errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	return err.Error()
}
