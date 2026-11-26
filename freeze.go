package bt

import (
	"bytes"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
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
// goroutine, during the reporting call. IsZero methods, used for omitzero,
// run there too. Struct fields follow the rules of encoding/json v1.
type freezer struct {
	limits valueLimits
	nodes  int
	bytes  int
	// visiting tracks pointer, map and slice headers on the current path
	// for cycle detection.
	visiting []visitKey
}

// typ separates containers that share an address, such as a struct and its first field.
type visitKey struct {
	kind reflect.Kind
	typ  reflect.Type
	ptr  uintptr
	len  int
}

func (f *freezer) enter(k visitKey) bool {
	if slices.Contains(f.visiting, k) {
		return false
	}
	f.visiting = append(f.visiting, k)
	return true
}

func (f *freezer) leave() {
	f.visiting = f.visiting[:len(f.visiting)-1]
}

func newFreezer(limits valueLimits) *freezer {
	return &freezer{limits: limits.normalize()}
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
	return f.freezeWith(rv, depth, false)
}

// encoding/json applies the ",string" option behind pointers too.
func (f *freezer) freezeWith(rv reflect.Value, depth int, quoted bool) any {
	f.nodes++
	var declared reflect.Type
	for rv.IsValid() && rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		if rv.CanInterface() && rv.NumMethod() > 0 {
			declared = rv.Type()
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil
	}
	if depth > f.limits.maxDepth {
		return f.marker("value truncated: max depth")
	}
	if declared != nil {
		if v, ok := f.convertDeclared(rv, declared, depth); ok {
			return v
		}
	}
	if v, ok := f.convert(rv, depth, quoted); ok {
		return v
	}

	switch rv.Kind() {
	case reflect.Bool:
		if quoted {
			return f.freezeString(strconv.FormatBool(rv.Bool()))
		}
		f.bytes += scalarCost
		return rv.Bool()
	case reflect.String:
		if quoted {
			return f.freezeQuotedString(rv.String())
		}
		return f.freezeString(rv.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if quoted {
			return f.freezeString(strconv.FormatInt(rv.Int(), 10))
		}
		f.bytes += scalarCost
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if quoted {
			return f.freezeString(strconv.FormatUint(rv.Uint(), 10))
		}
		f.bytes += scalarCost
		return rv.Uint()
	case reflect.Float32:
		return f.freezeFloat(rv.Float(), 32, quoted)
	case reflect.Float64:
		return f.freezeFloat(rv.Float(), 64, quoted)
	case reflect.Pointer:
		if rv.IsNil() {
			return nil
		}
		k := visitKey{kind: reflect.Pointer, typ: rv.Type(), ptr: rv.Pointer()}
		if !f.enter(k) {
			return f.marker("cycle")
		}
		defer f.leave()
		return f.freezeWith(rv.Elem(), depth+1, quoted)
	case reflect.Slice:
		if rv.IsNil() {
			return nil
		}
		if isByteSlice(rv.Type()) {
			return f.freezeBytes(rv.Bytes())
		}
		ptr := rv.Pointer()
		if ptr != 0 && rv.Len() > 0 {
			k := visitKey{kind: reflect.Slice, typ: rv.Type(), ptr: ptr, len: rv.Len()}
			if !f.enter(k) {
				return f.marker("cycle")
			}
			defer f.leave()
		}
		return f.freezeList(rv, depth)
	case reflect.Array:
		return f.freezeList(rv, depth)
	case reflect.Map:
		if rv.IsNil() {
			return nil
		}
		k := visitKey{kind: reflect.Map, ptr: rv.Pointer()}
		if !f.enter(k) {
			return f.marker("cycle")
		}
		defer f.leave()
		return f.freezeMap(rv, depth)
	case reflect.Struct:
		return f.freezeStruct(rv, depth)
	default:
		// Funcs, channels, complex numbers and unsafe pointers have no
		// JSON form.
		return f.marker("unsupported value of type %s", rv.Type())
	}
}

// convert handles types that carry their own representation. It mirrors the
// precedence of encoding/json (Marshaler before TextMarshaler) and adds
// error values, which encoding/json would render as an empty object. A
// pointer receiver is used when the value is addressable.
func (f *freezer) convert(rv reflect.Value, depth int, quoted bool) (interface{}, bool) {
	if !rv.CanInterface() {
		return nil, false
	}
	t := rv.Type()
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		// A typed nil is null, exactly as encoding/json emits it; calling
		// its methods would only invite a nil-receiver panic.
		return nil, true
	}

	addressable := t.Kind() != reflect.Pointer && rv.CanAddr()
	switch {
	case t == timeType:
		return f.freezeString(rv.Interface().(time.Time).Format(time.RFC3339Nano)), true
	case t == jsonNumberType:
		s := rv.String()
		if s == "" {
			s = "0"
		}
		if quoted {
			return f.freezeString(s), true
		}
		return f.freezeNumber(s), true
	case addressable && reflect.PointerTo(t).Implements(jsonMarshalerType):
		return f.fromJSONMarshaler(rv.Addr(), depth), true
	case t.Implements(jsonMarshalerType):
		return f.fromJSONMarshaler(rv, depth), true
	case addressable && reflect.PointerTo(t).Implements(textMarshalerType):
		return f.fromTextMarshaler(rv.Addr()), true
	case t.Implements(textMarshalerType):
		return f.fromTextMarshaler(rv), true
	case t.Implements(errorType):
		return f.freezeString(safeErrorString(rv.Interface().(error))), true
	}
	return nil, false
}

// encoding/json v1 takes the method from the declared interface type and calls it on a nil pointer too.
func (f *freezer) convertDeclared(rv reflect.Value, declared reflect.Type, depth int) (any, bool) {
	switch {
	case declared.Implements(jsonMarshalerType):
		if rv.Kind() == reflect.Pointer && rv.IsNil() {
			return f.fromJSONMarshaler(rv, depth), true
		}
	case declared.Implements(textMarshalerType):
		return f.fromTextMarshaler(rv), true
	}
	return nil, false
}

func (f *freezer) fromTextMarshaler(rv reflect.Value) any {
	text, ok := safeMarshalText(rv.Interface().(encoding.TextMarshaler))
	if !ok {
		return f.marker("MarshalText failed for %s", rv.Type())
	}
	return f.freezeString(text)
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

func (f *freezer) freezeFloat(x float64, bits int, quoted bool) interface{} {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		// Not representable in JSON.
		return f.marker("non-finite float %v", x)
	}
	if quoted {
		v := any(x)
		if bits == 32 {
			v = float32(x)
		}
		text, _ := json.Marshal(v)
		return f.freezeString(string(text))
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

func (f *freezer) freezeQuotedString(s string) string {
	if len(s) > f.limits.maxStringBytes {
		s = truncateUTF8(s, f.limits.maxStringBytes)
	}
	quoted, _ := json.Marshal(s)
	return f.freezeString(string(quoted))
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
// Integer keys are written in decimal, and a nil TextMarshaler pointer key
// is "".
func freezeMapKey(k reflect.Value) string {
	if k.Kind() == reflect.String {
		return k.String()
	}
	if k.CanInterface() {
		if tm, ok := k.Interface().(encoding.TextMarshaler); ok {
			if k.Kind() == reflect.Pointer && k.IsNil() {
				return ""
			}
			if text, ok := safeMarshalText(tm); ok {
				return text
			}
		}
		switch k.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return strconv.FormatInt(k.Int(), 10)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			return strconv.FormatUint(k.Uint(), 10)
		}
		return safeSprint(k.Interface())
	}
	return fmt.Sprintf("<%s>", k.Type())
}

func (f *freezer) freezeStruct(rv reflect.Value, depth int) map[string]any {
	fields := cachedStructFields(rv.Type())
	out := make(map[string]any, len(fields))
	mark := len(f.visiting)
	for i := range fields {
		sf := &fields[i]
		value, ok, cycle := f.enterField(rv, sf.index)
		if ok && !sf.omit(value) {
			if f.exhausted() {
				f.visiting = f.visiting[:mark]
				out[truncatedKey] = f.marker("fields of %s omitted", rv.Type())
				break
			}
			f.bytes += len(sf.name)
			if cycle {
				out[sf.name] = f.marker("cycle")
			} else {
				out[sf.name] = f.freezeWith(value, depth+1, sf.quoted)
			}
		}
		f.visiting = f.visiting[:mark]
	}
	return out
}

// The embedded pointers it passes stay on f.visiting until the caller cuts it back.
func (f *freezer) enterField(v reflect.Value, index []int) (field reflect.Value, ok, cycle bool) {
	for _, i := range index {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false, cycle // nil embedded struct pointer: nothing to promote
			}
			if !f.enter(visitKey{kind: reflect.Pointer, typ: v.Type(), ptr: v.Pointer()}) {
				cycle = true
			}
			v = v.Elem()
		}
		v = v.Field(i)
	}
	return v, true, cycle
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
