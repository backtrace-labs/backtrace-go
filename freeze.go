package bt

import (
	"bytes"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// valueLimits bounds one normalisation pass. Zero fields fall back to the
// package defaults below.
type valueLimits struct {
	maxDepth       int
	maxNodes       int
	maxStringBytes int
	maxTotalBytes  int
}

const (
	defaultFreezeMaxDepth       = 32
	defaultFreezeMaxNodes       = 2048
	defaultFreezeMaxStringBytes = 64 << 10
	defaultFreezeMaxTotalBytes  = 1 << 20

	// truncatedMarker is appended to strings clipped at a byte cap.
	truncatedMarker = "…[truncated]"

	// truncatedKey is the single entry that replaces the remaining keys of
	// a map once the budget is spent.
	truncatedKey = "[bt: truncated]"

	// scalarCost is charged against the byte budget for every number and
	// boolean, so floods of scalars are bounded like strings are.
	scalarCost = 8
)

func (l valueLimits) normalize() valueLimits {
	if l.maxDepth <= 0 {
		l.maxDepth = defaultFreezeMaxDepth
	}
	if l.maxNodes <= 0 {
		l.maxNodes = defaultFreezeMaxNodes
	}
	if l.maxStringBytes <= 0 {
		l.maxStringBytes = defaultFreezeMaxStringBytes
	}
	if l.maxTotalBytes <= 0 {
		l.maxTotalBytes = defaultFreezeMaxTotalBytes
	}
	return l
}

var (
	timeType          = reflect.TypeOf(time.Time{})
	jsonNumberType    = reflect.TypeOf(json.Number(""))
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	errorType         = reflect.TypeOf((*error)(nil)).Elem()
)

// freezer turns caller-owned values into a detached, JSON-native graph on
// the calling goroutine.
//
// The result contains ONLY these types: nil, bool, string, int64, uint64,
// float64, []interface{} and map[string]interface{}. That is the value model
// the delivery worker serializes and the model a BeforeSend hook observes.
//
// Two properties matter:
//
//   - Ownership. Nothing in the result aliases caller memory, so the worker
//     never serializes a map or slice the caller can still mutate (the
//     runtime's concurrent map access failure is fatal and unrecoverable).
//   - Bounded work. Depth, node count and bytes are capped while walking,
//     not after: an oversized collection stops being visited as soon as the
//     budget is spent and is closed with a single marker.
//
// Types that define their own JSON or text form (json.Marshaler,
// encoding.TextMarshaler) and error values are converted through those
// methods, each call guarded against panics, so common values such as
// time.Time, net.IP or a UUID keep the representation encoding/json gives
// them. Those methods are application code and run on the caller's
// goroutine, during the reporting call.
type freezer struct {
	limits valueLimits
	nodes  int
	bytes  int
	// visiting tracks pointer, map and slice headers on the current path
	// for cycle detection.
	visiting map[uintptr]bool
}

func newFreezer(limits valueLimits) *freezer {
	return &freezer{limits: limits.normalize(), visiting: map[uintptr]bool{}}
}

// freezeAttributes normalises every value of in under ONE shared budget.
// Keys are visited in sorted order so that, when the budget runs out, the
// same keys survive on every call; the rest are replaced by a single
// truncatedKey entry.
func freezeAttributes(in map[string]interface{}, limits valueLimits) map[string]interface{} {
	if in == nil {
		return nil
	}
	f := newFreezer(limits)
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]interface{}, len(keys))
	for i, k := range keys {
		if f.exhausted() {
			out[truncatedKey] = f.marker("%d of %d keys omitted", len(keys)-i, len(keys))
			break
		}
		out[k] = f.freeze(reflect.ValueOf(in[k]), 0)
	}
	return out
}

// freezeValue normalises a single value under its own budget.
func freezeValue(v interface{}, limits valueLimits) interface{} {
	return newFreezer(limits).freeze(reflect.ValueOf(v), 0)
}

