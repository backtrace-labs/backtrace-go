package bt

import (
	"sync"
	"time"
)

// Breadcrumb levels understood by the Backtrace UI.
const (
	BreadcrumbDebug   = "debug"
	BreadcrumbInfo    = "info"
	BreadcrumbWarning = "warning"
	BreadcrumbError   = "error"
)

// Breadcrumb is a lightweight trail entry recorded before an error occurs.
// Breadcrumbs are attached to every report as the "breadcrumbs" annotation.
type Breadcrumb struct {
	// Timestamp in Unix milliseconds. Filled automatically by AddBreadcrumb
	// when zero.
	Timestamp int64 `json:"timestamp"`

	// ID is a monotonically increasing sequence number.
	ID uint64 `json:"id"`

	// Level is one of the Breadcrumb* constants; defaults to "info".
	Level string `json:"level"`

	// Type categorizes the breadcrumb ("manual", "http", "log", ...);
	// defaults to "manual".
	Type string `json:"type"`

	// Message is the human-readable description.
	Message string `json:"message"`

	// Attributes carry optional structured metadata.
	Attributes map[string]interface{} `json:"attributes,omitempty"`
}

// breadcrumbRing is a fixed-capacity, concurrency-safe ring buffer.
type breadcrumbRing struct {
	mu   sync.Mutex
	buf  []Breadcrumb
	next uint64 // sequence counter
	head int    // index of oldest element
	size int    // number of stored elements
}

func newBreadcrumbRing(capacity int) *breadcrumbRing {
	if capacity <= 0 {
		return nil
	}
	return &breadcrumbRing{buf: make([]Breadcrumb, capacity)}
}

// cloneBreadcrumb detaches the attribute map from caller ownership.
func cloneBreadcrumb(b Breadcrumb) Breadcrumb {
	b.Attributes = cloneAnyMap(b.Attributes)
	return b
}

func (r *breadcrumbRing) add(b Breadcrumb) {
	if r == nil {
		return
	}
	b = cloneBreadcrumb(b)
	if b.Timestamp == 0 {
		b.Timestamp = time.Now().UnixMilli()
	}
	if b.Level == "" {
		b.Level = BreadcrumbInfo
	}
	if b.Type == "" {
		b.Type = "manual"
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	b.ID = r.next
	r.next++
	if r.size < len(r.buf) {
		r.buf[(r.head+r.size)%len(r.buf)] = b
		r.size++
		return
	}
	// Full: overwrite the oldest entry.
	r.buf[r.head] = b
	r.head = (r.head + 1) % len(r.buf)
}

// snapshot returns breadcrumbs ordered oldest to newest.
func (r *breadcrumbRing) snapshot() []Breadcrumb {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size == 0 {
		return nil
	}
	out := make([]Breadcrumb, r.size)
	for i := 0; i < r.size; i++ {
		out[i] = cloneBreadcrumb(r.buf[(r.head+i)%len(r.buf)])
	}
	return out
}
