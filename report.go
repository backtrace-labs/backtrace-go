package bt

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// ReportData is a fully assembled crash/error report, exposed to the
// Config.BeforeSend hook for scrubbing, enrichment, or dropping.
//
// Mutating maps and slices in place is safe: the hook runs on the SDK worker
// goroutine, on this report's own copy, and the report is serialized
// immediately afterwards.
//
// # Value model
//
// Attribute values, and the attributes of breadcrumbs, are normalised when
// the application hands them to the SDK. A hook therefore sees only these
// types in Attributes, for the attributes the SDK adds as well as for the
// application's own:
//
//	nil, bool, string, int64, uint64, float64,
//	[]interface{}, map[string]interface{}
//
// An int arrives as int64, a []string as []interface{}, a
// map[string]string as map[string]interface{}, a time.Time as an RFC 3339
// string and a struct as a map of its exported fields. Field names follow
// the json tags. Type assertions in a hook must use these types:
//
//	if h, ok := r.Attributes["headers"].(map[string]interface{}); ok {
//		delete(h, "Authorization")
//	}
//
// Annotations are assembled by the SDK and keep its types: "Environment
// Variables" is a map[string]string and "Dependencies" a
// map[string]map[string]string keyed by module path.
//
// # What a hook may change
//
// Everything except UUID, which identifies the report in SDK diagnostics and
// is always restored. Removing data sticks: SourceCode may be set to nil,
// Threads may be trimmed. The SDK only repairs what the wire format requires
// (a positive Timestamp, a non-empty classifier, at least one thread, a
// MainThread that exists, no frame referring to a removed SourceCode entry).
type ReportData struct {
	// UUID uniquely identifies the report (RFC 4122 version 4).
	UUID string

	// Timestamp is the report time in Unix seconds.
	Timestamp int64

	// Classifiers group the report in the Backtrace UI; the SDK sets
	// exactly one of "error", "panic", or "message". Error-graph type
	// names are reported via the "error.type" attribute and the
	// "Error Chain" annotation, not as classifiers.
	Classifiers []string

	// Attributes are indexed key/value pairs used for search and
	// aggregation. After BeforeSend, values become strings, booleans or
	// non-negative integers, maps and slices move to Annotations, and nil
	// values are dropped.
	Attributes map[string]interface{}

	// Annotations carry larger, non-indexed structured data
	// (environment variables, error chains, dependencies, breadcrumbs).
	Annotations map[string]interface{}

	// Threads maps thread IDs to captured goroutine stacks.
	Threads map[string]Thread

	// SourceCode maps source snippet IDs referenced by stack frames.
	SourceCode map[string]SourceCode

	// MainThread is the key in Threads of the faulting goroutine.
	MainThread string

	// Attachments lists file paths uploaded with the report as
	// "attachment_<basename>" multipart parts. Seeded from
	// Config.AttachmentPaths; BeforeSend may add or remove entries.
	Attachments []string

	// Error is the reported error, or nil for messages, other panic values
	// and crash reports. It is not a copy and it is not sent. BeforeSend can
	// inspect it with errors.Is and errors.As.
	Error error `json:"-"`
}

// toWire converts the report to the Backtrace JSON submission format.
func (r *ReportData) toWire() map[string]interface{} {
	return map[string]interface{}{
		"uuid":         r.UUID,
		"timestamp":    r.Timestamp,
		"lang":         "go",
		"langVersion":  runtime.Version(),
		"agent":        "backtrace-go",
		"agentVersion": Version,
		"classifiers":  r.Classifiers,
		"attributes":   r.Attributes,
		"annotations":  encodeAnnotations(r.Annotations),
		"threads":      r.Threads,
		"mainThread":   r.MainThread,
		"sourceCode":   r.SourceCode,
	}
}

func ensureMainThread(r *ReportData) {
	if r.MainThread == "" {
		r.MainThread = "main"
	}
	if _, ok := r.Threads[r.MainThread]; !ok {
		r.Threads[r.MainThread] = Thread{Name: "main", Fault: true, Stacks: []StackFrame{}}
	}
}

const (
	maxTrimmedAttributeBytes = 4096
	maxTrimmedErrorChain     = 10
	truncatedAttribute       = "backtrace.truncated"
)

var trimSteps = []struct {
	name  string
	apply func(*ReportData) bool
}{
	{"source_text", dropSourceText},
	{"threads", dropOtherThreads},
	{"dependencies_env", dropBulkAnnotations},
	{"error_chain", trimErrorChain},
	{"long_attributes", truncateLongAttributes},
	{"annotations", dropAnnotations},
}

func shrinkReport(r *ReportData, limit int) (body []byte, steps []string, err error) {
	for _, step := range trimSteps {
		if !step.apply(r) {
			continue
		}
		steps = append(steps, step.name)
		r.Attributes[truncatedAttribute] = strings.Join(steps, ",")
		if body, err = json.Marshal(r.toWire()); err != nil || len(body) <= limit {
			return body, steps, err
		}
	}
	return nil, steps, nil
}

