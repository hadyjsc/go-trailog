// Package gormhook provides GORM callback integration for trailog (Level 1 — zero-touch).
// Register once at startup and every GORM Save/Delete is automatically captured.
package gormhook

import (
	"fmt"

	"gorm.io/gorm"

	"github.com/hadyjsc/go-trailog"
)

const (
	beforeUpdateKey = "trailog:before_update"
	beforeDeleteKey = "trailog:before_delete"
)

// Register wires trailog audit callbacks into the given *gorm.DB for a specific
// entity type. entityType is the string label used in audit records (e.g. "invoice").
//
// Usage (once at startup):
//
//	gormhook.Register(db, tl.Recorder(), "invoice")
func Register(db *gorm.DB, rec trailog.Recorder, entityType string) error {
	if db == nil {
		return fmt.Errorf("trailog/gormhook: db is nil")
	}
	if rec == nil {
		return fmt.Errorf("trailog/gormhook: recorder is nil")
	}

	h := &hook{rec: rec, entityType: entityType}

	// Save-before: capture the current DB state so we have a "before" snapshot.
	if err := db.Callback().Update().Before("gorm:update").
		Register("trailog:before_update:"+entityType, h.beforeUpdate); err != nil {
		return fmt.Errorf("trailog/gormhook: register before_update for %q: %w", entityType, err)
	}
	// Save-after: diff before vs after and record the change.
	if err := db.Callback().Update().After("gorm:update").
		Register("trailog:after_update:"+entityType, h.afterUpdate); err != nil {
		return fmt.Errorf("trailog/gormhook: register after_update for %q: %w", entityType, err)
	}
	// Create-after: record the new entity.
	if err := db.Callback().Create().After("gorm:create").
		Register("trailog:after_create:"+entityType, h.afterCreate); err != nil {
		return fmt.Errorf("trailog/gormhook: register after_create for %q: %w", entityType, err)
	}
	// Delete-before: capture the entity state before deletion.
	if err := db.Callback().Delete().Before("gorm:delete").
		Register("trailog:before_delete:"+entityType, h.beforeDelete); err != nil {
		return fmt.Errorf("trailog/gormhook: register before_delete for %q: %w", entityType, err)
	}
	// Delete-after: record the deletion.
	if err := db.Callback().Delete().After("gorm:delete").
		Register("trailog:after_delete:"+entityType, h.afterDelete); err != nil {
		return fmt.Errorf("trailog/gormhook: register after_delete for %q: %w", entityType, err)
	}

	return nil
}

// hook holds the recorder and entity type for one registered GORM model.
type hook struct {
	rec        trailog.Recorder
	entityType string
}

// beforeUpdate fetches and caches the current DB row so afterUpdate can diff it.
func (h *hook) beforeUpdate(db *gorm.DB) {
	if db.Statement == nil || db.Statement.Context == nil {
		return
	}

	// Clone the current model to fetch the before-state from DB.
	// GORM stores the original model in db.Statement.Model.
	existing := cloneModel(db.Statement.Model)
	if existing == nil {
		return
	}

	result := db.Session(&gorm.Session{NewDB: true}).
		Model(existing).
		Where(db.Statement.Clauses).
		First(existing)
	if result.Error != nil {
		return
	}

	db.Statement.SetColumn(beforeUpdateKey, existing)
}

// afterUpdate diffs before vs after and records the update.
func (h *hook) afterUpdate(db *gorm.DB) {
	if db.Statement == nil || db.Statement.Context == nil || db.Error != nil {
		return
	}

	before, _ := db.Statement.Get(beforeUpdateKey)
	after := db.Statement.Model

	id := extractID(after)
	if id == "" {
		return
	}

	_ = h.rec.RecordUpdate(db.Statement.Context,
		trailog.Entity{Type: h.entityType, ID: id},
		before, after,
	)
}

// afterCreate records a newly created entity.
func (h *hook) afterCreate(db *gorm.DB) {
	if db.Statement == nil || db.Statement.Context == nil || db.Error != nil {
		return
	}

	after := db.Statement.Model
	id := extractID(after)
	if id == "" {
		return
	}

	_ = h.rec.RecordCreate(db.Statement.Context,
		trailog.Entity{Type: h.entityType, ID: id},
		after,
	)
}

// beforeDelete caches the entity before it is removed from the DB.
func (h *hook) beforeDelete(db *gorm.DB) {
	if db.Statement == nil || db.Statement.Context == nil {
		return
	}
	existing := cloneModel(db.Statement.Model)
	if existing == nil {
		return
	}

	result := db.Session(&gorm.Session{NewDB: true}).
		Model(existing).
		Where(db.Statement.Clauses).
		First(existing)
	if result.Error != nil {
		return
	}
	db.Statement.SetColumn(beforeDeleteKey, existing)
}

// afterDelete records the deletion using the cached before-state.
func (h *hook) afterDelete(db *gorm.DB) {
	if db.Statement == nil || db.Statement.Context == nil || db.Error != nil {
		return
	}

	before, _ := db.Statement.Get(beforeDeleteKey)
	if before == nil {
		before = db.Statement.Model
	}

	id := extractID(before)
	if id == "" {
		return
	}

	_ = h.rec.RecordDelete(db.Statement.Context,
		trailog.Entity{Type: h.entityType, ID: id},
		before,
	)
}

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

// cloneModel returns a new zero-value pointer of the same concrete type as v,
// so we can use it to fetch the current DB state without mutating v.
func cloneModel(v any) any {
	if v == nil {
		return nil
	}
	return shallowClone(v)
}

// extractID tries to retrieve the primary key string from a GORM model.
// It looks for an "ID" or "Id" field, falling back to the GORM convention.
func extractID(v any) string {
	if v == nil {
		return ""
	}
	return reflectID(v)
}
