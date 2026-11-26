package bt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func breadcrumbsOf(t *testing.T, atts map[string]string) []map[string]any {
	t.Helper()
	raw, ok := atts["attachment_bt-breadcrumbs-0"]
	if !ok {
		return nil
	}
	var crumbs []map[string]any
	if err := json.Unmarshal([]byte(raw), &crumbs); err != nil {
		t.Fatalf("breadcrumb part is not a JSON array: %v", err)
	}
	return crumbs
}

func crumbMessages(crumbs []map[string]any) []string {
	out := make([]string, 0, len(crumbs))
	for _, b := range crumbs {
		msg, _ := b["message"].(string)
		out = append(out, msg)
	}
	return out
}

func payloadFor(t *testing.T, rs *recordingServer, msg string) (map[string]any, map[string]string) {
	t.Helper()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for i, p := range rs.payloads {
		if attrsOrNil(p)["error.message"] == msg {
			return p, rs.attachments[i]
		}
	}
	t.Fatalf("no report with error.message %q", msg)
	return nil, nil
}

func TestScopeAttributePrecedence(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.Attributes = map[string]any{
			"k.call": "config", "k.inner": "config", "k.outer": "config", "k.client": "config", "k.config": "config",
		}
	})
	c.SetAttributes(map[string]any{"k.call": "client", "k.inner": "client", "k.outer": "client", "k.client": "client"})

	outer := WithScope(context.Background())
	ScopeFromContext(outer).SetAttributes(map[string]any{
		"k.call": "outer", "k.inner": "outer", "k.outer": "outer", "removed": "outer",
	})
	inner := WithScope(outer)
	ScopeFromContext(inner).SetAttribute("k.call", "inner")
	ScopeFromContext(inner).SetAttribute("k.inner", "inner")
	ScopeFromContext(inner).SetAttribute("removed", nil)
	ScopeFromContext(inner).SetAttribute("hostname", nil)
	ScopeFromContext(inner).SetAttribute("error.message", "from scope")

	c.ReportContext(inner, errors.New("inner report"), map[string]any{"k.call": "call"})
	c.ReportContext(outer, errors.New("outer report"), nil)
	c.Report(errors.New("plain report"), nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	want := map[string]map[string]any{
		"inner report": {"k.call": "call", "k.inner": "inner", "k.outer": "outer", "k.client": "client", "k.config": "config"},
		"outer report": {"k.call": "outer", "k.inner": "outer", "k.outer": "outer", "k.client": "client", "k.config": "config", "removed": "outer"},
		"plain report": {"k.call": "client", "k.inner": "client", "k.outer": "client", "k.client": "client", "k.config": "config"},
	}
	for msg, wantAttrs := range want {
		p, _ := payloadFor(t, rs, msg)
		attrs := attrsOf(t, p)
		for k, v := range wantAttrs {
			if attrs[k] != v {
				t.Errorf("%s: %s = %v, want %v", msg, k, attrs[k], v)
			}
		}
		_, hasHost := attrs["hostname"]
		_, hasRemoved := attrs["removed"]
		if msg == "inner report" && (hasHost || hasRemoved) {
			t.Errorf("inner report: nil scope values kept: hostname=%v removed=%v", attrs["hostname"], attrs["removed"])
		}
		if msg != "inner report" && !hasHost {
			t.Errorf("%s: hostname missing; a child's nil reached its parent", msg)
		}
	}
}

func TestScopesIsolateConcurrentWork(t *testing.T) {
	c, rs := newTestClient(t, nil)
	const n = 8
	var ready, wg sync.WaitGroup
	ready.Add(n)
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ctx := WithScope(context.Background())
			s := ScopeFromContext(ctx)
			s.SetAttribute("user.id", i)
			s.AddBreadcrumb(Breadcrumb{Message: fmt.Sprintf("step %d", i)})
			ready.Done()
			<-start
			c.ReportContext(ctx, fmt.Sprintf("request %d", i), nil)
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if rs.count() != n {
		t.Fatalf("got %d reports, want %d", rs.count(), n)
	}
	for i := 0; i < n; i++ {
		p, atts := payloadFor(t, rs, fmt.Sprintf("request %d", i))
		if got := attrsOf(t, p)["user.id"]; got != float64(i) {
			t.Errorf("request %d: user.id = %v", i, got)
		}
		if got := crumbMessages(breadcrumbsOf(t, atts)); !slices.Equal(got, []string{fmt.Sprintf("step %d", i)}) {
			t.Errorf("request %d: breadcrumbs = %q", i, got)
		}
	}
}

