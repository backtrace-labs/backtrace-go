package bt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingError struct {
	msg   string
	calls atomic.Int32
}

func (e *countingError) Error() string {
	e.calls.Add(1)
	return e.msg
}

type countingStringer struct{ calls atomic.Int32 }

func (s *countingStringer) String() string {
	s.calls.Add(1)
	return "stringer"
}

func TestReportLimiterSlidingWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newReportLimiter(3)
	l.now = func() time.Time { return now }
	steps := []struct {
		advance   time.Duration
		ok, first bool
	}{
		{0, true, false},
		{10 * time.Second, true, false},
		{10 * time.Second, true, false},
		{39 * time.Second, false, true},
		{0, false, false},
		{time.Second, true, false},
		{time.Second, false, true},
		{9 * time.Second, true, false},
		{10 * time.Second, true, false},
		{0, false, true},
	}
	for i, s := range steps {
		now = now.Add(s.advance)
		if _, ok, first := l.allow(); ok != s.ok || first != s.first {
			t.Errorf("step %d: allow() = %v, %v, want %v, %v", i, ok, first, s.ok, s.first)
		}
	}
}

func TestReportLimiterMatchesWindowModel(t *testing.T) {
	for _, limit := range []int{1, 2, 5, 17, 40, 300} {
		rng := rand.New(rand.NewPCG(uint64(limit), 7))
		now := time.Unix(1_700_000_000, 0)
		l := newReportLimiter(limit)
		l.now = func() time.Time { return now }
		var allowed []time.Time
		for i := 0; i < 8000; i++ {
			now = now.Add(time.Duration(rng.Int64N(int64(2 * time.Minute / time.Duration(limit)))))
			inWindow := 0
			for _, ts := range allowed {
				if now.Sub(ts) < time.Minute {
					inWindow++
				}
			}
			want := inWindow < limit
			slot, got, _ := l.allow()
			if got != want {
				t.Fatalf("limit %d, step %d: allow() = %v, want %v (%d in window)", limit, i, got, want, inWindow)
			}
			if want {
				allowed = append(allowed, slot)
			}
			if len(allowed) > 0 && rng.IntN(4) == 0 {
				k := rng.IntN(len(allowed))
				l.release(allowed[k])
				allowed = append(allowed[:k], allowed[k+1:]...)
			}
			if len(l.ring) > limit {
				t.Fatalf("limit %d: ring holds %d slots", limit, len(l.ring))
			}
			if len(allowed) > 4*limit {
				allowed = allowed[len(allowed)-limit:]
			}
		}
	}
}

