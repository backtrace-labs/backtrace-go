package bt

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// uuidLike is a [16]byte with a text form, like the common UUID types.
type uuidLike [16]byte

func (u uuidLike) MarshalText() ([]byte, error) {
	return []byte(fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])), nil
}

type panickyJSON struct{}

func (panickyJSON) MarshalJSON() ([]byte, error) { panic("MarshalJSON exploded") }

type panickyText struct{}

func (panickyText) MarshalText() ([]byte, error) { panic("MarshalText exploded") }

type explodingError struct{}

func (*explodingError) Error() string { panic("Error exploded") }

type freezeEmbedded struct {
	Promoted string
}

type freezeStruct struct {
	freezeEmbedded
	Name     string `json:"name"`
	Skipped  string `json:"-"`
	Empty    string `json:"empty,omitempty"`
	Count    int
	When     time.Time `json:"when"`
	hidden   string
	Children []string `json:"children"`
}

type (
	namedString string
	namedInt    int
)

// assertModel fails unless v contains only the documented model types.
func assertModel(t *testing.T, path string, v interface{}) {
	t.Helper()
	switch x := v.(type) {
	case nil, bool, string, int64, uint64, float64:
	case []interface{}:
		for i, e := range x {
			assertModel(t, fmt.Sprintf("%s[%d]", path, i), e)
		}
	case map[string]interface{}:
		for k, e := range x {
			assertModel(t, path+"."+k, e)
		}
	default:
		t.Errorf("%s: %T is not part of the value model", path, v)
	}
}

// TestFreezeConversions pins what each common Go value becomes. Before the
// type-aware pass these degraded to placeholders, base64 or number arrays.
func TestFreezeConversions(t *testing.T) {
	when := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	var nilPtr *freezeStruct

	cases := []struct {
		name string
		in   interface{}
		want interface{}
	}{
		{"time.Time", when, "2026-11-18T10:00:00Z"},
		{"*time.Time", &when, "2026-11-18T10:00:00Z"},
		{"time.Duration stays numeric", 1500 * time.Millisecond, int64(1500000000)},
		{"error", errors.New("boom"), "boom"},
		{"wrapped error", fmt.Errorf("wrap: %w", errors.New("inner")), "wrap: inner"},
		{"net.IP", net.IPv4(192, 168, 0, 1), "192.168.0.1"},
		{"netip.Addr", netip.MustParseAddr("10.0.0.1"), "10.0.0.1"},
		{"uuid-like array", uuidLike{0xde, 0xad, 0xbe, 0xef}, "deadbeef-0000-0000-0000-000000000000"},
		{"text-marshalling int", slog.LevelInfo, "INFO"},
		{"plain byte array", [3]byte{1, 2, 3}, []interface{}{uint64(1), uint64(2), uint64(3)}},
		{"byte slice", []byte("abc"), "YWJj"},
		{"raw JSON", json.RawMessage(`{"a":1,"b":[true,null]}`),
			map[string]interface{}{"a": int64(1), "b": []interface{}{true, nil}}},
		{"invalid raw JSON", json.RawMessage(`not json`), "not json"},
		{"typed nil pointer", nilPtr, nil},
		{"named string", namedString("x"), "x"},
		{"named int", namedInt(7), int64(7)},
		{"int map keys", map[int]string{1: "a"}, map[string]interface{}{"1": "a"}},
		{"string map", map[string]string{"k": "v"}, map[string]interface{}{"k": "v"}},
		{"string slice", []string{"a", "b"}, []interface{}{"a", "b"}},
		{"large uint64", uint64(1) << 63, uint64(1) << 63},
		{"float32 keeps its value", float32(0.1), 0.1},
		{"nil slice", []string(nil), nil},
		{"nil map", map[string]int(nil), nil},
		{"panicking MarshalJSON", panickyJSON{}, "[bt: MarshalJSON failed for bt.panickyJSON]"},
		{"panicking MarshalText", panickyText{}, "[bt: MarshalText failed for bt.panickyText]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := freezeValue(tc.in, valueLimits{})
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
			assertModel(t, tc.name, got)
		})
	}

	for name, v := range map[string]interface{}{"chan": make(chan int), "func": func() {}, "complex": complex(1, 2)} {
		got, _ := freezeValue(v, valueLimits{}).(string)
		if !strings.Contains(got, "unsupported value") {
			t.Errorf("%s = %q, want an unsupported-value marker", name, got)
		}
	}
	if got, _ := freezeValue(&explodingError{}, valueLimits{}).(string); !strings.Contains(got, "panicked") {
		t.Errorf("panicking Error() = %q, want the contained form", got)
	}
}