func TestScopeBreadcrumbsMergedByTimestamp(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) { cfg.MaxBreadcrumbs = 4 })
	outer := WithScope(context.Background())
	inner := WithScope(outer)
	add := func(ts int64, msg string) {
		b := Breadcrumb{Timestamp: ts, Message: msg}
		switch {
		case strings.HasPrefix(msg, "client"):
			c.AddBreadcrumb(b)
		case strings.HasPrefix(msg, "outer"):
			ScopeFromContext(outer).AddBreadcrumb(b)
		default:
			ScopeFromContext(inner).AddBreadcrumb(b)
		}
	}
	add(60, "inner-60")
	add(40, "client-40")
	add(50, "outer-50")
	add(20, "outer-20")
	add(10, "client-10")
	add(30, "inner-30")

	c.ReportContext(inner, "inner report", nil)
	c.ReportContext(outer, "outer report", nil)
	c.Report("plain report", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	want := map[string][]string{
		"inner report": {"inner-30", "client-40", "outer-50", "inner-60"},
		"outer report": {"client-10", "outer-20", "client-40", "outer-50"},
		"plain report": {"client-10", "client-40"},
	}
	for msg, wantCrumbs := range want {
		p, atts := payloadFor(t, rs, msg)
		crumbs := breadcrumbsOf(t, atts)
		if got := crumbMessages(crumbs); !slices.Equal(got, wantCrumbs) {
			t.Errorf("%s: breadcrumbs = %q, want %q", msg, got, wantCrumbs)
			continue
		}
		var highest float64
		for _, b := range crumbs {
			highest = max(highest, b["id"].(float64))
		}
		if got := attrsOf(t, p)["breadcrumbs.lastId"]; got != highest {
			t.Errorf("%s: breadcrumbs.lastId = %v, want the highest id %v", msg, got, highest)
		}
	}
}

func TestScopeKeepsNewestBreadcrumbs(t *testing.T) {
	c, rs := newTestClient(t, nil)
	ctx := WithScope(context.Background())
	for i := 0; i < 40; i++ {
		ScopeFromContext(ctx).AddBreadcrumb(Breadcrumb{Message: fmt.Sprintf("crumb %d", i)})
	}
	c.ReportContext(ctx, "many crumbs", nil)
	crumbs := breadcrumbsOf(t, flushAttachments(t, c, rs))
	if len(crumbs) != 32 || crumbs[0]["message"] != "crumb 8" {
		t.Fatalf("got %d breadcrumbs starting at %v, want the newest 32 starting at crumb 8", len(crumbs), crumbs[0]["message"])
	}
}

func flushAttachments(t *testing.T, c *Client, rs *recordingServer) map[string]string {
	t.Helper()
	flushOne(t, c, rs)
	return rs.lastAttachments()
}

func TestBreadcrumbIDsFollowAddOrderAcrossClientAndScopes(t *testing.T) {
	c, rs := newTestClient(t, nil)
	ctx := WithScope(context.Background())
	for _, msg := range []string{"client-1", "scope-1", "client-2", "scope-2"} {
		b := Breadcrumb{Timestamp: 1000, Message: msg}
		if strings.HasPrefix(msg, "client") {
			c.AddBreadcrumb(b)
		} else {
			ScopeFromContext(ctx).AddBreadcrumb(b)
		}
	}
	c.ReportContext(ctx, "same timestamp", nil)
	crumbs := breadcrumbsOf(t, flushAttachments(t, c, rs))
	if got := crumbMessages(crumbs); !slices.Equal(got, []string{"client-1", "scope-1", "client-2", "scope-2"}) {
		t.Fatalf("breadcrumbs = %q, want add order for equal timestamps", got)
	}
	for i := 1; i < len(crumbs); i++ {
		if crumbs[i]["id"].(float64) <= crumbs[i-1]["id"].(float64) {
			t.Errorf("ids not increasing: %v then %v", crumbs[i-1]["id"], crumbs[i]["id"])
		}
	}
}

