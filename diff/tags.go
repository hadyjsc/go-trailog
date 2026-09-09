// Package diff provides struct and JSON diffing for the trailog library.
package diff

import (
	"reflect"
	"strings"
	"sync"
)

// Tag values for the `trailog` struct tag.
const (
	TagTrack = "track" // field is tracked (included in diffs)
	TagID    = "id"    // field is the entity ID (not diffed)
	TagSkip  = "-"     // field is never tracked
	TagMask  = "mask"  // field is tracked but value is redacted in diffs
)

// fieldInfo holds parsed tag metadata for one struct field.
type fieldInfo struct {
	name   string // the field name used in FieldDiff.Field
	tag    string // raw tag value: "track", "-", "mask", "id"
	nested bool   // true when the field is a nested struct to recurse into
}

// typeCache caches parsed field metadata by reflect.Type to avoid re-parsing on every diff.
var typeCache sync.Map // map[reflect.Type][]fieldInfo

// parseType extracts trailog-tagged field info from a struct type, with caching.
func parseType(t reflect.Type) []fieldInfo {
	// Dereference pointer types.
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}

	if cached, ok := typeCache.Load(t); ok {
		return cached.([]fieldInfo)
	}

	var fields []fieldInfo
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}

		tag := sf.Tag.Get("trailog")
		if tag == "" {
			// No trailog tag — check if this is a nested struct we should recurse into.
			ft := sf.Type
			for ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				fields = append(fields, fieldInfo{
					name:   toSnake(sf.Name),
					tag:    "",
					nested: true,
				})
			}
			continue
		}

		parts := strings.Split(tag, ",")
		mainTag := strings.TrimSpace(parts[0])

		fields = append(fields, fieldInfo{
			name: toSnake(sf.Name),
			tag:  mainTag,
		})
	}

	typeCache.Store(t, fields)
	return fields
}

// toSnake converts a PascalCase or camelCase identifier to snake_case for use as field names.
func toSnake(s string) string {
	var result []rune
	runes := []rune(s)
	for i, r := range runes {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				result = append(result, '_')
			}
			result = append(result, r+32)
		} else {
			result = append(result, r)
		}
	}
	return string(result)
}
