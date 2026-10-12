package bt

import "fmt"

const formattingPanicPlaceholder = "value panicked while being formatted"

// safeSprint is used at every boundary that may execute application-defined
// String or Format methods. It must never call the diagnostic logger: the
// logger is application code too.
func safeSprint(v interface{}) (out string) {
	defer func() {
		if recover() != nil {
			out = fmt.Sprintf("<%T: %s>", v, formattingPanicPlaceholder)
		}
	}()
	return fmt.Sprint(v)
}

// safeErrorString is the error-specific equivalent of safeSprint. A typed-nil
// error is a non-nil interface and may panic in Error(); this helper contains
// that case.
func safeErrorString(err error) (out string) {
	if err == nil {
		return ""
	}
	defer func() {
		if recover() != nil {
			out = fmt.Sprintf("<%T: Error() panicked>", err)
		}
	}()
	return err.Error()
}

// cloneStringSlice detaches the slice header from caller ownership.
func cloneStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}

// cloneAnyMap detaches the map header from caller ownership. Nested reference
// values are documented as immutable after the reporting call returns.
func cloneAnyMap(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