func TestReportLimiterIsAtomic(t *testing.T) {
	l := newReportLimiter(100)
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if _, ok, _ := l.allow(); ok {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 100 {
		t.Errorf("allowed %d of 500 concurrent reports, want 100", allowed.Load())
	}
}

func TestReportLimiterDisabled(t *testing.T) {
	l := newReportLimiter(0)
	if l != nil {
		t.Fatal("ReportsPerMinute 0 created a limiter")
	}
	for i := 0; i < 1000; i++ {
		if _, ok, first := l.allow(); !ok || first {
			t.Fatal("disabled limiter rejected a report")
		}
	}
}

func TestReportsPerMinuteConfig(t *testing.T) {
	base := Config{Endpoint: "https://submit.backtrace.io/u/t/json"}
	for _, n := range []int{0, 1, math.MaxInt} {
		c := base
		c.ReportsPerMinute = n
		if err := c.normalize().validate(); err != nil {
			t.Errorf("ReportsPerMinute = %d rejected: %v", n, err)
		}
		if got := c.normalize().ReportsPerMinute; got != n {
			t.Errorf("normalize changed ReportsPerMinute %d to %d", n, got)
		}
	}
	huge := newReportLimiter(math.MaxInt)
	for i := 0; i < 20; i++ {
		huge.allow()
	}
	if len(huge.ring) > 32 {
		t.Errorf("limit math.MaxInt allocated %d slots for 20 reports", len(huge.ring))
	}

	neg := base
	neg.ReportsPerMinute = -1
	if _, err := NewClient(neg); err == nil || !strings.Contains(err.Error(), "ReportsPerMinute") {
		t.Errorf("NewClient with ReportsPerMinute -1: err = %v, want a ReportsPerMinute error", err)
	}

	old := Options.ReportsPerMinute
	t.Cleanup(func() { Options.ReportsPerMinute = old })
	Options.ReportsPerMinute = 7
	if got := optionsToConfig().ReportsPerMinute; got != 7 {
		t.Errorf("Options.ReportsPerMinute not carried into the legacy config: %d", got)
	}
}

func TestClientRateLimitDropsBeforeCapture(t *testing.T) {
	var hooks atomic.Int32
	logs := &lineLogger{}
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.ReportsPerMinute = 3
		cfg.Debug = true
		cfg.Logger = logs
		cfg.BeforeSend = func(r *ReportData) *ReportData { hooks.Add(1); return r }
	})

	errs := make([]*countingError, 5)
	attrs := make([]*countingStringer, 5)
	for i := range errs {
		errs[i] = &countingError{msg: fmt.Sprintf("e%d", i)}
		attrs[i] = &countingStringer{}
		c.ReportError(errs[i], map[string]any{"lazy": attrs[i]})
	}
	object := &countingStringer{}
	c.Report(object, nil)
	c.ReportPanicValue("panic over the limit", nil)
	if c.ReportPanicValueAndFlush(timeoutCtx(t, time.Second), "flushed panic over the limit", nil) {
		t.Error("ReportPanicValueAndFlush = true for a rate-limited report")
	}
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	if got := strings.Join(rs.messages(), ","); got != "e0,e1,e2" {
		t.Errorf("delivered messages = %q, want e0,e1,e2", got)
	}
	for i := 3; i < 5; i++ {
		if n := errs[i].calls.Load(); n != 0 {
			t.Errorf("rate-limited error %d: Error() called %d times", i, n)
		}
		if n := attrs[i].calls.Load(); n != 0 {
			t.Errorf("rate-limited report %d: attribute String() called %d times", i, n)
		}
	}
	if n := object.calls.Load(); n != 0 {
		t.Errorf("rate-limited Report object: String() called %d times", n)
	}
	s := c.Stats()
	if s.Accepted != 3 || s.Delivered != 3 || s.ClientRateLimited != 5 || c.DroppedReports() != 5 {
		t.Errorf("stats = %+v, dropped %d; want Accepted 3, Delivered 3, ClientRateLimited 5, dropped 5", s, c.DroppedReports())
	}
	if hooks.Load() != 3 {
		t.Errorf("BeforeSend ran %d times, want 3", hooks.Load())
	}
	limitLines := 0
	for _, line := range logs.all() {
		if strings.Contains(line, "3 reports per minute") {
			limitLines++
		}
	}
	if limitLines != 1 {
		t.Errorf("rate limit logged %d times, want once per episode: %q", limitLines, logs.all())
	}
}

func TestSampledReportsDoNotUseRateBudget(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.ReportsPerMinute = 1
		cfg.SampleRate = math.SmallestNonzeroFloat64
	})
	for i := 0; i < 5; i++ {
		c.ReportMessage("sampled out", nil)
	}
	c.ReportPanicValue("first panic", nil)
	c.ReportPanicValue("second panic", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if got := rs.messages(); len(got) != 1 || got[0] != "first panic" {
		t.Errorf("delivered = %q, want only the first panic", got)
	}
	if s := c.Stats(); s.Sampled != 5 || s.ClientRateLimited != 1 || s.Accepted != 1 {
		t.Errorf("stats = %+v, want Sampled 5, ClientRateLimited 1, Accepted 1", s)
	}
}

