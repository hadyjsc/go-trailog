// Package mysql provides a MySQL-backed Store implementation for trailog.
// It uses the same store.Store interface as the Postgres implementation,
// with MySQL-specific SQL dialect adjustments:
//   - Placeholders are ? (positional, not $N)
//   - UUIDs are stored as CHAR(36)
//   - JSON columns use JSON type (MySQL 5.7.8+)
//   - Timestamps use DATETIME(6) with UTC_TIMESTAMP(6)
//   - No SKIP LOCKED before MySQL 8.0 — the outbox query uses FOR UPDATE only
package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hadyjsc/go-trailog/store"

	_ "github.com/go-sql-driver/mysql" // MySQL driver
)

// Store implements store.Store against a MySQL database.
type Store struct {
	db *sql.DB
}

// New opens a connection to the given MySQL DSN and returns a ready Store.
// DSN format: user:password@tcp(host:port)/dbname?parseTime=true&loc=UTC
func New(dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("trailog/store/mysql: open: %w", err)
	}
	s := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// NewFromDB creates a Store from an already-open *sql.DB.
func NewFromDB(db *sql.DB) *Store { return &Store{db: db} }

// Ping checks the connection.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("trailog/store/mysql: ping: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (s *Store) Close() error { return s.db.Close() }

