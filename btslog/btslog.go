// Package btslog sends log/slog records to Backtrace.
//
//	h := btslog.NewHandler(slog.NewJSONHandler(os.Stderr, nil), btslog.Options{Client: client})
//	logger := slog.New(h)
//	logger.InfoContext(ctx, "charging card", "order", id)
//	logger.ErrorContext(ctx, "charge failed", "err", err, "order", id)
//
// The first call records a "log" breadcrumb on the bt.Scope that ctx
// carries. The second reports err with the attributes order=<id> and
// log.message="charge failed", plus the scope's data and that breadcrumb.
package btslog

import (
	"context"
	"log/slog"
	"math"

	bt "github.com/backtrace-labs/backtrace-go"
	"github.com/backtrace-labs/backtrace-go/internal/logbridge"
)

// Options configures NewHandler.
type Options struct {
	// Client receives the reports and breadcrumbs. When nil, the handler
	// uses bt.DefaultClient() at the time of each record.
	Client *bt.Client

	// ReportLevel is the lowest level that is reported. Default:
	// slog.LevelError. A *slog.LevelVar can change it at run time.
	ReportLevel slog.Leveler

	// BreadcrumbLevel is the lowest level that becomes a breadcrumb.
	// Records at or above ReportLevel are reported instead. Default:
	// slog.LevelInfo.
	BreadcrumbLevel slog.Leveler
}

// NewHandler returns a slog.Handler that passes each record to next, when
// next is enabled for its level, and also sends it to Backtrace. A nil next
// discards the records.
//
// A record at or above ReportLevel is reported with its context and carries
// the data of the context's bt.Scope. The first error-valued attribute,
// with the keys "err" and "error" preferred, is the reported error, and the
// message goes in the "log.message" attribute. Without one, the message is
// reported. The other attributes become report attributes keyed by group
// path, such as "request.method".
//
// A record below ReportLevel and at or above BreadcrumbLevel becomes a "log"
// breadcrumb on the context's scope, or on the client when the context
// carries none.
func NewHandler(next slog.Handler, opts Options) slog.Handler {
	h := &handler{
		next:        next,
		client:      opts.Client,
		reportLevel: opts.ReportLevel,
		crumbLevel:  opts.BreadcrumbLevel,
	}
	if h.reportLevel == nil {
		h.reportLevel = slog.LevelError
	}
	if h.crumbLevel == nil {
		h.crumbLevel = slog.LevelInfo
	}
	return h
}

type handler struct {
	next        slog.Handler
	client      *bt.Client
	reportLevel slog.Leveler
	crumbLevel  slog.Leveler
	prefix      string
	fields      []field
}

type field struct {
	key     string
	name    string
	value   slog.Value
	err     error
	errOnly bool
}

const (
	disabledLevel = slog.Level(math.MaxInt)
	maxGroupDepth = 32
)

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= min(levelOf(h.reportLevel), levelOf(h.crumbLevel)) {
		return true
	}
	return h.next != nil && h.next.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	h.send(ctx, r)
	if h.next != nil && h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := *h
	if h.next != nil {
		h2.next = h.next.WithAttrs(attrs)
	}
	fields := make([]field, len(h.fields), len(h.fields)+len(attrs))
	copy(fields, h.fields)
	for _, a := range attrs {
		fields = appendFields(fields, h.prefix, a, 0)
	}
	h2.fields = fields
	return &h2
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	if h.next != nil {
		h2.next = h.next.WithGroup(name)
	}
	h2.prefix = h.prefix + name + "."
	return &h2
}

