package bt

import (
	"cmp"
	"encoding/json"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Breadcrumb levels understood by the Backtrace UI.
const (
	BreadcrumbDebug   = "debug"
	BreadcrumbInfo    = "info"
	BreadcrumbWarning = "warning"
	BreadcrumbError   = "error"
	BreadcrumbFatal   = "fatal"
)

// Breadcrumb types understood by the Backtrace UI.
const (
	BreadcrumbTypeManual        = "manual"
	BreadcrumbTypeLog           = "log"
	BreadcrumbTypeNavigation    = "navigation"
	BreadcrumbTypeHTTP          = "http"
	BreadcrumbTypeSystem        = "system"
	BreadcrumbTypeUser          = "user"
	BreadcrumbTypeConfiguration = "configuration"
)

const (
	maxBreadcrumbMessageBytes = 1024
	maxBreadcrumbBytes        = 4096
)

// Breadcrumb is a lightweight trail entry recorded before an error occurs.
// Reports carry the breadcrumbs in ReportData.Breadcrumbs.
//
// Each breadcrumb is limited to 4 KiB of JSON. The message is cut to fit,
// and the attributes are dropped when they do not fit even without a
// message. A breadcrumb that still does not fit is discarded.
type Breadcrumb struct {
	// Timestamp in Unix milliseconds. Filled automatically by AddBreadcrumb
	// when zero.
	Timestamp int64 `json:"timestamp"`

	// ID is a monotonically increasing sequence number. All clients and
	// scopes share the sequence.
	ID uint64 `json:"id"`

	// Level is one of the Breadcrumb level constants; defaults to "info".
	Level string `json:"level"`

	// Type categorizes the breadcrumb ("manual", "http", "log", ...);
	// defaults to "manual".
	Type string `json:"type"`

	// Message is the human-readable description. It is cut at 1024 bytes.
	Message string `json:"message"`

	// Attributes carry optional structured metadata. Values are converted
	// like report attributes, except that maps, slices and structs become
	// JSON text and nil values are dropped.
	Attributes map[string]interface{} `json:"attributes,omitempty"`
}

var (
	breadcrumbIDSeed = uint64(time.Now().UnixMilli())
	breadcrumbIDs    atomic.Uint64
)

// breadcrumbRing is a fixed-capacity, concurrency-safe ring buffer.
type breadcrumbRing struct {
	mu   sync.Mutex
	buf  []Breadcrumb
	head int // index of oldest element
	size int // number of stored elements
}

func newBreadcrumbRing(capacity int) *breadcrumbRing {
	if capacity <= 0 {
		return nil
	}
	return &breadcrumbRing{buf: make([]Breadcrumb, capacity)}
}

func prepareBreadcrumb(b Breadcrumb) (Breadcrumb, bool) {
	if b.Timestamp == 0 {
		b.Timestamp = time.Now().UnixMilli()
	}
	if b.Level == "" {
		b.Level = BreadcrumbInfo
	}
	if b.Type == "" {
		b.Type = "manual"
	}
	b.Message = truncateUTF8(b.Message, maxBreadcrumbMessageBytes)
	b.Attributes = breadcrumbAttributes(b.Attributes)

	id := b.ID
	// The real ID is assigned later. Measure with the widest one.
	b.ID = math.MaxUint64
	b, ok := fitBreadcrumb(b)
	b.ID = id
	return b, ok
}

func breadcrumbAttributes(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	frozen := freezeAttributes(in, valueLimits{})
	out := make(map[string]any, len(frozen))
	for k, v := range frozen {
		value, kind := scalarValue(v)
		switch kind {
		case valueNil:
			continue
		case valueComposite:
			value = string(encodeComposite(v))
		}
		out[k] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func fitBreadcrumb(b Breadcrumb) (Breadcrumb, bool) {
	if encodedBreadcrumbSize(b) <= maxBreadcrumbBytes {
		return b, true
	}
	msg := b.Message
	b.Message = ""
	if encodedBreadcrumbSize(b) > maxBreadcrumbBytes {
		b.Attributes = nil
	}
	lo, hi := 0, len(msg)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		b.Message = truncateUTF8(msg, mid)
		if encodedBreadcrumbSize(b) <= maxBreadcrumbBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	b.Message = truncateUTF8(msg, lo)
	return b, encodedBreadcrumbSize(b) <= maxBreadcrumbBytes
}

func encodedBreadcrumbSize(b Breadcrumb) int {
	data, err := json.Marshal(b)
	if err != nil {
		return math.MaxInt
	}
	return len(data)
}

func (r *breadcrumbRing) add(b Breadcrumb) {
	if r == nil {
		return
	}
	b, ok := prepareBreadcrumb(b)
	if !ok {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	b.ID = breadcrumbIDSeed + breadcrumbIDs.Add(1) - 1
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
	// Entries were normalised on add. Each report still gets its own copy
	// of the attribute maps, so a BeforeSend hook that edits one report's
	// breadcrumbs cannot change the ring or any later report.
	out := make([]Breadcrumb, r.size)
	for i := 0; i < r.size; i++ {
		crumb := r.buf[(r.head+i)%len(r.buf)]
		crumb.Attributes = copyModelMap(crumb.Attributes)
		out[i] = crumb
	}
	return out
}

func mergeBreadcrumbs(client []Breadcrumb, scopes []*Scope, limit int) []Breadcrumb {
	if limit <= 0 {
		return nil
	}
	merged := client
	for _, s := range scopes {
		merged = append(merged, s.breadcrumbs()...)
	}
	slices.SortFunc(merged, func(a, b Breadcrumb) int {
		return cmp.Or(cmp.Compare(a.Timestamp, b.Timestamp), cmp.Compare(a.ID, b.ID))
	})
	if len(merged) > limit {
		merged = merged[len(merged)-limit:]
	}
	return merged
}

func maxBreadcrumbID(crumbs []Breadcrumb) uint64 {
	var id uint64
	for _, b := range crumbs {
		id = max(id, b.ID)
	}
	return id
}
