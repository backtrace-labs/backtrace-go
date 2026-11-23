package bt

import (
	"encoding/json"
	"errors"
	"fmt"
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