// exhausted reports whether the node or byte budget is spent. Collections
// check it before every element and stop instead of visiting the rest.
func (f *freezer) exhausted() bool {
	return f.nodes >= f.limits.maxNodes || f.bytes >= f.limits.maxTotalBytes
}

// marker builds a "[bt: …]" note and charges it against the byte budget.
func (f *freezer) marker(format string, a ...interface{}) string {
	s := fmt.Sprintf("[bt: "+format+"]", a...)
	f.bytes += len(s)
	return s
}

func (f *freezer) freeze(rv reflect.Value, depth int) interface{} {
	f.nodes++
	for rv.IsValid() && rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil
	}
	if depth > f.limits.maxDepth {
		return f.marker("value truncated: max depth")
	}
	if v, ok := f.convert(rv, depth); ok {
		return v
	}

	switch rv.Kind() {
	case reflect.Bool:
		f.bytes += scalarCost
		return rv.Bool()
	case reflect.String:
		return f.freezeString(rv.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f.bytes += scalarCost
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		f.bytes += scalarCost
		return rv.Uint()
	case reflect.Float32:
		return f.freezeFloat(rv.Float(), 32)
	case reflect.Float64:
		return f.freezeFloat(rv.Float(), 64)
	case reflect.Pointer:
		if rv.IsNil() {
			return nil
		}
		ptr := rv.Pointer()
		if f.visiting[ptr] {
			return f.marker("cycle")
		}
		f.visiting[ptr] = true
		defer delete(f.visiting, ptr)
		return f.freeze(rv.Elem(), depth+1)
	case reflect.Slice:
		if rv.IsNil() {
			return nil
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return f.freezeBytes(rv.Bytes())
		}
		ptr := rv.Pointer()
		if ptr != 0 && rv.Len() > 0 {
			if f.visiting[ptr] {
				return f.marker("cycle")
			}
			f.visiting[ptr] = true
			defer delete(f.visiting, ptr)
		}
		return f.freezeList(rv, depth)
	case reflect.Array:
		return f.freezeList(rv, depth)
	case reflect.Map:
		if rv.IsNil() {
			return nil
		}
		ptr := rv.Pointer()
		if f.visiting[ptr] {
			return f.marker("cycle")
		}
		f.visiting[ptr] = true
		defer delete(f.visiting, ptr)
		return f.freezeMap(rv, depth)
	case reflect.Struct:
		out := map[string]interface{}{}
		f.freezeStructInto(out, rv, depth)
		return out
	default:
		// Funcs, channels, complex numbers and unsafe pointers have no
		// JSON form.
		return f.marker("unsupported value of type %s", rv.Type())
	}
}

// convert handles types that carry their own representation. It mirrors the
// precedence of encoding/json (Marshaler before TextMarshaler) and adds
// error values, which encoding/json would render as an empty object.
func (f *freezer) convert(rv reflect.Value, depth int) (interface{}, bool) {
	if !rv.CanInterface() {
		return nil, false
	}
	t := rv.Type()
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		// A typed nil is null, exactly as encoding/json emits it; calling
		// its methods would only invite a nil-receiver panic.
		return nil, true
	}

	switch {
	case t == timeType:
		return f.freezeString(rv.Interface().(time.Time).Format(time.RFC3339Nano)), true
	case t == jsonNumberType:
		return f.freezeNumber(rv.String()), true
	case t.Implements(jsonMarshalerType):
		return f.fromJSONMarshaler(rv, depth), true
	case t.Implements(textMarshalerType):
		text, ok := safeMarshalText(rv.Interface().(encoding.TextMarshaler))
		if !ok {
			return f.marker("MarshalText failed for %s", t), true
		}
		return f.freezeString(text), true
	case t.Implements(errorType):
		return f.freezeString(safeErrorString(rv.Interface().(error))), true
	}
	return nil, false
}