func TestScopeAttachments(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name+" content"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cfgFile, outerFile, innerFile := write("config.log"), write("outer.log"), write("inner.log")

	c, rs := newTestClient(t, func(cfg *Config) { cfg.AttachmentPaths = []string{cfgFile} })
	outer := WithScope(context.Background())
	ScopeFromContext(outer).AddAttachment(outerFile)
	ScopeFromContext(outer).AddAttachment(outerFile)
	inner := WithScope(outer)
	ScopeFromContext(inner).AddAttachment(innerFile)
	ScopeFromContext(inner).AddAttachment(outerFile)
	ScopeFromContext(inner).AddAttachment(cfgFile)
	ScopeFromContext(inner).AddAttachment("")

	c.ReportContext(inner, "inner report", nil)
	c.ReportContext(outer, "outer report", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	want := map[string][]string{
		"inner report": {"attachment_config.log", "attachment_inner.log", "attachment_outer.log"},
		"outer report": {"attachment_config.log", "attachment_outer.log"},
	}
	for msg, wantParts := range want {
		_, atts := payloadFor(t, rs, msg)
		var got []string
		for name, content := range atts {
			got = append(got, name)
			if want := strings.TrimPrefix(name, "attachment_") + " content"; content != want {
				t.Errorf("%s: part %s = %q, want %q", msg, name, content, want)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, wantParts) {
			t.Errorf("%s: parts = %v, want %v", msg, got, wantParts)
		}
	}

	disabled, drs := newTestClient(t, func(cfg *Config) { cfg.MaxAttachments = -1 })
	disabled.ReportContext(inner, "attachments disabled", nil)
	flushOne(t, disabled, drs)
	if atts := drs.lastAttachments(); len(atts) != 0 {
		t.Errorf("MaxAttachments=-1 still uploaded scope attachments: %v", atts)
	}
}

func TestScopeAttachmentCap(t *testing.T) {
	s := ScopeFromContext(WithScope(context.Background()))
	s.AddAttachment("")
	for i := 0; i < 40; i++ {
		s.AddAttachment(fmt.Sprintf("/tmp/file-%d", i))
		s.AddAttachment("/tmp/file-0")
	}
	paths := scopeAttachments([]*Scope{s})
	if len(paths) != 32 || paths[0] != "/tmp/file-0" || paths[31] != "/tmp/file-31" {
		t.Errorf("kept %d paths (%v ... %v), want the first 32", len(paths), paths[0], paths[len(paths)-1])
	}
}

func TestNilScopeAndContextAreSafe(t *testing.T) {
	var s *Scope
	s.SetAttribute("k", "v")
	s.SetAttributes(map[string]any{"k": "v"})
	s.AddBreadcrumb(Breadcrumb{Message: "ignored"})
	s.AddAttachment("/etc/hosts")

	var nilCtx context.Context
	if ScopeFromContext(nilCtx) != nil || ScopeFromContext(context.Background()) != nil {
		t.Error("ScopeFromContext found a scope in a context without one")
	}
	if ScopeFromContext(WithScope(nilCtx)) == nil {
		t.Error("WithScope(nil) carries no scope")
	}
	parent := WithScope(context.Background())
	if child := ScopeFromContext(WithScope(parent)); child == nil || child.parent != ScopeFromContext(parent) {
		t.Error("WithScope did not create a child of the existing scope")
	}

	c, rs := newTestClient(t, nil)
	mustNotPanic(t, "ReportContext(nil)", func() { c.ReportContext(nilCtx, "nil ctx", nil) })
	mustNotPanic(t, "ReportPanicValueContext(nil)", func() { c.ReportPanicValueContext(nilCtx, "nil ctx panic", nil) })
	mustNotPanic(t, "ReportPanicValueAndFlush(nil)", func() {
		if !c.ReportPanicValueAndFlush(nilCtx, "nil ctx flush", nil) {
			t.Error("ReportPanicValueAndFlush(nil ctx) = false")
		}
	})
	c.ReportContext(context.Background(), nil, nil)
	if !c.Flush(5*time.Second) || rs.count() != 3 {
		t.Errorf("got %d reports, want 3 (stats %+v)", rs.count(), c.Stats())
	}
}

type panickyValueContext struct{ context.Context }

func (panickyValueContext) Value(any) any { panic("Value panicked") }

func TestPanickingContextIsContained(t *testing.T) {
	c, rs := newTestClient(t, nil)
	ctx := panickyValueContext{context.Background()}
	mustNotPanic(t, "ReportContext", func() { c.ReportContext(ctx, "x", nil) })
	mustNotPanic(t, "ReportPanicValueContext", func() { c.ReportPanicValueContext(ctx, "x", nil) })
	mustNotPanic(t, "ReportPanicValueAndFlush", func() {
		if c.ReportPanicValueAndFlush(ctx, "x", nil) {
			t.Error("ReportPanicValueAndFlush = true for a report that could not be captured")
		}
	})
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelable := panickyValueContext{parent}
	mustNotPanic(t, "ReportPanicValueAndFlush with a cancelable context", func() {
		if c.ReportPanicValueAndFlush(cancelable, "x", nil) {
			t.Error("ReportPanicValueAndFlush = true for a report that could not be captured")
		}
	})
	c.Flush(5 * time.Second)
	if got := c.Stats(); got.Internal != 4 || got.Accepted != 0 || rs.count() != 0 {
		t.Errorf("Internal = %d, Accepted = %d, sent %d; want 4, 0 and 0", got.Internal, got.Accepted, rs.count())
	}
}

func TestScopeIsReadAtCapture(t *testing.T) {
	c, rs := newTestClient(t, nil)
	release := holdMessages(t, rs, "hold")
	c.ReportMessage("hold", nil)
	entered(t, rs)

	ctx := WithScope(context.Background())
	s := ScopeFromContext(ctx)
	s.SetAttribute("state", "before")
	s.AddBreadcrumb(Breadcrumb{Message: "before"})
	c.ReportContext(ctx, "scoped", nil)
	s.SetAttribute("state", "after")
	s.AddBreadcrumb(Breadcrumb{Message: "after"})
	s.AddAttachment("/etc/hosts")

	release["hold"]()
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	p, atts := payloadFor(t, rs, "scoped")
	if got := attrsOf(t, p)["state"]; got != "before" {
		t.Errorf("state = %v, want the value at capture", got)
	}
	if got := crumbMessages(breadcrumbsOf(t, atts)); !slices.Equal(got, []string{"before"}) {
		t.Errorf("breadcrumbs = %q, want only the one recorded before capture", got)
	}
	if _, ok := atts["attachment_hosts"]; ok {
		t.Error("attachment added after capture was uploaded")
	}
}

func TestScopeAttachmentReadAtSend(t *testing.T) {
	dir := t.TempDir()
	kept, removed := filepath.Join(dir, "kept.log"), filepath.Join(dir, "removed.json")
	for _, path := range []string{kept, removed} {
		if err := os.WriteFile(path, []byte("at capture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, rs := newTestClient(t, nil)
	release := holdMessages(t, rs, "hold")
	c.ReportMessage("hold", nil)
	entered(t, rs)

	ctx := WithScope(context.Background())
	ScopeFromContext(ctx).AddAttachment(kept)
	ScopeFromContext(ctx).AddAttachment(removed)
	c.ReportContext(ctx, "request failed", nil)
	if err := os.WriteFile(kept, []byte("at send"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}

	release["hold"]()
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	_, atts := payloadFor(t, rs, "request failed")
	if got := atts["attachment_kept.log"]; got != "at send" {
		t.Errorf("kept.log part = %q, want the content at send time", got)
	}
	if _, ok := atts["attachment_removed.json"]; ok {
		t.Error("a file removed before the send was uploaded")
	}
}

func TestNegativeMaxBreadcrumbsDisablesScopeBreadcrumbs(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) { cfg.MaxBreadcrumbs = -1 })
	ctx := WithScope(context.Background())
	c.AddBreadcrumb(Breadcrumb{Message: "client"})
	ScopeFromContext(ctx).AddBreadcrumb(Breadcrumb{Message: "scope"})
	c.ReportContext(ctx, "no breadcrumbs", nil)
	attrs := attrsOf(t, flushOne(t, c, rs))
	if _, ok := rs.lastAttachments()["attachment_bt-breadcrumbs-0"]; ok {
		t.Error("breadcrumb part sent with MaxBreadcrumbs = -1")
	}
	if id, ok := attrs["breadcrumbs.lastId"]; ok {
		t.Errorf("breadcrumbs.lastId = %v with MaxBreadcrumbs = -1", id)
	}
	if n := c.Stats().Internal; n != 0 {
		t.Errorf("Internal = %d, want 0", n)
	}
}

func TestScopeSharedAcrossGoroutines(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) { cfg.QueueSize = 256 })
	parent := WithScope(context.Background())
	s := ScopeFromContext(parent)
	missing := filepath.Join(t.TempDir(), "missing")
	const writers, reporters, rounds = 4, 4, 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				s.SetAttribute("writer", w)
				s.SetAttributes(map[string]any{"round": i})
				s.AddBreadcrumb(Breadcrumb{Message: fmt.Sprintf("writer %d round %d", w, i)})
				s.AddAttachment(fmt.Sprintf("%s-%d-%d.log", missing, w, i))
			}
		}()
	}
	for r := 0; r < reporters; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				child := WithScope(parent)
				ScopeFromContext(child).SetAttribute("reporter", r)
				c.ReportContext(parent, "through the parent", nil)
				c.ReportPanicValueContext(child, "through a child", nil)
			}
		}()
	}
	close(start)
	wg.Wait()
	if !c.Flush(10 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if want := 2 * reporters * rounds; rs.count() != want {
		t.Errorf("got %d reports, want %d (stats %+v)", rs.count(), want, c.Stats())
	}
	if n := c.Stats().Internal; n != 0 {
		t.Errorf("Internal = %d, want 0", n)
	}
}

