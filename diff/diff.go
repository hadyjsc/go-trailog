package diff

import (
	"fmt"
	"reflect"
	"strings"
)

const maskValue = "***"

// FieldDiff holds the old and new value for a single changed field.
type FieldDiff struct {
	Field    string `json:"field"`
	OldValue any    `json:"old_value"`
	NewValue any    `json:"new_value"`
	Type     string `json:"type,omitempty"`
}

// Struct computes a field-level diff between two struct values (before and after).
// It respects `trailog` struct tags:
//   - `trailog:"track"` — include the field
//   - `trailog:"-"`     — always skip
//   - `trailog:"mask"`  — record that it changed, but redact the value to "***"
//   - `trailog:"id"`    — skip (it's the entity ID, not a data field)
//
// Fields without a tag are skipped unless they are nested structs, in which case
// Struct recurses into them (dot-path notation: "address.city").
// If before or after are nil (e.g. for a create or delete), pass nil explicitly.
func Struct(before, after any) ([]FieldDiff, error) {
	return structDiff(before, after, "")
}

func structDiff(before, after any, prefix string) ([]FieldDiff, error) {
	// Handle nil cases (create / delete).
	if before == nil && after == nil {
		return nil, nil
	}

	var beforeVal, afterVal reflect.Value
	if before != nil {
		beforeVal = reflect.ValueOf(before)
		for beforeVal.Kind() == reflect.Ptr {
			beforeVal = beforeVal.Elem()
		}
	}
	if after != nil {
		afterVal = reflect.ValueOf(after)
		for afterVal.Kind() == reflect.Ptr {
			afterVal = afterVal.Elem()
		}
	}

	// Determine the type to use for field iteration.
	var t reflect.Type
	if before != nil {
		t = beforeVal.Type()
	} else {
		t = afterVal.Type()
	}

	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("trailog/diff: expected struct, got %s", t.Kind())
	}

	fields := parseType(t)
	var diffs []FieldDiff

	for i, fi := range fields {
		sf := t.Field(i)

		// Skip fields with no tag unless they are nested structs.
		if fi.tag == TagSkip || fi.tag == TagID {
			continue
		}

		fieldPath := fi.name
		if prefix != "" {
			fieldPath = prefix + "." + fi.name
		}

		var bv, av reflect.Value
		if before != nil {
			bv = beforeVal.Field(i)
		}
		if after != nil {
			av = afterVal.Field(i)
		}

		// Recurse into nested structs.
		if fi.nested {
			ft := sf.Type
			for ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				var bInterface, aInterface any
				if before != nil && bv.IsValid() {
					bInterface = indirect(bv).Interface()
				}
				if after != nil && av.IsValid() {
					aInterface = indirect(av).Interface()
				}
				nested, err := structDiff(bInterface, aInterface, fieldPath)
				if err != nil {
					return nil, err
				}
				diffs = append(diffs, nested...)
			}
			continue
		}

		if fi.tag != TagTrack && fi.tag != TagMask {
			continue
		}

		var oldVal, newVal any
		if before != nil && bv.IsValid() {
			oldVal = indirect(bv).Interface()
		}
		if after != nil && av.IsValid() {
			newVal = indirect(av).Interface()
		}

		// No change — skip.
		if reflect.DeepEqual(oldVal, newVal) {
			continue
		}

		if fi.tag == TagMask {
			if oldVal != nil {
				oldVal = maskValue
			}
			if newVal != nil {
				newVal = maskValue
			}
		}

		diffs = append(diffs, FieldDiff{
			Field:    fieldPath,
			OldValue: oldVal,
			NewValue: newVal,
			Type:     inferType(oldVal, newVal),
		})
	}

	return diffs, nil
}

// indirect dereferences a pointer Value to its underlying value.
func indirect(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

// inferType returns a string type hint for a field value, used to populate FieldDiff.Type.
func inferType(old, new any) string {
	v := old
	if v == nil {
		v = new
	}
	if v == nil {
		return ""
	}
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return "number"
	case []any, []map[string]any:
		return "array"
	case map[string]any:
		return "json"
	default:
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Slice, reflect.Array:
			return "array"
		case reflect.Map, reflect.Struct:
			return "json"
		default:
			return strings.ToLower(rv.Kind().String())
		}
	}
}
