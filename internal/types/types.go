// Package types holds shared primitive types used across trailog sub-packages
// to avoid import cycles.
package types

// Entity identifies a specific record by its type name and string ID.
type Entity struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// String returns "type:id".
func (e Entity) String() string {
	return e.Type + ":" + e.ID
}