func TestReportPanicValueContextCarriesScope(t *testing.T) {
	c, rs := newTestClient(t, nil)
	ctx := WithScope(context.Background())
	ScopeFromContext(ctx).SetAttribute("request.id", "r-1")
	c.ReportPanicValueContext(ctx, "scoped panic", map[string]any{"call": "yes"})
	attrs := attrsOf(t, flushOne(t, c, rs))
	if attrs["request.id"] != "r-1" || attrs["call"] != "yes" || attrs["report_type"] != "panic" ||
		attrs["error.type"] != "Unhandled exception" {
		t.Errorf("attributes = %v", attrs)
	}
}

func TestReportPanicValueAndFlushContext(t *testing.T) {
	t.Run("scope and delivery", func(t *testing.T) {
		c, rs := newTestClient(t, nil)
		ctx := WithScope(timeoutCtx(t, 5*time.Second))
		ScopeFromContext(ctx).SetAttribute("request.id", "r-2")
		if !c.ReportPanicValueAndFlush(ctx, "flushed", nil) {
			t.Fatal("ReportPanicValueAndFlush = false")
		}
		if rs.count() != 1 {
			t.Fatalf("returned before delivery: %d reports", rs.count())
		}
		if attrs := attrsOf(t, rs.last()); attrs["request.id"] != "r-2" || attrs["report_type"] != "panic" {
			t.Errorf("attributes = %v", attrs)
		}
	})

	t.Run("context deadline bounds the wait", func(t *testing.T) {
		c, rs := newTestClient(t, nil)
		holdMessages(t, rs, "stuck")
		start := time.Now()
		if c.ReportPanicValueAndFlush(timeoutCtx(t, 100*time.Millisecond), "stuck", nil) {
			t.Error("ReportPanicValueAndFlush = true while the server holds the report")
		}
		if waited := time.Since(start); waited < 100*time.Millisecond || waited > 2*time.Second {
			t.Errorf("waited %v, want about 100ms", waited)
		}
	})

	t.Run("cancellation ends the wait and keeps the report", func(t *testing.T) {
		c, rs := newTestClient(t, nil)
		release := holdMessages(t, rs, "cancelled")
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		start := time.Now()
		if c.ReportPanicValueAndFlush(ctx, "cancelled", nil) {
			t.Error("ReportPanicValueAndFlush = true after cancellation")
		}
		if waited := time.Since(start); waited > 2*time.Second {
			t.Errorf("waited %v after cancellation", waited)
		}
		release["cancelled"]()
		flushOne(t, c, rs)
	})

	t.Run("no deadline waits DefaultFlushTimeout", func(t *testing.T) {
		if flushFallbackTimeout != DefaultFlushTimeout {
			t.Fatalf("fallback deadline = %v, want DefaultFlushTimeout", flushFallbackTimeout)
		}
		const short = 100 * time.Millisecond
		flushFallbackTimeout = short
		t.Cleanup(func() { flushFallbackTimeout = DefaultFlushTimeout })
		c, rs := newTestClient(t, nil)
		holdMessages(t, rs, "default deadline")
		start := time.Now()
		if c.ReportPanicValueAndFlush(context.Background(), "default deadline", nil) {
			t.Error("ReportPanicValueAndFlush = true while the server holds the report")
		}
		if waited := time.Since(start); waited < short || waited > short+2*time.Second {
			t.Errorf("waited %v, want about %v", waited, short)
		}
	})
}

