package bt

import (
	"cmp"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"
)

// structField must stay in step with encoding/json v1 typeFields.
type structField struct {
	name      string
	index     []int
	omitEmpty bool
	omitZero  bool
	isZero    func(reflect.Value) bool
	quoted    bool
}

type isZeroer interface {
	IsZero() bool
}

var (
	isZeroerType     = reflect.TypeFor[isZeroer]()
	structFieldCache sync.Map
)

func cachedStructFields(t reflect.Type) []structField {
	if fields, ok := structFieldCache.Load(t); ok {
		return fields.([]structField)
	}
	fields, _ := structFieldCache.LoadOrStore(t, resolveStructFields(t))
	return fields.([]structField)
}

func resolveStructFields(t reflect.Type) []structField {
	type candidate struct {
		structField
		tagged bool
		typ    reflect.Type
	}
	current := []candidate{}
	next := []candidate{{typ: t}}
	var count, nextCount map[reflect.Type]int
	visited := map[reflect.Type]bool{}
	var found []candidate

	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, map[reflect.Type]int{}

		for _, embedded := range current {
			if visited[embedded.typ] {
				continue
			}
			visited[embedded.typ] = true

			for i := 0; i < embedded.typ.NumField(); i++ {
				sf := embedded.typ.Field(i)
				if sf.Anonymous {
					et := sf.Type
					if et.Kind() == reflect.Pointer {
						et = et.Elem()
					}
					if !sf.IsExported() && et.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				if !isValidJSONTagName(name) {
					name = ""
				}
				index := make([]int, len(embedded.index)+1)
				copy(index, embedded.index)
				index[len(embedded.index)] = i

				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}

				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					c := candidate{
						structField: structField{
							name:      name,
							index:     index,
							omitEmpty: hasTagOption(opts, "omitempty"),
							omitZero:  hasTagOption(opts, "omitzero"),
							quoted:    hasTagOption(opts, "string") && isQuotableKind(ft.Kind()),
						},
						tagged: name != "",
					}
					if name == "" {
						c.name = sf.Name
					}
					if c.omitZero {
						c.isZero = isZeroFunc(sf.Type)
					}
					found = append(found, c)
					if count[embedded.typ] > 1 {
						found = append(found, c)
					}
					continue
				}

				nextCount[ft]++
				if nextCount[ft] == 1 {
					next = append(next, candidate{structField: structField{name: ft.Name(), index: index}, typ: ft})
				}
			}
		}
	}

	slices.SortFunc(found, func(a, b candidate) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		if c := cmp.Compare(len(a.index), len(b.index)); c != 0 {
			return c
		}
		if a.tagged != b.tagged {
			if a.tagged {
				return -1
			}
			return +1
		}
		return slices.Compare(a.index, b.index)
	})

	out := make([]structField, 0, len(found))
	for advance, i := 0, 0; i < len(found); i += advance {
		for advance = 1; i+advance < len(found) && found[i+advance].name == found[i].name; advance++ {
		}
		if advance > 1 && len(found[i].index) == len(found[i+1].index) && found[i].tagged == found[i+1].tagged {
			continue
		}
		out = append(out, found[i].structField)
	}
	slices.SortFunc(out, func(a, b structField) int { return slices.Compare(a.index, b.index) })
	return out
}

func isValidJSONTagName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}

func hasTagOption(opts, option string) bool {
	for opts != "" {
		var name string
		name, opts, _ = strings.Cut(opts, ",")
		if name == option {
			return true
		}
	}
	return false
}

func isQuotableKind(k reflect.Kind) bool {
	switch k {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.String:
		return true
	}
	return false
}

func isZeroFunc(t reflect.Type) func(reflect.Value) bool {
	switch {
	case t.Kind() == reflect.Interface && t.Implements(isZeroerType):
		return func(v reflect.Value) bool {
			return v.IsNil() || (v.Elem().Kind() == reflect.Pointer && v.Elem().IsNil()) || callIsZero(v)
		}
	case t.Kind() == reflect.Pointer && t.Implements(isZeroerType):
		return func(v reflect.Value) bool {
			return v.IsNil() || callIsZero(v)
		}
	case t.Implements(isZeroerType):
		return callIsZero
	case reflect.PointerTo(t).Implements(isZeroerType):
		return func(v reflect.Value) bool {
			if !v.CanAddr() {
				if !v.CanInterface() {
					return false
				}
				boxed := reflect.New(v.Type()).Elem()
				boxed.Set(v)
				v = boxed
			}
			return callIsZero(v.Addr())
		}
	}
	return nil
}

// A panicking IsZero keeps the field, like other failing user methods.
func callIsZero(v reflect.Value) (zero bool) {
	defer func() {
		if recover() != nil {
			zero = false
		}
	}()
	return v.Interface().(isZeroer).IsZero()
}

func (sf *structField) omit(v reflect.Value) bool {
	if sf.omitEmpty && isEmptyJSONValue(v) {
		return true
	}
	if !sf.omitZero {
		return false
	}
	if sf.isZero == nil {
		return v.IsZero()
	}
	return sf.isZero(v)
}

func isEmptyJSONValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}

// Byte-kind elements with their own JSON or text form are not written as base64.
func isByteSlice(t reflect.Type) bool {
	if t.Elem().Kind() != reflect.Uint8 {
		return false
	}
	p := reflect.PointerTo(t.Elem())
	return !p.Implements(jsonMarshalerType) && !p.Implements(textMarshalerType)
}
