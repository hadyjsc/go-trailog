package diff

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// JSON computes a field-level diff between two JSON-like maps (map[string]any).
// Useful when diffing JSONB columns or untyped data from the database.
// Nested maps are recursed into using dot-path notation ("address.city").
// Slice/array values are compared holistically (not element-by-element) and produce
// a single array-type FieldDiff if they differ.
func JSON(before, after map[string]any) []FieldDiff {
	return jsonDiff(before, after, "")
}

// JSONBytes unmarshals two JSON byte slices and diffs them.
func JSONBytes(before, after []byte) ([]FieldDiff, error) {
	var bMap, aMap map[string]any
	if len(before) > 0 {
		if err := json.Unmarshal(before, &bMap); err != nil {
			return nil, fmt.Errorf("trailog/diff: unmarshal before: %w", err)
		}
	}
	if len(after) > 0 {
		if err := json.Unmarshal(after, &aMap); err != nil {
			return nil, fmt.Errorf("trailog/diff: unmarshal after: %w", err)
		}
	}
	return JSON(bMap, aMap), nil
}

func jsonDiff(before, after map[string]any, prefix string) []FieldDiff {
	var diffs []FieldDiff

	// Collect all keys from both maps.
	keys := make(map[string]struct{})
	for k := range before {
		keys[k] = struct{}{}
	}
	for k := range after {
		keys[k] = struct{}{}
	}

	for key := range keys {
		fieldPath := key
		if prefix != "" {
			fieldPath = prefix + "." + key
		}

		bVal, bOk := before[key]
		aVal, aOk := after[key]

		// Field removed — only record if the old value was non-nil (a real removal).
		if bOk && !aOk {
			if bVal != nil {
				diffs = append(diffs, FieldDiff{
					Field:    fieldPath,
					OldValue: bVal,
					NewValue: nil,
					Type:     inferType(bVal, nil),
				})
			}
			continue
		}
		// Field added — only record if the new value is non-nil (a real addition).
		if !bOk && aOk {
			if aVal != nil {
				diffs = append(diffs, FieldDiff{
					Field:    fieldPath,
					OldValue: nil,
					NewValue: aVal,
					Type:     inferType(nil, aVal),
				})
			}
			continue
		}

		// Both exist — check for nested map to recurse.
		bMap, bIsMap := bVal.(map[string]any)
		aMap, aIsMap := aVal.(map[string]any)
		if bIsMap && aIsMap {
			nested := jsonDiff(bMap, aMap, fieldPath)
			diffs = append(diffs, nested...)
			continue
		}

		// Compare using deep equality.
		if !reflect.DeepEqual(bVal, aVal) {
			diffs = append(diffs, FieldDiff{
				Field:    fieldPath,
				OldValue: bVal,
				NewValue: aVal,
				Type:     inferType(bVal, aVal),
			})
		}
	}

	return diffs
}