// recoveredFrom runs fn and returns the value it panicked with, or nil.
func recoveredFrom(fn func()) (recovered interface{}) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

func TestClientDeferredPanicHelpers(t *testing.T) {
	c, rs := newTestClient(t, nil)

	got := recoveredFrom(func() {
		defer c.ReportPanic(map[string]any{"helper": "ReportPanic"})
		panic("client repanic")
	})
	if got != "client repanic" {
		t.Fatalf("ReportPanic re-panicked with %#v", got)
	}
	if rs.count() != 1 {
		t.Fatalf("ReportPanic re-panicked before delivery (%d reports)", rs.count())
	}
	if attrs := attrsOf(t, rs.last()); attrs["helper"] != "ReportPanic" || attrs["report_type"] != "panic" ||
		attrs["error.message"] != "client repanic" {
		t.Errorf("ReportPanic attributes = %v", attrs)
	}

	if got := recoveredFrom(func() {
		defer c.ReportAndRecoverPanic(map[string]any{"helper": "ReportAndRecoverPanic"})
		panic("client recovered")
	}); got != nil {
		t.Fatalf("ReportAndRecoverPanic let %#v through", got)
	}
	if !c.Flush(5*time.Second) || rs.count() != 2 {
		t.Fatalf("ReportAndRecoverPanic report not delivered (%d reports)", rs.count())
	}
	if attrs := attrsOf(t, rs.last()); attrs["helper"] != "ReportAndRecoverPanic" || attrs["error.message"] != "client recovered" {
		t.Errorf("ReportAndRecoverPanic attributes = %v", attrs)
	}

	func() {
		defer c.ReportPanic(nil)
		defer c.ReportAndRecoverPanic(nil)
	}()
	if !c.Flush(5*time.Second) || rs.count() != 2 {
		t.Errorf("helpers reported without a panic (%d reports)", rs.count())
	}

	var nilClient *Client
	if got := recoveredFrom(func() {
		defer nilClient.ReportAndRecoverPanic(nil)
		panic("nil recover")
	}); got != "nil recover" {
		t.Errorf("nil ReportAndRecoverPanic changed the panic: %#v", got)
	}
	if got := recoveredFrom(func() {
		defer nilClient.ReportPanic(nil)
		panic("nil repanic")
	}); got != "nil repanic" {
		t.Errorf("nil ReportPanic changed the panic: %#v", got)
	}
}