// fromJSONMarshaler runs a user MarshalJSON under panic containment, then
// decodes its output back into plain values under the same budget, so the
// result obeys the value model regardless of what the method produced.
func (f *freezer) fromJSONMarshaler(rv reflect.Value, depth int) interface{} {
	t := rv.Type()
	raw, ok := safeMarshalJSON(rv.Interface().(json.Marshaler))
	if !ok {
		return f.marker("MarshalJSON failed for %s", t)
	}
	if remaining := f.limits.maxTotalBytes - f.bytes; len(raw) > remaining {
		return f.marker("value of type %s omitted: %d bytes exceed the budget", t, len(raw))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded interface{}
	if err := dec.Decode(&decoded); err != nil {
		// Not valid JSON (for example a hand-built json.RawMessage): keep
		// the text rather than failing the whole report later.
		return f.freezeString(string(raw))
	}
	// The decoded graph is SDK-owned and acyclic; walking it applies the
	// node and byte accounting and converts json.Number.
	return f.freeze(reflect.ValueOf(decoded), depth+1)
}

func safeMarshalJSON(m json.Marshaler) (raw []byte, ok bool) {
	defer func() {
		if recover() != nil {
			raw, ok = nil, false
		}
	}()
	raw, err := m.MarshalJSON()
	return raw, err == nil
}

func safeMarshalText(m encoding.TextMarshaler) (text string, ok bool) {
	defer func() {
		if recover() != nil {
			text, ok = "", false
		}
	}()
	raw, err := m.MarshalText()
	return string(raw), err == nil
}

// freezeNumber converts a decoded JSON number to the narrowest model type.
func (f *freezer) freezeNumber(s string) interface{} {
	f.bytes += scalarCost
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if u, err := strconv.ParseUint(s, 10, 64); err == nil {
		return u
	}
	if x, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(x) && !math.IsInf(x, 0) {
		return x
	}
	return f.freezeString(s)
}

func (f *freezer) freezeFloat(x float64, bits int) interface{} {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		// Not representable in JSON.
		return f.marker("non-finite float %v", x)
	}
	f.bytes += scalarCost
	if bits == 32 {
		// reflect widens float32 to float64 (0.1 becomes
		// 0.10000000149011612). Round-trip through the shortest float32
		// form so the wire value is what the caller wrote.
		if exact, err := strconv.ParseFloat(strconv.FormatFloat(x, 'g', -1, 32), 64); err == nil {
			return exact
		}
	}
	return x
}

// freezeString clips s to the per-string cap and to what is left of the
// total budget, always on a rune boundary.
func (f *freezer) freezeString(s string) string {
	allowed := f.limits.maxStringBytes
	if remaining := f.limits.maxTotalBytes - f.bytes; remaining < allowed {
		allowed = remaining
	}
	if len(s) > allowed {
		s = clipString(s, allowed)
	}
	f.bytes += len(s)
	return s
}

// clipString shortens s to at most max bytes including the truncation
// marker, never splitting a UTF-8 sequence.
func clipString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= len(truncatedMarker) {
		return truncatedMarker
	}
	cut := max - len(truncatedMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMarker
}

// freezeBytes renders a byte slice the way encoding/json does (standard
// base64). The size is checked BEFORE anything is copied or encoded.
func (f *freezer) freezeBytes(b []byte) interface{} {
	encoded := base64.StdEncoding.EncodedLen(len(b))
	allowed := f.limits.maxStringBytes
	if remaining := f.limits.maxTotalBytes - f.bytes; remaining < allowed {
		allowed = remaining
	}
	if encoded > allowed {
		return f.marker("%d bytes omitted", len(b))
	}
	f.bytes += encoded
	return base64.StdEncoding.EncodeToString(b)
}

// freezeList copies a slice or array, stopping at the budget.
func (f *freezer) freezeList(rv reflect.Value, depth int) interface{} {
	n := rv.Len()
	room := n
	if left := f.limits.maxNodes - f.nodes; left < room {
		room = left
	}
	if room < 0 {
		room = 0
	}
	out := make([]interface{}, 0, room+1)
	for i := 0; i < n; i++ {
		if f.exhausted() {
			out = append(out, f.marker("%d of %d elements omitted", n-i, n))
			break
		}
		out = append(out, f.freeze(rv.Index(i), depth+1))
	}
	return out
}

