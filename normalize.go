package bt

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"unicode/utf8"
)

const (
	maxErrorMessageBytes = 1024
	maxExactFloatInt     = 1 << 53
	maxPrintDepth        = 32
	maxPrintNodes        = 10000
	messageAnnotation    = "Message"
)

type valueKind int

const (
	valueScalar valueKind = iota
	valueNil
	valueComposite
)

func scalarValue(v any) (any, valueKind) {
	switch x := v.(type) {
	case nil:
		return nil, valueNil
	case string, bool:
		return x, valueScalar
	case int64:
		return signedValue(x), valueScalar
	case uint64:
		return unsignedValue(x), valueScalar
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return freezeValue(x, valueLimits{}), valueScalar
		}
		return floatValue(x), valueScalar
	case []any, map[string]any:
		return nil, valueComposite
	}
	return scalarValue(freezeValue(v, valueLimits{}))
}

func signedValue(n int64) any {
	if n >= 0 {
		return n
	}
	return strconv.FormatInt(n, 10)
}

func unsignedValue(n uint64) any {
	if n <= math.MaxInt64 {
		return n
	}
	return strconv.FormatUint(n, 10)
}

func floatValue(f float64) any {
	if f >= 0 && f < maxExactFloatInt && f == math.Trunc(f) {
		return int64(f)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func normalizeAttributes(attrs, annotations map[string]any) {
	for k, v := range attrs {
		out, kind := scalarValue(v)
		switch kind {
		case valueNil:
			delete(attrs, k)
		case valueComposite:
			delete(attrs, k)
			if _, exists := annotations[k]; !exists {
				annotations[k] = encodeComposite(v)
			}
		default:
			attrs[k] = out
		}
	}
	if msg, ok := attrs["error.message"].(string); ok && len(msg) > maxErrorMessageBytes {
		attrs["error.message"] = truncateUTF8(msg, maxErrorMessageBytes)
		if _, exists := annotations[messageAnnotation]; !exists {
			annotations[messageAnnotation] = msg
		}
	}
}

func encodeComposite(v any) json.RawMessage {
	data, _ := json.Marshal(freezeValue(v, valueLimits{}))
	return data
}

func printValue(v any) string {
	// fmt has no cycle detection: a self-referencing map would overflow the stack.
	budget := maxPrintNodes
	if !boundedValue(reflect.ValueOf(v), 0, &budget) {
		return fmt.Sprintf("<%T: cyclic or too large to print>", v)
	}
	return safeSprint(v)
}

// Follows fmt: values with Format, Error or String methods and pointers below the top level are not walked.
func boundedValue(rv reflect.Value, depth int, budget *int) bool {
	*budget--
	if *budget < 0 || depth > maxPrintDepth {
		return false
	}
	if rv.IsValid() && rv.CanInterface() {
		switch rv.Interface().(type) {
		case fmt.Formatter, error, fmt.Stringer:
			return true
		}
	}
	switch rv.Kind() {
	case reflect.Pointer:
		if depth > 0 || rv.IsNil() {
			return true
		}
		switch rv.Elem().Kind() {
		case reflect.Array, reflect.Slice, reflect.Struct, reflect.Map:
			return boundedValue(rv.Elem(), depth+1, budget)
		}
	case reflect.Interface:
		if rv.IsNil() {
			return true
		}
		return boundedValue(rv.Elem(), depth+1, budget)
	case reflect.Map:
		if flatKind(rv.Type().Key().Kind()) && flatKind(rv.Type().Elem().Kind()) {
			return true
		}
		iter := rv.MapRange()
		for iter.Next() {
			if !boundedValue(iter.Key(), depth+1, budget) || !boundedValue(iter.Value(), depth+1, budget) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		if flatKind(rv.Type().Elem().Kind()) {
			return true
		}
		for i := 0; i < rv.Len(); i++ {
			if !boundedValue(rv.Index(i), depth+1, budget) {
				return false
			}
		}
	case reflect.Struct:
		for i := 0; i < rv.NumField(); i++ {
			if !boundedValue(rv.Field(i), depth+1, budget) {
				return false
			}
		}
	}
	return true
}

func flatKind(k reflect.Kind) bool {
	switch k {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Array, reflect.Struct:
		return false
	}
	return true
}

func encodeAnnotations(annotations map[string]any) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(annotations))
	for k, v := range annotations {
		out[k] = encodeAnnotation(v)
	}
	return out
}

func encodeAnnotation(v any) (out json.RawMessage) {
	defer func() {
		if recover() != nil {
			out = annotationPlaceholder(v)
		}
	}()
	data, err := json.Marshal(v)
	if err != nil {
		return annotationPlaceholder(v)
	}
	return data
}

func annotationPlaceholder(v any) json.RawMessage {
	data, _ := json.Marshal(fmt.Sprintf("<%T: not JSON-encodable>", v))
	return data
}

func truncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for i := 0; i < utf8.UTFMax && cut > 0 && !utf8.RuneStart(s[cut]); i++ {
		cut--
	}
	return s[:cut]
}
