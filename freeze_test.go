package bt

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"math/rand/v2"
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
		assertFreezeParity(t, fmt.Sprintf("%T", v), v, "")
	}
}

var generousLimits = valueLimits{maxDepth: 64, maxNodes: 1 << 20, maxStringBytes: 1 << 24, maxTotalBytes: 1 << 28}

func assertFreezeParity(t *testing.T, name string, v any, marker string) {
	t.Helper()
	frozen, err := json.Marshal(freezeValue(v, generousLimits))
	if err != nil {
		t.Errorf("%s: normalised value is not marshalable: %v", name, err)
		return
	}
	if marker != "" {
		if !strings.Contains(string(frozen), marker) {
			t.Errorf("%s: %s does not contain %q", name, frozen, marker)
		}
		return
	}
	direct, err := json.Marshal(v)
	if err != nil {
		t.Errorf("%s: encoding/json failed: %v", name, err)
		return
	}
	var a, b interface{}
	if err := json.Unmarshal(direct, &a); err != nil {
		t.Errorf("%s: %v", name, err)
	}
	if err := json.Unmarshal(frozen, &b); err != nil {
		t.Errorf("%s: %v", name, err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("%s: JSON changed\n direct: %s\n frozen: %s", name, direct, frozen)
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

	t.Run("struct fields stop at the budget", func(t *testing.T) {
		out := freezeValue(parityQuoted{}, valueLimits{maxNodes: 4}).(map[string]any)
		if len(out) > 4 {
			t.Errorf("%d fields kept under a budget of 4 values", len(out))
		}
		if marker, _ := out[truncatedKey].(string); !strings.Contains(marker, "fields of bt.parityQuoted omitted") {
			t.Errorf("no fields-omitted marker: %#v", out)
		}
	})

	t.Run("promoted fields sit at the depth of direct fields", func(t *testing.T) {
		v := parityFan{&parityPB{&parityD{1, 2}, 3}, &parityPC{&parityD{4, 5}, 6}}
		want := map[string]any{"PB": int64(3), "PC": int64(6)}
		if got := freezeValue(v, valueLimits{maxDepth: 1}); !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
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

type freezeSelfEmbed struct {
	*freezeSelfEmbed
	Name string
}

type freezeCycleInner struct{ V, W any }

type freezeCycleOuter struct {
	*freezeCycleInner
	X int
}

type freezeFanA struct {
	*freezeFanA
	*freezeFanB
	A int
}

type freezeFanB struct {
	*freezeFanA
	*freezeFanB
	B int
}

func TestFreezeEmbeddedPointers(t *testing.T) {
	t.Run("self embedding", func(t *testing.T) {
		self := &freezeSelfEmbed{Name: "a"}
		self.freezeSelfEmbed = self
		want := map[string]any{"Name": "a"}
		for _, v := range []any{self, *self} {
			if got := freezeValue(v, valueLimits{}); !reflect.DeepEqual(got, want) {
				t.Errorf("%T: got %#v, want %#v", v, got, want)
			}
		}
	})

	t.Run("long chain exposes the outer fields only", func(t *testing.T) {
		head := &freezeSelfEmbed{Name: "head"}
		for i, cur := 0, head; i < 10_000; i++ {
			cur.freezeSelfEmbed = &freezeSelfEmbed{Name: "tail"}
			cur = cur.freezeSelfEmbed
		}
		want := map[string]any{"Name": "head"}
		if got := freezeValue(head, valueLimits{}); !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("cycle through an embedded pointer", func(t *testing.T) {
		o := &freezeCycleOuter{freezeCycleInner: &freezeCycleInner{}, X: 1}
		o.V, o.W = *o, *o
		nested := map[string]any{"V": "[bt: cycle]", "W": "[bt: cycle]", "X": int64(1)}
		want := map[string]any{"V": nested, "W": nested, "X": int64(1)}
		if got := freezeValue(o, valueLimits{}); !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
		attrs := freezeAttributes(map[string]any{"a": o, "b": "keep"}, valueLimits{})
		if attrs["b"] != "keep" {
			t.Errorf("key after the cycle lost: %#v", attrs)
		}
		if _, cut := attrs[truncatedKey]; cut {
			t.Errorf("budget spent on the cycle: %#v", attrs)
		}
	})

	t.Run("shared embedded pointers promote each type once", func(t *testing.T) {
		a, b := &freezeFanA{}, &freezeFanB{}
		for i := 0; i < 64; i++ {
			a, b = &freezeFanA{freezeFanA: a, freezeFanB: b, A: i}, &freezeFanB{freezeFanA: a, freezeFanB: b, B: i}
		}
		start := time.Now()
		assertFreezeParity(t, "fan-out", a, "")
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("fan-out took %v", elapsed)
		}
	})
}

type ptrJSON struct{ N int }

func (p *ptrJSON) MarshalJSON() ([]byte, error) { return []byte(fmt.Sprintf(`{"ptr":%d}`, p.N)), nil }

type ptrText struct{ S string }

func (p *ptrText) MarshalText() ([]byte, error) { return []byte("text:" + p.S), nil }

type (
	plainByte uint8
	textByte  uint8
	jsonByte  uint8
)

func (b *textByte) MarshalText() ([]byte, error) { return []byte(fmt.Sprintf("b%d", *b)), nil }

func (b jsonByte) MarshalJSON() ([]byte, error) { return []byte(fmt.Sprintf(`"j%d"`, b)), nil }

type valueZero struct{ N int }

func (v valueZero) IsZero() bool { return v.N == 1 }

type ptrZero struct{ N int }

func (v *ptrZero) IsZero() bool { return v.N == 2 }

type panicZero struct{ N int }

func (panicZero) IsZero() bool { panic("IsZero exploded") }

type stringerKey int

func (stringerKey) String() string { return "key" }

type (
	ParityCount int
	parityCount int
)

type parityInner struct {
	Name string
	X    int
}

type parityShadow struct {
	Name string
	parityInner
}

type parityDupA struct{ X, A int }

type parityDupB struct{ X, B int }

type paritySameDepth struct {
	parityDupA
	parityDupB
}

type parityTagged struct {
	Y int `json:"X"`
}

type parityUntagged struct{ X int }

type parityTagWins struct {
	parityTagged
	parityUntagged
}

type parityTagWinsReversed struct {
	parityUntagged
	parityTagged
}

type parityOmitEmpty struct {
	When  time.Time      `json:",omitempty"`
	Inner parityInner    `json:",omitempty"`
	Array [2]int         `json:",omitempty"`
	Slice []int          `json:",omitempty"`
	Map   map[string]int `json:",omitempty"`
	Ptr   *int           `json:",omitempty"`
	Iface any            `json:",omitempty"`
	Str   string         `json:",omitempty"`
}

type parityOmitZero struct {
	When   time.Time   `json:",omitzero"`
	Inner  parityInner `json:",omitzero"`
	Array  [2]int      `json:",omitzero"`
	Slice  []int       `json:",omitzero"`
	Value  valueZero   `json:",omitzero"`
	Ptr    ptrZero     `json:",omitzero"`
	PtrPtr *ptrZero    `json:",omitzero"`
	ValPtr *valueZero  `json:",omitzero"`
	Iface  isZeroer    `json:",omitzero"`
}

type parityMarshalers struct {
	JSON      ptrJSON
	Text      ptrText
	JSONPtr   *ptrJSON
	JSONSlice []ptrJSON
	TextArray [2]ptrText
	JSONMap   map[string]ptrJSON
}

type parityQuoted struct {
	B      bool        `json:",string"`
	I      int         `json:",string"`
	U      uint16      `json:",string"`
	F      float64     `json:",string"`
	F32    float32     `json:",string"`
	S      string      `json:",string"`
	Named  namedString `json:",string"`
	IntPtr *int        `json:",string"`
	StrPtr *string     `json:",string"`
	PtrPtr **int       `json:",string"` //nolint:staticcheck
	Number json.Number `json:",string"`
	Level  slog.Level  `json:",string"`
	Slice  []int       `json:",string"` //nolint:staticcheck
	When   time.Time   `json:",string"` //nolint:staticcheck
}

type parityBytes struct {
	Plain []byte
	Named []plainByte
	Text  []textByte
	JSON  []jsonByte
	Array [3]byte
}

type parityHidden struct{ X int }

type parityTags struct {
	parityHidden `json:"hidden"`
	Dash         int `json:"-,"` //nolint:staticcheck
	Skip         int `json:"-"`
	Punct        int `json:"ok-name.1"`
	Opts         int `json:",omitempty"`
}

type parityInvalidTags struct {
	Quote     int `json:"it's"`           //nolint:staticcheck
	Backslash int `json:"a\\b,omitempty"` //nolint:staticcheck
	Control   int `json:"t\tt"`
}

type parityEmbeddedKinds struct {
	fmt.Stringer
	ParityCount
	parityCount
	time.Duration `json:"dur"`
}

type parityEmbeddedTime struct {
	time.Time
}

type parityD struct {
	X  int
	DY int `json:"dy"`
}

type parityB1 struct {
	parityD
	B1 int
}

type parityB2 struct {
	parityD
	B2 int
}

type parityCopies struct {
	parityB1
	parityB2
}

type parityPB struct {
	*parityD
	PB int
}

type parityPC struct {
	*parityD
	PC int
}

type parityFan struct {
	*parityPB
	*parityPC
}

type parityAlias struct {
	A int
	P *int
}

type parityElem struct{ V int }

type paritySliceAlias struct {
	Items []parityElem
	First *parityElem
}

type parityEmbedAlias struct {
	First parityInner
	*parityInner
}

type parityNode struct {
	Next *parityNode
	V    int
}

type parityArrayHead struct {
	A [1]int
	B []int
}

type parityAnyHead struct {
	A [2]any
	B []any
}

type nilText struct{}

func (*nilText) MarshalText() ([]byte, error) { return []byte("niltext"), nil }

type nilJSON struct{}

func (*nilJSON) MarshalJSON() ([]byte, error) { return []byte(`"niljson"`), nil }

type parityDeclaredNil struct {
	Text    encoding.TextMarshaler
	JSON    json.Marshaler
	Big     encoding.TextMarshaler
	Texts   []encoding.TextMarshaler
	TextMap map[string]encoding.TextMarshaler
	Ptr     *nilText
}

func TestFreezeStructParity(t *testing.T) {
	one, two := 1, 2
	onePtr := &one
	str := "<p>"
	when := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

	alias := &parityAlias{A: 7}
	alias.P = &alias.A
	sliceAlias := &paritySliceAlias{Items: []parityElem{{1}, {2}}}
	sliceAlias.First = &sliceAlias.Items[0]
	elemAlias := []parityAlias{{A: 1}, {A: 2}}
	elemAlias[1].P = &elemAlias[0].A
	embedAlias := &parityEmbedAlias{First: parityInner{Name: "first", X: 1}}
	embedAlias.parityInner = &embedAlias.First
	arrayHead := []parityArrayHead{{A: [1]int{5}}}
	arrayHead[0].B = arrayHead[0].A[:]
	anyHead := []parityAnyHead{{A: [2]any{1, "x"}}}
	anyHead[0].B = anyHead[0].A[:1]
	declaredNil := parityDeclaredNil{
		Text: (*nilText)(nil), JSON: (*nilJSON)(nil), Big: (*big.Int)(nil),
		Texts:   []encoding.TextMarshaler{(*nilText)(nil)},
		TextMap: map[string]encoding.TextMarshaler{"a": (*nilText)(nil)},
	}
	cycle := &parityNode{V: 1}
	cycle.Next = &parityNode{V: 2, Next: cycle}

	marshalers := parityMarshalers{
		JSON: ptrJSON{1}, Text: ptrText{"t"}, JSONPtr: &ptrJSON{2},
		JSONSlice: []ptrJSON{{3}}, TextArray: [2]ptrText{{"a"}, {"b"}},
		JSONMap: map[string]ptrJSON{"k": {4}},
	}
	quoted := parityQuoted{
		B: true, I: -3, U: 9, F: 1e21, F32: 1e-7, S: "<a href=\"x\"> ", Named: "n",
		IntPtr: &two, StrPtr: &str, PtrPtr: &onePtr, Number: "12.5", Level: slog.LevelWarn,
		Slice: []int{1}, When: when,
	}
	omitZero := parityOmitZero{Value: valueZero{1}, Ptr: ptrZero{2}, PtrPtr: &ptrZero{2}, ValPtr: &valueZero{1}, Iface: (*ptrZero)(nil)}
	omitZeroKept := parityOmitZero{
		When: when, Value: valueZero{0}, Ptr: ptrZero{0}, PtrPtr: &ptrZero{3},
		ValPtr: &valueZero{3}, Iface: &ptrZero{5}, Slice: []int{},
	}

	cases := []struct {
		name   string
		value  any
		marker string
	}{
		{"outer field beats promoted", parityShadow{Name: "outer", parityInner: parityInner{Name: "inner", X: 1}}, ""},
		{"same-depth duplicates dropped", paritySameDepth{parityDupA{1, 2}, parityDupB{3, 4}}, ""},
		{"tagged beats untagged", parityTagWins{parityTagged{1}, parityUntagged{2}}, ""},
		{"tagged beats untagged reversed", parityTagWinsReversed{parityUntagged{2}, parityTagged{1}}, ""},
		{"omitempty zero values", parityOmitEmpty{Slice: []int{}, Map: map[string]int{}}, ""},
		{"omitempty set values", parityOmitEmpty{When: when, Array: [2]int{1}, Slice: []int{1}, Ptr: &one, Iface: 0, Str: "s"}, ""},
		{"omitzero zero values", parityOmitZero{}, ""},
		{"omitzero IsZero true", omitZero, ""},
		{"omitzero IsZero true addressable", &omitZero, ""},
		{"omitzero IsZero false", omitZeroKept, ""},
		{"omitzero IsZero false addressable", &omitZeroKept, ""},
		{"marshalers by value", marshalers, ""},
		{"marshalers addressable", &marshalers, ""},
		{"marshaler slice elements", []ptrJSON{{1}, {2}}, ""},
		{"marshaler array by value", [1]ptrText{{"v"}}, ""},
		{"marshaler array addressable", &[1]ptrText{{"p"}}, ""},
		{"quoted", quoted, ""},
		{"quoted addressable", &quoted, ""},
		{"quoted zero", parityQuoted{}, ""},
		{"byte slices", parityBytes{[]byte{1}, []plainByte{2}, []textByte{3, 4}, []jsonByte{5}, [3]byte{6}}, ""},
		{"tag names", parityTags{parityHidden{1}, 2, 3, 4, 0}, ""},
		{"embedded non-struct kinds", parityEmbeddedKinds{stringerKey(3), 4, 5, time.Second}, ""},
		{"embedded nil interface", parityEmbeddedKinds{}, ""},
		{"embedded time.Time", parityEmbeddedTime{when}, ""},
		{"embedded copies annihilated", parityCopies{parityB1{parityD{1, 2}, 3}, parityB2{parityD{4, 5}, 6}}, ""},
		{"embedded pointers to one type", parityFan{&parityPB{&parityD{1, 2}, 3}, &parityPC{&parityD{4, 5}, 6}}, ""},
		{"nil embedded pointers", parityFan{}, ""},
		{"self-embedded pointer", &freezeSelfEmbed{Name: "a", freezeSelfEmbed: &freezeSelfEmbed{Name: "b"}}, ""},
		{"stringer map key", map[stringerKey]int{3: 1}, ""},
		{"nil TextMarshaler pointer map key", map[*nilText]int{nil: 1}, ""},
		{"empty json.Number", json.Number(""), ""},
		{"pointer to own first field", alias, ""},
		{"pointer to own slice element", sliceAlias, ""},
		{"element pointing into its own slice", elemAlias, ""},
		{"embedded pointer to sibling field", embedAlias, ""},
		{"slice of an array field at the start of its element", arrayHead, ""},
		{"any slice of an array field at the start of its element", anyHead, ""},
		{"nil pointer in a declared marshaler interface", declaredNil, ""},
		{"nil pointer in a declared marshaler interface addressable", &declaredNil, ""},
		{"pointer cycle", cycle, "[bt: cycle]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertFreezeParity(t, tc.name, tc.value, tc.marker)
		})
	}

	t.Run("invalid tag names fall back to the field name", func(t *testing.T) {
		want := map[string]any{"Quote": int64(1), "Backslash": int64(2), "Control": int64(3)}
		if got := freezeValue(parityInvalidTags{1, 2, 3}, valueLimits{}); !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("declared text interface uses MarshalText as in encoding/json v1", func(t *testing.T) {
		five := big.NewInt(5)
		for _, tc := range []struct {
			value any
			want  string
		}{
			{struct{ K encoding.TextMarshaler }{five}, `{"K":"5"}`},
			{[]encoding.TextMarshaler{five}, `["5"]`},
			{map[string]interface{ encoding.TextMarshaler }{"a": five}, `{"a":"5"}`},
			{struct{ K json.Marshaler }{five}, `{"K":5}`},
			{[]any{five}, `[5]`},
		} {
			got, err := json.Marshal(freezeValue(tc.value, valueLimits{}))
			if err != nil || string(got) != tc.want {
				t.Errorf("%T: got %s (%v), want %s", tc.value, got, err, tc.want)
			}
		}
	})

	t.Run("panicking IsZero keeps the field", func(t *testing.T) {
		v := struct {
			P panicZero `json:",omitzero"`
		}{}
		want := map[string]any{"P": map[string]any{"N": int64(0)}}
		if got := freezeValue(v, valueLimits{}); !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})
}

var parityRandomTypes = []reflect.Type{
	reflect.TypeFor[freezeStruct](),
	reflect.TypeFor[freezeSelfEmbed](),
	reflect.TypeFor[parityShadow](),
	reflect.TypeFor[paritySameDepth](),
	reflect.TypeFor[parityTagWins](),
	reflect.TypeFor[parityTagWinsReversed](),
	reflect.TypeFor[parityOmitEmpty](),
	reflect.TypeFor[parityOmitZero](),
	reflect.TypeFor[parityMarshalers](),
	reflect.TypeFor[parityQuoted](),
	reflect.TypeFor[parityBytes](),
	reflect.TypeFor[parityTags](),
	reflect.TypeFor[parityEmbeddedKinds](),
	reflect.TypeFor[parityCopies](),
	reflect.TypeFor[parityFan](),
	reflect.TypeFor[parityNode](),
	reflect.TypeFor[[]parityMarshalers](),
	reflect.TypeFor[map[string]*parityQuoted](),
}

func TestFreezeRandomParity(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200; i++ {
		for _, typ := range parityRandomTypes {
			ptr := reflect.New(typ)
			fillRandom(r, ptr.Elem(), 0)
			name := fmt.Sprintf("%s#%d", typ, i)
			assertFreezeParity(t, name, ptr.Interface(), "")
			assertFreezeParity(t, name, ptr.Elem().Interface(), "")
		}
	}
}

var (
	randomStrings = []string{"", "a", "<&>", "héllo", " ", "line\nbreak", "\x00", "\xff", `"q"`}
	randomNumbers = []string{"", "0", "12", "-3.5e2"}
	randomFloats  = []float64{0, 1.5, -2.25, 1e-7, 1e21, 123456.789, 0.1, math.Copysign(0, -1)}
	randomInts    = []int64{0, 1, -1, 127, -128, math.MaxInt32, math.MinInt64}
	randomIfaces  = []any{nil, 1, "s", 2.5, true, []any{1, "x"}, map[string]any{"k": nil},
		valueZero{1}, valueZero{2}, &ptrZero{2}, &ptrZero{3}, (*ptrZero)(nil), stringerKey(2), slog.LevelWarn}
)

func fillRandom(r *rand.Rand, v reflect.Value, depth int) {
	if v.Kind() != reflect.Struct && v.Kind() != reflect.Array && !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(r.IntN(2) == 1)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(randomInts[r.IntN(len(randomInts))])
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(uint64(randomInts[r.IntN(len(randomInts))]))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(randomFloats[r.IntN(len(randomFloats))])
	case reflect.String:
		if v.Type() == jsonNumberType {
			v.SetString(randomNumbers[r.IntN(len(randomNumbers))])
		} else {
			v.SetString(randomStrings[r.IntN(len(randomStrings))])
		}
	case reflect.Pointer:
		if depth < 4 && r.IntN(3) > 0 {
			p := reflect.New(v.Type().Elem())
			fillRandom(r, p.Elem(), depth+1)
			v.Set(p)
		}
	case reflect.Slice:
		if n := r.IntN(5) - 1; n >= 0 && depth < 4 {
			s := reflect.MakeSlice(v.Type(), n, n)
			for i := 0; i < n; i++ {
				fillRandom(r, s.Index(i), depth+1)
			}
			v.Set(s)
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			fillRandom(r, v.Index(i), depth+1)
		}
	case reflect.Map:
		if n := r.IntN(4) - 1; n >= 0 && depth < 4 {
			m := reflect.MakeMapWithSize(v.Type(), n)
			for i := 0; i < n; i++ {
				k := reflect.New(v.Type().Key()).Elem()
				e := reflect.New(v.Type().Elem()).Elem()
				fillRandom(r, k, depth+1)
				fillRandom(r, e, depth+1)
				m.SetMapIndex(k, e)
			}
			v.Set(m)
		}
	case reflect.Struct:
		if v.Type() == timeType {
			if v.CanSet() && r.IntN(2) == 1 {
				v.Set(reflect.ValueOf(time.Date(2026, 11, 18, 10, 0, 0, r.IntN(1000), time.UTC)))
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			fillRandom(r, v.Field(i), depth+1)
		}
	case reflect.Interface:
		var fits []any
		for _, c := range randomIfaces {
			if c == nil || reflect.TypeOf(c).Implements(v.Type()) {
				fits = append(fits, c)
			}
		}
		if c := fits[r.IntN(len(fits))]; c != nil {
			v.Set(reflect.ValueOf(c))
		}
	}
}