// TestFreezeStructFields pins that structs follow encoding/json naming.
func TestFreezeStructFields(t *testing.T) {
	in := freezeStruct{
		freezeEmbedded: freezeEmbedded{Promoted: "up"},
		Name:           "n",
		Skipped:        "never",
		Count:          3,
		When:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		hidden:         "never",
		Children:       []string{"c"},
	}
	want := map[string]interface{}{
		"Promoted": "up",
		"name":     "n",
		"Count":    int64(3),
		"when":     "2026-01-02T03:04:05Z",
		"children": []interface{}{"c"},
	}
	for _, v := range []interface{}{in, &in} {
		if got := freezeValue(v, valueLimits{}); !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	}
}

// TestFreezeMatchesEncodingJSON pins parity: for values encoding/json can
// render, normalising first must not change the JSON that is submitted.
func TestFreezeMatchesEncodingJSON(t *testing.T) {
	when := time.Date(2026, 11, 18, 10, 0, 0, 123456789, time.UTC)
	values := []interface{}{
		when, &when, net.ParseIP("2001:db8::1"), netip.MustParseAddr("10.1.2.3"),
		uuidLike{1, 2, 3}, slog.LevelError, []byte{0, 1, 2, 250}, [2]int{4, 5},
		json.RawMessage(`[1,"two",{"three":3.5}]`),
		freezeStruct{Name: "s", Children: []string{}},
		&freezeStruct{Name: "p", Empty: "set"},
		map[string]interface{}{"n": 1, "s": []string{"x"}, "m": map[string]bool{"t": true}},
		map[int]string{2: "b", 1: "a"},
		[]interface{}{nil, true, "s", 1.5, float32(0.25), uint8(9), int8(-3)},
		namedString("ns"), namedInt(-4), "plain", 42, true, nil,
	}
	for _, v := range values {
		direct, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%T: encoding/json failed: %v", v, err)
		}
		frozen, err := json.Marshal(freezeValue(v, valueLimits{}))
		if err != nil {
			t.Fatalf("%T: normalised value is not marshalable: %v", v, err)
		}
		var a, b interface{}
		_ = json.Unmarshal(direct, &a)
		_ = json.Unmarshal(frozen, &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%T: JSON changed\n direct: %s\n frozen: %s", v, direct, frozen)
		}
	}
}