func TestFullQueueDropsBeforeCapture(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.QueueSize = 1
		cfg.ReportsPerMinute = 4
	})
	release := holdMessages(t, rs, "occupies the worker", "fills the priority queue")

	c.ReportMessage("occupies the worker", nil)
	entered(t, rs)
	c.ReportMessage("fills the queue", nil)
	if c.ReportPanicValueAndFlush(timeoutCtx(t, 20*time.Millisecond), "fills the priority queue", nil) {
		t.Fatal("ReportPanicValueAndFlush = true while the worker is held")
	}

	regular := &countingError{msg: "regular over a full queue"}
	attr := &countingStringer{}
	c.ReportError(regular, map[string]any{"lazy": attr})
	priority := &countingError{msg: "expired flush over a full queue"}
	if c.ReportPanicValueAndFlush(timeoutCtx(t, 0), priority, nil) {
		t.Error("ReportPanicValueAndFlush with an expired deadline = true over a full queue")
	}
	live := WithScope(context.Background())
	scoped := &countingError{msg: "scoped over a full queue"}
	for name, report := range map[string]func(){
		"Report":                  func() { c.Report(scoped, nil) },
		"ReportContext":           func() { c.ReportContext(live, scoped, nil) },
		"ReportPanicValueContext": func() { c.ReportPanicValueContext(live, scoped, nil) },
	} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			report()
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s blocked on a full queue", name)
		}
	}
	if n := regular.calls.Load() + attr.calls.Load() + priority.calls.Load() + scoped.calls.Load(); n != 0 {
		t.Errorf("queue-full reports ran %d Error/String calls, want none", n)
	}
	if s := c.Stats(); s.QueueFull != 5 || s.ClientRateLimited != 0 || s.Accepted != 3 {
		t.Errorf("stats = %+v, want QueueFull 5, ClientRateLimited 0, Accepted 3", s)
	}

	release["occupies the worker"]()
	release["fills the priority queue"]()
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	c.ReportMessage("uses the last slot", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	c.ReportMessage("over the limit", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if s := c.Stats(); s.Accepted != 4 || s.Delivered != 4 || s.ClientRateLimited != 1 {
		t.Errorf("stats = %+v, want Accepted 4, Delivered 4, ClientRateLimited 1: queue-full drops must not use the budget", s)
	}
}

func TestClosedClientDropsBeforeCapture(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.Close()

	err := &countingError{msg: "after close"}
	attr := &countingStringer{}
	c.ReportError(err, map[string]any{"lazy": attr})
	start := time.Now()
	if c.ReportPanicValueAndFlush(timeoutCtx(t, 5*time.Second), err, nil) {
		t.Error("ReportPanicValueAndFlush = true on a closed client")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("ReportPanicValueAndFlush on a closed client waited %v", waited)
	}
	if n := err.calls.Load() + attr.calls.Load(); n != 0 {
		t.Errorf("reports on a closed client ran %d Error/String calls, want none", n)
	}
	if s := c.Stats(); s.Closed != 2 {
		t.Errorf("stats = %+v, want Closed 2", s)
	}
}

var errSentinel = errors.New("sentinel")

func TestBeforeSendSeesOriginalError(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]error{}
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.BeforeSend = func(r *ReportData) *ReportData {
			msg, _ := r.Attributes["error.message"].(string)
			mu.Lock()
			seen[msg] = r.Error
			mu.Unlock()
			return r
		}
	})
	panicErr := &testWrapErr{msg: "code 3"}
	c.ReportError(fmt.Errorf("lookup: %w", errSentinel), nil)
	c.Report(fmt.Errorf("decode: %w", &testWrapErr{msg: "code 7"}), nil)
	c.ReportPanicValue(panicErr, nil)
	c.ReportPanicValue("plain panic", nil)
	c.ReportMessage("plain message", nil)
	c.Report("message object", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(seen["lookup: sentinel"], errSentinel) {
		t.Errorf("errors.Is(report.Error, errSentinel) = false, Error = %v", seen["lookup: sentinel"])
	}
	var coded *testWrapErr
	if !errors.As(seen["decode: code 7"], &coded) || coded.msg != "code 7" {
		t.Errorf("errors.As(report.Error, *testWrapErr) failed, Error = %v", seen["decode: code 7"])
	}
	if seen["code 3"] != error(panicErr) {
		t.Errorf("panic report Error = %v, want the panic value", seen["code 3"])
	}
	for _, msg := range []string{"plain panic", "plain message", "message object"} {
		if err, ok := seen[msg]; !ok || err != nil {
			t.Errorf("%q: Error = %v (seen %v), want nil", msg, err, ok)
		}
	}
	if rs.count() != 6 {
		t.Errorf("requests = %d, want 6", rs.count())
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, p := range rs.payloads {
		for k := range p {
			if strings.EqualFold(k, "error") {
				t.Errorf("payload carries top-level key %q", k)
			}
		}
	}
	if body, err := json.Marshal(ReportData{Error: errSentinel}); err != nil || strings.Contains(string(body), `"Error"`) {
		t.Errorf("json.Marshal(ReportData) = %s, %v; want Error left out", body, err)
	}
}

func TestReportLimiterRelease(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	l := newReportLimiter(2)
	l.now = func() time.Time { return now }
	first, _, _ := l.allow()
	now = base.Add(10 * time.Second)
	second, _, _ := l.allow()
	if _, ok, _ := l.allow(); ok {
		t.Fatal("third report allowed with a limit of 2")
	}
	l.release(first)
	now = base.Add(20 * time.Second)
	if _, ok, _ := l.allow(); !ok {
		t.Fatal("released slot not reused")
	}
	now = base.Add(69 * time.Second)
	if _, ok, _ := l.allow(); ok {
		t.Fatal("allowed while the 10s and 20s reports are in the window")
	}
	now = base.Add(70 * time.Second)
	if _, ok, _ := l.allow(); !ok {
		t.Fatal("not allowed after the 10s report left the window")
	}
	l.release(second)
	if _, ok, _ := l.allow(); ok {
		t.Error("releasing an expired slot freed a live one")
	}
	var disabled *reportLimiter
	disabled.release(now)
	l.release(time.Time{})
	if l.count != 2 {
		t.Errorf("zero-slot release changed the count to %d", l.count)
	}
}

func TestRateLimitedPanicFlushWaitsForBacklog(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) { cfg.ReportsPerMinute = 2 })
	release := holdMessages(t, rs, "e1")
	c.ReportMessage("e1", nil)
	entered(t, rs)
	c.ReportMessage("e2", nil)

	type result struct {
		ok        bool
		delivered uint64
	}
	done := make(chan result, 1)
	ctx := timeoutCtx(t, 5*time.Second)
	go func() {
		ok := c.ReportPanicValueAndFlush(ctx, "dying", nil)
		done <- result{ok, c.Stats().Delivered}
	}()
	waitFor(t, "the panic report to be rate limited", func() bool { return c.Stats().ClientRateLimited == 1 })
	select {
	case r := <-done:
		t.Fatalf("ReportPanicValueAndFlush returned %+v before the earlier reports were delivered", r)
	default:
	}
	release["e1"]()
	r := <-done
	if r.ok || r.delivered != 2 {
		t.Errorf("ReportPanicValueAndFlush = %v with %d delivered at return, want false with 2", r.ok, r.delivered)
	}
}