// SaveRevision persists the revision, entity changes, and field diffs atomically.
func (s *Store) SaveRevision(ctx context.Context, rev store.Revision) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("trailog/store/mysql: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	metaJSON, err := marshalJSON(rev.Metadata)
	if err != nil {
		return err
	}
	actorExtraJSON, err := marshalJSON(rev.ActorExtra)
	if err != nil {
		return err
	}

	const revQ = `
		INSERT IGNORE INTO audit_revision
			(id, correlation_id, actor_id, actor_type, actor_name, actor_email, actor_ip,
			 actor_extra, action, reason, metadata, occurred_at, prev_hash, hash)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

	_, err = tx.ExecContext(ctx, revQ,
		rev.ID, rev.CorrelationID, rev.ActorID, rev.ActorType, rev.ActorName,
		rev.ActorEmail, rev.ActorIP, nullableStr(actorExtraJSON),
		rev.Action, rev.Reason, nullableStr(metaJSON), rev.OccurredAt.UTC(),
		rev.PrevHash, rev.Hash,
	)
	if err != nil {
		return fmt.Errorf("trailog/store/mysql: insert revision: %w", err)
	}

	for _, ec := range rev.Changes {
		beforeJSON, err := marshalJSON(ec.SnapshotBefore)
		if err != nil {
			return err
		}
		afterJSON, err := marshalJSON(ec.SnapshotAfter)
		if err != nil {
			return err
		}
		const ecQ = `
			INSERT INTO audit_entity_change
				(id, revision_id, entity_type, entity_id, op, snapshot_before, snapshot_after)
			VALUES (?,?,?,?,?,?,?)`
		_, err = tx.ExecContext(ctx, ecQ,
			ec.ID, ec.RevisionID, ec.EntityType, ec.EntityID, ec.Op,
			nullableStr(beforeJSON), nullableStr(afterJSON),
		)
		if err != nil {
			return fmt.Errorf("trailog/store/mysql: insert entity_change: %w", err)
		}

		for _, fd := range ec.Fields {
			oldJSON, err := marshalJSON(fd.OldValue)
			if err != nil {
				return err
			}
			newJSON, err := marshalJSON(fd.NewValue)
			if err != nil {
				return err
			}
			const fdQ = `
				INSERT INTO audit_field_diff
					(id, entity_change_id, field_name, old_value, new_value, value_type)
				VALUES (?,?,?,?,?,?)`
			_, err = tx.ExecContext(ctx, fdQ,
				fd.ID, fd.EntityChangeID, fd.FieldName,
				nullableStr(oldJSON), nullableStr(newJSON), fd.ValueType,
			)
			if err != nil {
				return fmt.Errorf("trailog/store/mysql: insert field_diff: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("trailog/store/mysql: commit: %w", err)
	}
	return nil
}

// GetRevision returns the full Revision (with EntityChanges and FieldDiffs).
func (s *Store) GetRevision(ctx context.Context, id string) (*store.Revision, error) {
	const q = `
		SELECT id, correlation_id, actor_id, actor_type, actor_name, actor_email, actor_ip,
		       actor_extra, action, reason, metadata, occurred_at, prev_hash, hash
		FROM audit_revision WHERE id = ?`

	row := s.db.QueryRowContext(ctx, q, id)
	rev, err := scanRevisionRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("trailog/store/mysql: revision %q not found", id)
		}
		return nil, fmt.Errorf("trailog/store/mysql: get revision: %w", err)
	}
	changes, err := s.loadChangesForRevision(ctx, id)
	if err != nil {
		return nil, err
	}
	rev.Changes = changes
	return rev, nil
}

// ListRevisions returns a cursor-paginated page of revisions, newest first.
func (s *Store) ListRevisions(ctx context.Context, f store.TimelineFilter) ([]store.Revision, string, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}

	args := []any{}
	var conds []string

	entityConds := []string{"(ec.entity_type = ? AND ec.entity_id = ?)"}
	args = append(args, f.EntityType, f.EntityID)
	for _, rel := range f.RelatedIDs {
		entityConds = append(entityConds, "(ec.entity_type = ? AND ec.entity_id = ?)")
		args = append(args, rel.Type, rel.ID)
	}
	conds = append(conds, "("+strings.Join(entityConds, " OR ")+")")

	if f.ActorID != "" {
		conds = append(conds, "r.actor_id = ?")
		args = append(args, f.ActorID)
	}
	if !f.From.IsZero() {
		conds = append(conds, "r.occurred_at >= ?")
		args = append(args, f.From.UTC())
	}
	if !f.To.IsZero() {
		conds = append(conds, "r.occurred_at <= ?")
		args = append(args, f.To.UTC())
	}
	if len(f.Actions) > 0 {
		placeholders := strings.Repeat("?,", len(f.Actions))
		placeholders = placeholders[:len(placeholders)-1]
		conds = append(conds, "r.action IN ("+placeholders+")")
		for _, a := range f.Actions {
			args = append(args, a)
		}
	}
	if f.Cursor != "" {
		conds = append(conds, "r.occurred_at < (SELECT occurred_at FROM audit_revision WHERE id = ?)")
		args = append(args, f.Cursor)
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit+1)

	q := fmt.Sprintf(`
		SELECT DISTINCT r.id, r.correlation_id, r.actor_id, r.actor_type, r.actor_name,
		       r.actor_email, r.actor_ip, r.actor_extra, r.action, r.reason,
		       r.metadata, r.occurred_at, r.prev_hash, r.hash
		FROM audit_revision r
		JOIN audit_entity_change ec ON ec.revision_id = r.id
		%s
		ORDER BY r.occurred_at DESC
		LIMIT ?`, where)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("trailog/store/mysql: list revisions: %w", err)
	}
	defer rows.Close()

	var revisions []store.Revision
	for rows.Next() {
		rev, err := scanRevisionRows(rows)
		if err != nil {
			return nil, "", err
		}
		revisions = append(revisions, *rev)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if len(revisions) > limit {
		nextCursor = revisions[limit-1].ID
		revisions = revisions[:limit]
	}
	for i := range revisions {
		changes, err := s.loadChangesForRevision(ctx, revisions[i].ID)
		if err != nil {
			return nil, "", err
		}
		revisions[i].Changes = changes
	}
	return revisions, nextCursor, nil
}

// GetEntityChanges returns all EntityChange rows for an entity, newest first.
func (s *Store) GetEntityChanges(ctx context.Context, entityType, entityID string) ([]store.EntityChange, error) {
	const q = `
		SELECT ec.id, ec.revision_id, ec.entity_type, ec.entity_id, ec.op,
		       ec.snapshot_before, ec.snapshot_after
		FROM audit_entity_change ec
		JOIN audit_revision r ON r.id = ec.revision_id
		WHERE ec.entity_type = ? AND ec.entity_id = ?
		ORDER BY r.occurred_at DESC`

	rows, err := s.db.QueryContext(ctx, q, entityType, entityID)
	if err != nil {
		return nil, fmt.Errorf("trailog/store/mysql: get entity changes: %w", err)
	}
	defer rows.Close()

	var changes []store.EntityChange
	for rows.Next() {
		ec, err := scanEntityChange(rows)
		if err != nil {
			return nil, err
		}
		fields, err := s.loadFieldDiffs(ctx, ec.ID)
		if err != nil {
			return nil, err
		}
		ec.Fields = fields
		changes = append(changes, *ec)
	}
	return changes, rows.Err()
}

// LatestSnapshot returns the most recent snapshot_after for an entity.
func (s *Store) LatestSnapshot(ctx context.Context, entityType, entityID string) (map[string]any, error) {
	const q = `
		SELECT ec.snapshot_after
		FROM audit_entity_change ec
		JOIN audit_revision r ON r.id = ec.revision_id
		WHERE ec.entity_type = ? AND ec.entity_id = ?
		ORDER BY r.occurred_at DESC LIMIT 1`

	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, q, entityType, entityID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trailog/store/mysql: latest snapshot: %w", err)
	}
	if !raw.Valid {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw.String), &m); err != nil {
		return nil, fmt.Errorf("trailog/store/mysql: unmarshal snapshot: %w", err)
	}
	return m, nil
}

// SaveRevertLog persists a revert-log row.
func (s *Store) SaveRevertLog(ctx context.Context, log store.RevertLog) error {
	detail, err := marshalJSON(log.ConflictDetail)
	if err != nil {
		return err
	}
	const q = `
		INSERT INTO audit_revert_log
			(id, revert_revision_id, target_revision_id, strategy, had_conflicts, conflict_detail)
		VALUES (?,?,?,?,?,?)`
	_, err = s.db.ExecContext(ctx, q,
		log.ID, log.RevertRevisionID, log.TargetRevisionID,
		log.Strategy, log.HadConflicts, nullableStr(detail),
	)
	if err != nil {
		return fmt.Errorf("trailog/store/mysql: save revert log: %w", err)
	}
	return nil
}

// LastRevisionHash returns the hash of the most recently written revision.
func (s *Store) LastRevisionHash(ctx context.Context) (string, error) {
	const q = `SELECT hash FROM audit_revision ORDER BY occurred_at DESC LIMIT 1`
	var h sql.NullString
	err := s.db.QueryRowContext(ctx, q).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("trailog/store/mysql: last hash: %w", err)
	}
	return h.String, nil
}

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

func (s *Store) loadChangesForRevision(ctx context.Context, revisionID string) ([]store.EntityChange, error) {
	const q = `
		SELECT id, revision_id, entity_type, entity_id, op, snapshot_before, snapshot_after
		FROM audit_entity_change WHERE revision_id = ?`
	rows, err := s.db.QueryContext(ctx, q, revisionID)
	if err != nil {
		return nil, fmt.Errorf("trailog/store/mysql: load entity changes: %w", err)
	}
	defer rows.Close()
	var changes []store.EntityChange
	for rows.Next() {
		ec, err := scanEntityChange(rows)
		if err != nil {
			return nil, err
		}
		fields, err := s.loadFieldDiffs(ctx, ec.ID)
		if err != nil {
			return nil, err
		}
		ec.Fields = fields
		changes = append(changes, *ec)
	}
	return changes, rows.Err()
}

func (s *Store) loadFieldDiffs(ctx context.Context, entityChangeID string) ([]store.FieldDiff, error) {
	const q = `
		SELECT id, entity_change_id, field_name, old_value, new_value, value_type
		FROM audit_field_diff WHERE entity_change_id = ?`
	rows, err := s.db.QueryContext(ctx, q, entityChangeID)
	if err != nil {
		return nil, fmt.Errorf("trailog/store/mysql: load field diffs: %w", err)
	}
	defer rows.Close()
	var diffs []store.FieldDiff
	for rows.Next() {
		var fd store.FieldDiff
		var oldRaw, newRaw sql.NullString
		if err := rows.Scan(&fd.ID, &fd.EntityChangeID, &fd.FieldName, &oldRaw, &newRaw, &fd.ValueType); err != nil {
			return nil, err
		}
		if oldRaw.Valid {
			_ = json.Unmarshal([]byte(oldRaw.String), &fd.OldValue)
		}
		if newRaw.Valid {
			_ = json.Unmarshal([]byte(newRaw.String), &fd.NewValue)
		}
		diffs = append(diffs, fd)
	}
	return diffs, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanRevisionRow(row rowScanner) (*store.Revision, error) {
	return scanRev(row)
}
func scanRevisionRows(rows *sql.Rows) (*store.Revision, error) {
	return scanRev(rows)
}
func scanRev(row rowScanner) (*store.Revision, error) {
	var rev store.Revision
	var actorExtra, meta sql.NullString
	var prevHash, hash sql.NullString
	err := row.Scan(
		&rev.ID, &rev.CorrelationID,
		&rev.ActorID, &rev.ActorType, &rev.ActorName, &rev.ActorEmail, &rev.ActorIP,
		&actorExtra, &rev.Action, &rev.Reason, &meta,
		&rev.OccurredAt, &prevHash, &hash,
	)
	if err != nil {
		return nil, err
	}
	if actorExtra.Valid {
		_ = json.Unmarshal([]byte(actorExtra.String), &rev.ActorExtra)
	}
	if meta.Valid {
		_ = json.Unmarshal([]byte(meta.String), &rev.Metadata)
	}
	rev.PrevHash = prevHash.String
	rev.Hash = hash.String
	return &rev, nil
}

func scanEntityChange(rows *sql.Rows) (*store.EntityChange, error) {
	var ec store.EntityChange
	var before, after sql.NullString
	if err := rows.Scan(&ec.ID, &ec.RevisionID, &ec.EntityType, &ec.EntityID, &ec.Op, &before, &after); err != nil {
		return nil, err
	}
	if before.Valid {
		_ = json.Unmarshal([]byte(before.String), &ec.SnapshotBefore)
	}
	if after.Valid {
		_ = json.Unmarshal([]byte(after.String), &ec.SnapshotAfter)
	}
	return &ec, nil
}

func marshalJSON(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("trailog/store/mysql: marshal json: %w", err)
	}
	return b, nil
}

func nullableStr(b []byte) sql.NullString {
	if b == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}