// TestFreezeBoundsWork pins that the caps stop the walk instead of
// replacing every remaining element: an oversized value must cost what the
// budget allows, not what the value holds.
func TestFreezeBoundsWork(t *testing.T) {
	t.Run("huge slice", func(t *testing.T) {
		big := make([]int, 5_000_000)
		out := freezeValue(big, valueLimits{}).([]interface{})
		if len(out) > defaultFreezeMaxNodes+1 {
			t.Fatalf("visited %d elements, cap is %d", len(out), defaultFreezeMaxNodes)
		}
		last, _ := out[len(out)-1].(string)
		if !strings.Contains(last, "elements omitted") {
			t.Errorf("no single truncation marker at the end: %v", out[len(out)-1])
		}
		for _, e := range out[:len(out)-1] {
			if _, isMarker := e.(string); isMarker {
				t.Fatal("a marker per element: the cap did not stop iteration")
			}
		}
		if allocs := testing.AllocsPerRun(3, func() { freezeValue(big, valueLimits{}) }); allocs > 4*defaultFreezeMaxNodes {
			t.Errorf("%.0f allocations for a capped walk", allocs)
		}
	})

	t.Run("huge map", func(t *testing.T) {
		big := make(map[int]int, 300_000)
		for i := 0; i < 300_000; i++ {
			big[i] = i
		}
		out := freezeValue(big, valueLimits{}).(map[string]interface{})
		if len(out) > defaultFreezeMaxNodes+1 {
			t.Fatalf("collected %d keys, cap is %d", len(out), defaultFreezeMaxNodes)
		}
		if _, ok := out[truncatedKey]; !ok {
			t.Error("truncated map carries no marker entry")
		}
	})

	t.Run("huge byte slice is not copied", func(t *testing.T) {
		big := make([]byte, 64<<20)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		out := freezeValue(big, valueLimits{})
		runtime.ReadMemStats(&after)
		if s, _ := out.(string); !strings.Contains(s, "bytes omitted") {
			t.Errorf("got %T, want an omission marker", out)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
			t.Errorf("allocated %d bytes for a byte slice over the cap", grew)
		}
	})

	t.Run("scalars count against the byte budget", func(t *testing.T) {
		out := freezeValue(make([]int, 10_000), valueLimits{maxTotalBytes: 1024}).([]interface{})
		if len(out) > 1024/scalarCost+1 {
			t.Errorf("%d numbers kept under a 1 KiB budget", len(out))
		}
	})

	t.Run("shared budget is deterministic", func(t *testing.T) {
		attrs := make(map[string]interface{}, 5000)
		for i := 0; i < 5000; i++ {
			attrs[fmt.Sprintf("k%05d", i)] = i
		}
		first := freezeAttributes(attrs, valueLimits{})
		second := freezeAttributes(attrs, valueLimits{})
		if len(first) > defaultFreezeMaxNodes+1 {
			t.Fatalf("%d keys kept, cap is %d", len(first), defaultFreezeMaxNodes)
		}
		if _, ok := first[truncatedKey]; !ok {
			t.Error("no marker entry for the omitted keys")
		}
		if _, ok := first["k00000"]; !ok {
			t.Error("keys are not visited in sorted order")
		}
		if !reflect.DeepEqual(first, second) {
			t.Error("the surviving key set differs between two calls")
		}
	})
}

func TestFreezeDetachesAndTerminates(t *testing.T) {
	limits := valueLimits{}

	inner := []interface{}{1, 2}
	outer := map[string]interface{}{"list": inner}
	frozen := freezeValue(outer, limits).(map[string]interface{})
	inner[0] = 99
	if got := frozen["list"].([]interface{})[0]; got != int64(1) {
		t.Errorf("slice not detached: %v", got)
	}

	for _, x := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if v, _ := freezeValue(x, limits).(string); !strings.Contains(v, "non-finite") {
			t.Errorf("%v = %q, want a marker", x, v)
		}
	}

	// Cycles through a map, a slice and a pointer all terminate.
	cyclicMap := map[string]interface{}{}
	cyclicMap["self"] = cyclicMap
	cyclicSlice := make([]interface{}, 1)
	cyclicSlice[0] = cyclicSlice
	type node struct{ Next *node }
	cyclicPtr := &node{}
	cyclicPtr.Next = cyclicPtr
	for _, v := range []interface{}{cyclicMap, cyclicSlice, cyclicPtr} {
		out := freezeValue(v, limits)
		assertModel(t, "cycle", out)
		if _, err := json.Marshal(out); err != nil {
			t.Errorf("%T: cyclic value is not JSON-safe after normalising: %v", v, err)
		}
	}

	deep := map[string]interface{}{}
	cur := deep
	for i := 0; i < 100; i++ {
		next := map[string]interface{}{}
		cur["next"] = next
		cur = next
	}
	if _, err := json.Marshal(freezeValue(deep, limits)); err != nil {
		t.Errorf("deep value is not JSON-safe after normalising: %v", err)
	}

	// Strings are clipped on a rune boundary.
	long := strings.Repeat("é", 1000) // two bytes per rune
	for max := 20; max < 40; max++ {
		clipped := freezeValue(long, valueLimits{maxStringBytes: max}).(string)
		if len(clipped) > max {
			t.Errorf("cap %d: got %d bytes", max, len(clipped))
		}
		if !utf8.ValidString(clipped) {
			t.Errorf("cap %d: clipped in the middle of a rune", max)
		}
	}
}