func TestUnacceptedReportReturnsRateSlot(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.QueueSize = 1
		cfg.ReportsPerMinute = 3
	})
	release := holdMessages(t, rs, "occupies the worker", "fills the priority queue")
	c.ReportMessage("occupies the worker", nil)
	entered(t, rs)
	if c.ReportPanicValueAndFlush(timeoutCtx(t, 20*time.Millisecond), "fills the priority queue", nil) {
		t.Fatal("ReportPanicValueAndFlush = true while the worker is held")
	}
	waiting := &countingError{msg: "waits on a full queue"}
	if c.ReportPanicValueAndFlush(timeoutCtx(t, 20*time.Millisecond), waiting, nil) {
		t.Fatal("ReportPanicValueAndFlush = true over a full priority queue")
	}
	if waiting.calls.Load() == 0 {
		t.Fatal("waiting panic report was not captured before its deadline")
	}

	release["occupies the worker"]()
	release["fills the priority queue"]()
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	c.ReportMessage("uses the returned slot", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	c.ReportMessage("over the limit", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if s := c.Stats(); s.Accepted != 3 || s.Delivered != 3 || s.QueueFull != 1 || s.ClientRateLimited != 1 {
		t.Errorf("stats = %+v, want Accepted 3, Delivered 3, QueueFull 1, ClientRateLimited 1", s)
	}
}

func TestRateLimitedDropDoesNotAllocate(t *testing.T) {
	limited, _ := newTestClient(t, func(cfg *Config) { cfg.ReportsPerMinute = 1 })
	limited.ReportMessage("uses the only slot", nil)
	if !limited.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}

	if n := testing.AllocsPerRun(200, func() { limited.ReportMessage("dropped", nil) }); n != 0 {
		t.Errorf("ReportMessage allocates %v times per call, want 0", n)
	}
	if s := limited.Stats(); s.Accepted != 1 || s.ClientRateLimited == 0 {
		t.Errorf("stats = %+v, want Accepted 1, ClientRateLimited > 0", s)
	}
}
