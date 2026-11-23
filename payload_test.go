package bt

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func flushOne(t *testing.T, c *Client, rs *recordingServer) map[string]any {
	t.Helper()
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if rs.count() != 1 {
		t.Fatalf("got %d reports, want 1 (stats %+v)", rs.count(), c.Stats())
	}
	return rs.last()
}

func annotationsOf(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	annotations, _ := payload["annotations"].(map[string]any)
	return annotations
}

func TestAttributeValuesAreBackendStorable(t *testing.T) {
	c, rs := newTestClient(t, nil)
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	c.ReportMessage("values", map[string]any{
		"ratio":  math.NaN(),
		"inf":    math.Inf(1),
		"neg":    -5,
		"frac":   0.75,
		"whole":  3.0,
		"cause":  io.ErrUnexpectedEOF,
		"when":   when,
		"tags":   []string{"a", "b"},
		"nested": map[string]int{"depth": 1},
	})
	payload := flushOne(t, c, rs)
	attrs := attrsOf(t, payload)

	want := map[string]any{
		"ratio": "[bt: non-finite float NaN]",
		"inf":   "[bt: non-finite float +Inf]",
		"neg":   "-5",
		"frac":  "0.75",
		"whole": float64(3),
		"cause": "unexpected EOF",
		"when":  "2026-01-02T03:04:05Z",
	}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("attribute %s = %#v, want %#v", k, attrs[k], v)
		}
	}
	for k, v := range attrs {
		switch x := v.(type) {
		case string, bool:
		case float64:
			if x < 0 || x != math.Trunc(x) {
				t.Errorf("attribute %s = %v is not a non-negative integer", k, x)
			}
		default:
			t.Errorf("attribute %s has non-scalar wire value %#v", k, v)
		}
	}

	annotations := annotationsOf(t, payload)
	if tags, _ := annotations["tags"].([]any); len(tags) != 2 || tags[0] != "a" {
		t.Errorf("tags annotation = %#v", annotations["tags"])
	}
	if nested, _ := annotations["nested"].(map[string]any); nested["depth"] != float64(1) {
		t.Errorf("nested annotation = %#v", annotations["nested"])
	}
	if s := c.Stats(); s.Delivered != 1 || s.Serialization != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestNilAttributeSuppressesDefaults(t *testing.T) {
	var workerDefault string
	for k, v := range machineAttributes(diag{}) {
		if s, ok := v.(string); ok && s != "" {
			workerDefault = k
			break
		}
	}

	c, rs := newTestClient(t, nil)
	c.SetAttribute("hostname", nil)
	if workerDefault != "" {
		c.SetAttribute(workerDefault, nil)
	}
	c.ReportMessage("suppressed", map[string]any{"process.id": nil})

	attrs := attrsOf(t, flushOne(t, c, rs))
	for _, k := range []string{"hostname", "process.id", workerDefault} {
		if _, ok := attrs[k]; ok && k != "" {
			t.Errorf("attribute %s sent despite a nil value", k)
		}
	}
	if _, ok := attrs["application"]; !ok {
		t.Error("unrelated default attribute missing")
	}
}

func TestEmptyClassifiersAreRestored(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.BeforeSend = func(r *ReportData) *ReportData {
			r.Classifiers[0] = ""
			return r
		}
	})
	c.ReportMessage("empty classifiers", nil)
	got, _ := flushOne(t, c, rs)["classifiers"].([]any)
	if len(got) != 1 || got[0] != "message" {
		t.Errorf("classifiers = %v, want the captured [message]", got)
	}
}

func TestUnencodableBreadcrumbDoesNotPoisonReports(t *testing.T) {
	c, rs := newTestClient(t, nil)
	c.AddBreadcrumb(Breadcrumb{Message: "bad", Attributes: map[string]any{"v": math.Inf(1)}})
	c.AddBreadcrumb(Breadcrumb{Message: strings.Repeat("A", 9<<20)})
	for i := 0; i < 3; i++ {
		c.ReportMessage(fmt.Sprintf("report %d", i), nil)
	}
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if s := c.Stats(); s.Delivered != 3 || s.Serialization != 0 || s.Oversize != 0 {
		t.Errorf("stats = %+v, want 3 delivered", s)
	}
	if part := rs.lastAttachments()["attachment_bt-breadcrumbs-0"]; len(part) > 2*maxBreadcrumbBytes {
		t.Errorf("breadcrumb part = %d bytes", len(part))
	}
}

func TestLongMessageKeepsFullText(t *testing.T) {
	long := strings.Repeat("é", 1000) + "TAIL"
	cases := []struct {
		name   string
		cfg    func(*Config)
		report func(*Client)
		want   string
	}{
		{"message", nil, func(c *Client) { c.ReportMessage(long, nil) }, long},
		{"non-error panic", nil, func(c *Client) { c.ReportPanicValue(long, nil) }, long},
		{"error chain", nil, func(c *Client) { c.ReportError(fmt.Errorf("wrapped: %w", errors.New(long)), nil) }, "wrapped: " + long},
		{"chain disabled", func(cfg *Config) { cfg.MaxErrorDepth = -1 }, func(c *Client) { c.ReportError(errors.New(long), nil) }, long},
	}
	for _, tc := range cases {
		c, rs := newTestClient(t, tc.cfg)
		tc.report(c)
		payload := flushOne(t, c, rs)
		if msg, _ := attrsOf(t, payload)["error.message"].(string); len(msg) > maxErrorMessageBytes || !utf8.ValidString(msg) || !strings.HasPrefix(tc.want, msg) {
			t.Errorf("%s: error.message = %d bytes, want a valid prefix of at most %d", tc.name, len(msg), maxErrorMessageBytes)
		}
		if got := annotationsOf(t, payload)[messageAnnotation]; got != tc.want {
			t.Errorf("%s: Message annotation = %.40q, want the full text", tc.name, got)
		}
	}
}

func TestOversizeReportIsTrimmed(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.MaxReportBytes = 16 << 10
	})
	c.ReportMessage("big", map[string]any{"blob": strings.Repeat("b", 32<<10)})
	payload := flushOne(t, c, rs)

	attrs := attrsOf(t, payload)
	if blob, _ := attrs["blob"].(string); len(blob) != maxTrimmedAttributeBytes {
		t.Errorf("blob = %d bytes, want %d", len(blob), maxTrimmedAttributeBytes)
	}
	if steps, _ := attrs[truncatedAttribute].(string); !strings.HasSuffix(steps, "long_attributes") {
		t.Errorf("%s = %q", truncatedAttribute, steps)
	}
	if s := c.Stats(); s.Delivered != 1 || s.Oversize != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestOversizeReportDroppedAfterTrimming(t *testing.T) {
	c, rs := newTestClient(t, func(cfg *Config) {
		cfg.MaxReportBytes = 512
	})
	c.ReportMessage("cannot fit", nil)
	if !c.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if rs.count() != 0 || c.Stats().Oversize != 1 {
		t.Errorf("reports = %d, stats = %+v; want one Oversize drop", rs.count(), c.Stats())
	}
}