// freezeMap copies a map with string keys in sorted order. When the map is
// larger than the remaining node budget only that many entries are collected
// in the first place, so a huge map is never enumerated or sorted in full.
func (f *freezer) freezeMap(rv reflect.Value, depth int) interface{} {
	type entry struct {
		key   string
		value reflect.Value
	}
	total := rv.Len()
	room := f.limits.maxNodes - f.nodes
	if room < 0 {
		room = 0
	}
	take := total
	if take > room {
		take = room
	}

	entries := make([]entry, 0, take)
	iter := rv.MapRange()
	for len(entries) < take && iter.Next() {
		entries = append(entries, entry{freezeMapKey(iter.Key()), iter.Value()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })

	out := make(map[string]interface{}, len(entries)+1)
	done := 0
	for _, e := range entries {
		if f.exhausted() {
			break
		}
		key := clipString(e.key, f.limits.maxStringBytes)
		f.bytes += len(key)
		out[key] = f.freeze(e.value, depth+1)
		done++
	}
	if done < total {
		out[truncatedKey] = f.marker("%d of %d keys omitted", total-done, total)
	}
	return out
}

// freezeMapKey renders a map key as a JSON object key. It follows
// encoding/json: string kinds as they are, TextMarshaler keys through
// MarshalText, everything else formatted (fmt recovers a panicking String).
func freezeMapKey(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	if k.CanInterface() {
		if tm, ok := k.Interface().(encoding.TextMarshaler); ok {
			if text, ok := safeMarshalText(tm); ok {
				return text
			}
		}
		return safeSprint(k.Interface())
	}
	return fmt.Sprintf("<%s>", k.Type())
}

// freezeStructInto adds the exported fields of a struct to out, named as
// encoding/json would name them: the json tag when present, "-" skipped,
// omitempty honoured, untagged embedded structs flattened.
func (f *freezer) freezeStructInto(out map[string]interface{}, rv reflect.Value, depth int) {
	t := rv.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag, tagged := field.Tag.Lookup("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		value := rv.Field(i)

		if field.Anonymous && (!tagged || name == "") {
			embedded := value
			for embedded.Kind() == reflect.Pointer && !embedded.IsNil() {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Pointer && embedded.Type().Elem().Kind() == reflect.Struct {
				continue // nil embedded struct pointer: nothing to promote
			}
			if embedded.Kind() == reflect.Struct && embedded.Type() != timeType {
				f.freezeStructInto(out, embedded, depth)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if strings.Contains(","+opts+",", ",omitempty,") && value.IsZero() {
			continue
		}
		if f.exhausted() {
			out[truncatedKey] = f.marker("fields of %s omitted", t)
			return
		}
		f.bytes += len(name)
		out[name] = f.freeze(value, depth+1)
	}
}

// copyModel returns a deep copy of a value-model graph. Only []interface{}
// and map[string]interface{} are composite there, so no reflection is
// needed. The plain Go numbers the SDK itself puts into attributes (an int
// process id, a uint32 counter) are widened on the way, so that the copy
// holds model types only.
func copyModel(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		return copyModelMap(x)
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, e := range x {
			out[i] = copyModel(e)
		}
		return out
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case uint:
		return uint64(x)
	case uint8:
		return uint64(x)
	case uint16:
		return uint64(x)
	case uint32:
		return uint64(x)
	case uintptr:
		return uint64(x)
	case float32:
		// Through the shortest decimal form, as the normaliser does, so
		// 0.1 stays 0.1.
		if f, err := strconv.ParseFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), 64); err == nil {
			return f
		}
		return float64(x)
	default:
		return v
	}
}

// copyModelMap is copyModel for an attribute map; nil stays nil.
func copyModelMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = copyModel(v)
	}
	return out
}