func useDefaultClient(t *testing.T, c *Client) {
	t.Helper()
	SetDefaultClient(c)
	t.Cleanup(func() { SetDefaultClient(nil) })
}

func TestSetDefaultClientRoutesPackageAPI(t *testing.T) {
	legacy := DefaultClient()
	if legacy == nil {
		t.Fatal("the Options-based default client is not configured")
	}
	c, rs := newTestClient(t, nil)
	legacy.Flush(5 * time.Second)
	legacyServer.reset()
	useDefaultClient(t, c)
	if DefaultClient() != c {
		t.Fatal("DefaultClient did not return the client set with SetDefaultClient")
	}

	SetAttribute("routed.single", "yes")
	SetAttributes(map[string]any{"routed.multi": "yes"})
	AddBreadcrumb(Breadcrumb{Message: "routed crumb"})
	Report(errors.New("routed report"), nil)
	ReportPanicValue("routed panic", nil)
	ctx := WithScope(context.Background())
	ScopeFromContext(ctx).SetAttribute("routed.scope", "yes")
	if !ReportPanicValueAndFlush(ctx, "routed flush", nil) {
		t.Error("package ReportPanicValueAndFlush = false")
	}
	if got := recoveredFrom(func() {
		defer ReportPanic(nil)
		panic("routed repanic")
	}); got != "routed repanic" {
		t.Errorf("ReportPanic re-panicked with %#v", got)
	}
	if got := recoveredFrom(func() {
		defer ReportAndRecoverPanic(nil)
		panic("routed recover")
	}); got != nil {
		t.Errorf("ReportAndRecoverPanic let %#v through", got)
	}
	if !Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	if got := rs.messages(); len(got) != 5 {
		t.Fatalf("set client got %q, want 5 reports", got)
	}
	if n := legacyServer.count(); n != 0 {
		t.Errorf("Options-based client got %d reports while another client was set", n)
	}
	for _, msg := range []string{"routed report", "routed panic", "routed flush", "routed repanic", "routed recover"} {
		p, atts := payloadFor(t, rs, msg)
		attrs := attrsOf(t, p)
		if attrs["routed.single"] != "yes" || attrs["routed.multi"] != "yes" {
			t.Errorf("%s: package attributes missing: %v", msg, attrs)
		}
		if got := crumbMessages(breadcrumbsOf(t, atts)); !slices.Equal(got, []string{"routed crumb"}) {
			t.Errorf("%s: breadcrumbs = %q", msg, got)
		}
		if _, scoped := attrs["routed.scope"]; scoped != (msg == "routed flush") {
			t.Errorf("%s: routed.scope = %v", msg, attrs["routed.scope"])
		}
	}
	legacyAttrMu.Lock()
	_, leaked := Options.Attributes["routed.single"]
	legacyAttrMu.Unlock()
	if leaked {
		t.Error("SetAttribute wrote Options.Attributes while a client was set")
	}

	SetDefaultClient(nil)
	if DefaultClient() != legacy {
		t.Fatal("SetDefaultClient(nil) did not return to the Options-based client")
	}
	Report(errors.New("back to legacy"), nil)
	if !Flush(5*time.Second) || legacyServer.count() != 1 || rs.count() != 5 {
		t.Errorf("after SetDefaultClient(nil): legacy got %d, set client got %d", legacyServer.count(), rs.count())
	}
}