func (h *handler) send(ctx context.Context, r slog.Record) {
	defer func() { _ = recover() }()
	reported := r.Level >= levelOf(h.reportLevel)
	if !reported && r.Level < levelOf(h.crumbLevel) {
		return
	}
	if logbridge.InDiagnostic() {
		return
	}
	logbridge.HandleRecord(func() {
		fields := make([]field, len(h.fields), len(h.fields)+r.NumAttrs())
		copy(fields, h.fields)
		r.Attrs(func(a slog.Attr) bool {
			fields = appendFields(fields, h.prefix, a, 0)
			return true
		})
		if reported {
			h.sendReport(ctx, r, fields)
		} else {
			h.addBreadcrumb(ctx, r, fields)
		}
	})
}

func (h *handler) sendReport(ctx context.Context, r slog.Record, fields []field) {
	c := h.clientFor()
	if c == nil {
		return
	}
	errIndex := reportedError(fields)
	attrs := make(map[string]any, len(fields)+1)
	for i, f := range fields {
		if i != errIndex && !f.errOnly {
			attrs[f.key] = attributeValue(f.value)
		}
	}
	var object any = r.Message
	if errIndex >= 0 {
		object = fields[errIndex].err
		attrs["log.message"] = r.Message
	}
	ctx = logbridge.WithCallSite(ctx, r.PC)
	c.ReportContext(ctx, object, attrs)
}

func (h *handler) addBreadcrumb(ctx context.Context, r slog.Record, fields []field) {
	b := bt.Breadcrumb{
		Level:   breadcrumbLevel(r.Level),
		Type:    bt.BreadcrumbTypeLog,
		Message: r.Message,
	}
	if !r.Time.IsZero() {
		b.Timestamp = r.Time.UnixMilli()
	}
	if len(fields) > 0 {
		b.Attributes = make(map[string]any, len(fields))
		for _, f := range fields {
			if !f.errOnly {
				b.Attributes[f.key] = attributeValue(f.value)
			}
		}
	}
	if s := bt.ScopeFromContext(ctx); s != nil {
		s.AddBreadcrumb(b)
		return
	}
	h.clientFor().AddBreadcrumb(b)
}

func (h *handler) clientFor() *bt.Client {
	if h.client != nil {
		return h.client
	}
	return bt.DefaultClient()
}

func appendFields(dst []field, prefix string, a slog.Attr, depth int) []field {
	v := a.Value.Resolve()
	if v.Kind() != slog.KindGroup {
		if a.Key == "" {
			return dst
		}
		return append(dst, field{key: prefix + a.Key, name: a.Key, value: v, err: errorOf(a.Value, v)})
	}
	if depth >= maxGroupDepth {
		return dst
	}
	if err := errorOf(a.Value, v); err != nil && a.Key != "" {
		dst = append(dst, field{key: prefix + a.Key, name: a.Key, err: err, errOnly: true})
	}
	if a.Key != "" {
		prefix += a.Key + "."
	}
	for _, g := range v.Group() {
		dst = appendFields(dst, prefix, g, depth+1)
	}
	return dst
}

func attributeValue(v slog.Value) any {
	if v.Kind() == slog.KindDuration {
		return v.Duration().String()
	}
	return v.Any()
}

func errorOf(raw, resolved slog.Value) error {
	if raw.Kind() == slog.KindLogValuer {
		if err, ok := raw.Any().(error); ok {
			return err
		}
	}
	if resolved.Kind() == slog.KindAny {
		err, _ := resolved.Any().(error)
		return err
	}
	return nil
}

func reportedError(fields []field) int {
	first := -1
	for i, f := range fields {
		if f.err == nil {
			continue
		}
		if f.name == "err" || f.name == "error" {
			return i
		}
		if first < 0 {
			first = i
		}
	}
	return first
}

func breadcrumbLevel(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return bt.BreadcrumbDebug
	case l < slog.LevelWarn:
		return bt.BreadcrumbInfo
	case l < slog.LevelError:
		return bt.BreadcrumbWarning
	default:
		return bt.BreadcrumbError
	}
}

func levelOf(l slog.Leveler) (level slog.Level) {
	defer func() {
		if recover() != nil {
			level = disabledLevel
		}
	}()
	return l.Level()
}