func dropSourceText(r *ReportData) bool {
	changed := false
	for id, sc := range r.SourceCode {
		if sc != (SourceCode{Path: sc.Path}) {
			r.SourceCode[id] = SourceCode{Path: sc.Path}
			changed = true
		}
	}
	return changed
}

func dropOtherThreads(r *ReportData) bool {
	changed := false
	for id, t := range r.Threads {
		if id != r.MainThread && !t.Fault {
			delete(r.Threads, id)
			changed = true
		}
	}
	if !changed {
		return false
	}
	referenced := map[string]bool{}
	for _, t := range r.Threads {
		for _, f := range t.Stacks {
			referenced[f.SourceCodeID] = true
		}
	}
	for id := range r.SourceCode {
		if !referenced[id] {
			delete(r.SourceCode, id)
		}
	}
	return true
}

func dropBulkAnnotations(r *ReportData) bool {
	changed := false
	for _, k := range []string{"Dependencies", "Environment Variables"} {
		if _, ok := r.Annotations[k]; ok {
			delete(r.Annotations, k)
			changed = true
		}
	}
	return changed
}

func trimErrorChain(r *ReportData) bool {
	chain, ok := r.Annotations["Error Chain"].([]errorChainLink)
	if !ok || len(chain) <= maxTrimmedErrorChain {
		return false
	}
	r.Annotations["Error Chain"] = chain[:maxTrimmedErrorChain]
	return true
}

func truncateLongAttributes(r *ReportData) bool {
	changed := false
	for k, v := range r.Attributes {
		if s, ok := v.(string); ok && len(s) > maxTrimmedAttributeBytes {
			r.Attributes[k] = truncateUTF8(s, maxTrimmedAttributeBytes)
			changed = true
		}
	}
	return changed
}

func dropAnnotations(r *ReportData) bool {
	if len(r.Annotations) == 0 {
		return false
	}
	r.Annotations = map[string]any{}
	return true
}

var uuidFallbackCounter atomic.Uint64

// uuid4 returns an RFC 4122 version 4 UUID from crypto/rand. If crypto/rand
// somehow fails (documented never to happen on supported platforms), the
// fallback derives distinct bytes from time, PID, and a counter rather than
// producing a process-wide constant.
func uuid4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		n := uuidFallbackCounter.Add(1)
		seed := fmt.Sprintf("%d:%d:%d", time.Now().UnixNano(), os.Getpid(), n)
		sum := sha256.Sum256([]byte(seed))
		copy(b[:], sum[:16])
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// errorChainLink describes one node in an unwrapped error graph.
type errorChainLink struct {
	ID       int    `json:"id"`
	ParentID *int   `json:"parentId,omitempty"`
	Source   string `json:"source,omitempty"`
	Type     string `json:"type"`
	Message  string `json:"message"`
}

// unwrapErrorChain walks err's unwrap graph — both Unwrap() error and
// Unwrap() []error (errors.Join) forms — bounded by depth and total node
// count, with cycle detection. All application-defined methods are invoked
// behind panic containment. A maxDepth < 0 disables capture.
func unwrapErrorChain(err error, maxDepth, maxNodes int) []errorChainLink {
	if err == nil || maxDepth < 0 || maxNodes < 1 {
		return nil
	}
	links := make([]errorChainLink, 0, 8)
	seen := make(map[string]struct{})

	var visit func(current error, depth int, parent *int, source string)
	visit = func(current error, depth int, parent *int, source string) {
		if current == nil || depth > maxDepth || len(links) >= maxNodes {
			return
		}
		key := errorVisitKey(current)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}

		id := len(links)
		links = append(links, errorChainLink{
			ID:       id,
			ParentID: parent,
			Source:   source,
			Type:     fmt.Sprintf("%T", current),
			Message:  safeErrorString(current),
		})
		parentID := id

		if many := safeUnwrapMany(current); many != nil {
			for i, child := range many {
				visit(child, depth+1, &parentID, fmt.Sprintf("errors[%d]", i))
			}
			return
		}
		visit(safeUnwrapOne(current), depth+1, &parentID, "unwrap")
	}

	visit(err, 0, nil, "")
	return links
}

// errorVisitKey identifies an error for cycle detection: pointer identity
// where available, type+message otherwise.
func errorVisitKey(err error) string {
	rv := reflect.ValueOf(err)
	if rv.IsValid() && rv.Kind() == reflect.Pointer && !rv.IsNil() {
		return fmt.Sprintf("%T@%x", err, rv.Pointer())
	}
	return fmt.Sprintf("%T:%s", err, safeErrorString(err))
}

func safeUnwrapOne(err error) (out error) {
	defer func() { _ = recover() }()
	if u, ok := err.(interface{ Unwrap() error }); ok {
		return u.Unwrap()
	}
	return nil
}

func safeUnwrapMany(err error) (out []error) {
	defer func() { _ = recover() }()
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		return append([]error(nil), u.Unwrap()...)
	}
	return nil
}
