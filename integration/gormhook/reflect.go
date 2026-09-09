package gormhook

import (
	"fmt"
	"reflect"
)

// shallowClone allocates a new zero-value pointer of the same concrete type as v.
// If v is already a pointer, the returned value is *T (not **T).
func shallowClone(v any) any {
	if v == nil {
		return nil
	}
	t := reflect.TypeOf(v)
	// Dereference any pointer levels to get the concrete struct type.
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return reflect.New(t).Interface()
}

// reflectID walks the struct fields of v looking for an exported string-like
// field named "ID", "Id", or "id", returning its string representation.
// Falls back to an empty string if none is found.
func reflectID(v any) string {
	if v == nil {
		return ""
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}

	for _, name := range []string{"ID", "Id", "id"} {
		f := rv.FieldByName(name)
		if !f.IsValid() || !f.CanInterface() {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			return f.String()
		default:
			return fmt.Sprintf("%v", f.Interface())
		}
	}
	return ""
}