func TestFlushWaitsForOptionsClientAfterSwitch(t *testing.T) {
	legacy := DefaultClient()
	if legacy == nil {
		t.Fatal("the Options-based default client is not configured")
	}
	legacy.Flush(5 * time.Second)
	legacyServer.reset()
	release := holdMessages(t, legacyServer, "legacy backlog")
	t.Cleanup(func() {
		legacyServer.mu.Lock()
		legacyServer.holds = nil
		legacyServer.mu.Unlock()
	})
	Report("legacy backlog", nil)

	c, _ := newTestClient(t, nil)
	useDefaultClient(t, c)
	if Flush(100 * time.Millisecond) {
		t.Error("Flush = true while the Options-based client still holds a report")
	}
	release["legacy backlog"]()
	if !Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if got := legacyServer.messages(); !slices.Equal(got, []string{"legacy backlog"}) {
		t.Errorf("Options-based client delivered %q, want the backlog report", got)
	}
}

func TestSetDefaultClientWithoutOptions(t *testing.T) {
	resetDefaultClientForTest(t)
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	Options.Endpoint = ""
	Options.Token = ""
	if DefaultClient() != nil {
		t.Fatal("DefaultClient without configuration is not nil")
	}

	c, rs := newTestClient(t, nil)
	useDefaultClient(t, c)
	if got := recoveredFrom(func() {
		defer ReportAndRecoverPanic(nil)
		panic("recovered through set client")
	}); got != nil {
		t.Fatalf("ReportAndRecoverPanic did not recover with a client set: %#v", got)
	}
	if attrs := attrsOf(t, flushOne(t, c, rs)); attrs["error.message"] != "recovered through set client" {
		t.Errorf("error.message = %v", attrs["error.message"])
	}

	SetDefaultClient(nil)
	if DefaultClient() != nil {
		t.Error("DefaultClient after SetDefaultClient(nil) without configuration is not nil")
	}
	if got := recoveredFrom(func() {
		defer ReportAndRecoverPanic(nil)
		panic("unconfigured again")
	}); got != "unconfigured again" {
		t.Errorf("unconfigured ReportAndRecoverPanic recovered %#v", got)
	}
}
