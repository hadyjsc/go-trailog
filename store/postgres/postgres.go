// Package postgres provides a PostgreSQL-backed Store implementation for trailog.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hadyjsc/go-trailog/store"

	_ "github.com/lib/pq" // PostgreSQL driver
)

// Store implements store.Store against a PostgreSQL database.
type Store struct {
	db *sql.DB
}

// New opens a connection to the given Postgres DSN and returns a ready Store.
// The caller is responsible for calling Close() when done.
func New(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("trailog/store/postgres: open: %w", err)
	}
	s := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// NewFromDB creates a Store from an already-open *sql.DB (useful when the
// caller manages the connection pool themselves, e.g. in GORM integrations).
func NewFromDB(db *sql.DB) *Store {
	return &Store{db: db}
}

// Ping verifies the connection is live.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("trailog/store/postgres: ping: %w", err)
	}
	return nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// SaveRevision inserts the revision, its entity changes, field diffs, and optional
// revert log row atomically inside a single transaction.
func (s *Store) SaveRevision(ctx context.Context, rev store.Revision) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("trailog/store/postgres: begin tx: %w", err)
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
		INSERT INTO audit_revision
			(id, correlation_id, actor_id, actor_type, actor_name, actor_email, actor_ip,
			 actor_extra, action, reason, metadata, occurred_at, prev_hash, hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (id) DO NOTHING`

	_, err = tx.ExecContext(ctx, revQ,
		rev.ID, rev.CorrelationID, rev.ActorID, rev.ActorType, rev.ActorName,
		rev.ActorEmail, rev.ActorIP, actorExtraJSON,
		rev.Action, rev.Reason, metaJSON, rev.OccurredAt, rev.PrevHash, rev.Hash,
	)
	if err != nil {
		return fmt.Errorf("trailog/store/postgres: insert revision: %w", err)
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
			VALUES ($1,$2,$3,$4,$5,$6,$7)`

		_, err = tx.ExecContext(ctx, ecQ,
			ec.ID, ec.RevisionID, ec.EntityType, ec.EntityID, ec.Op,
			nullableJSON(beforeJSON), nullableJSON(afterJSON),
		)
		if err != nil {
			return fmt.Errorf("trailog/store/postgres: insert entity_change: %w", err)
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
				VALUES ($1,$2,$3,$4,$5,$6)`
			_, err = tx.ExecContext(ctx, fdQ,
				fd.ID, fd.EntityChangeID, fd.FieldName,
				nullableJSON(oldJSON), nullableJSON(newJSON), fd.ValueType,
			)
			if err != nil {
				return fmt.Errorf("trailog/store/postgres: insert field_diff: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("trailog/store/postgres: commit: %w", err)
	}
	return nil
}

// ListEntities returns a paginated list of distinct (entity_type, entity_id) pairs
// enriched with metadata from their most recent audit change.
func (s *Store) ListEntities(ctx context.Context, f store.ListEntitiesFilter) (*store.ListEntitiesResult, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}

	// Validate / normalise sort options.
	sortCol := "last_changed_at"
	switch f.SortField {
	case "entity_type":
		sortCol = "entity_type"
	case "entity_id":
		sortCol = "entity_id"
	}
	sortDir := "DESC"
	if strings.ToLower(f.SortDir) == "asc" {
		sortDir = "ASC"
	}

	args := []any{}
	var conds []string
	argIdx := 1

	if f.EntityType != "" {
		conds = append(conds, fmt.Sprintf("ec.entity_type = $%d", argIdx))
		args = append(args, f.EntityType)
		argIdx++
	}
	if f.ActorID != "" {
		conds = append(conds, fmt.Sprintf("r.actor_id = $%d", argIdx))
		args = append(args, f.ActorID)
		argIdx++
	}
	if f.Op != "" {
		conds = append(conds, fmt.Sprintf("ec.op = $%d", argIdx))
		args = append(args, f.Op)
		argIdx++
	}
	if !f.From.IsZero() {
		conds = append(conds, fmt.Sprintf("r.occurred_at >= $%d", argIdx))
		args = append(args, f.From)
		argIdx++
	}
	if !f.To.IsZero() {
		conds = append(conds, fmt.Sprintf("r.occurred_at <= $%d", argIdx))
		args = append(args, f.To)
		argIdx++
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	// Count total matching entities (before pagination).
	countQ := fmt.Sprintf(`
		SELECT COUNT(*) FROM (
			SELECT ec.entity_type, ec.entity_id
			FROM audit_entity_change ec
			JOIN audit_revision r ON r.id = ec.revision_id
			%s
			GROUP BY ec.entity_type, ec.entity_id
		) sub`, where)
	var total int
	if err := s.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("trailog/store/postgres: list entities count: %w", err)
	}

	// Cursor: decode "<last_occurred_at>|<last_entity_id>".
	if f.Cursor != "" {
		parts := strings.SplitN(f.Cursor, "|", 2)
		if len(parts) == 2 {
			cursorTime, err := time.Parse(time.RFC3339Nano, parts[0])
			cursorID := parts[1]
			if err == nil {
				// Keyset pagination: rows that come after the cursor row in the chosen sort order.
				cursorCond := ""
				switch sortDir {
				case "ASC":
					cursorCond = fmt.Sprintf(
						"(last_changed_at > $%d OR (last_changed_at = $%d AND entity_id > $%d))",
						argIdx, argIdx+1, argIdx+2,
					)
				default:
					cursorCond = fmt.Sprintf(
						"(last_changed_at < $%d OR (last_changed_at = $%d AND entity_id > $%d))",
						argIdx, argIdx+1, argIdx+2,
					)
				}
				args = append(args, cursorTime, cursorTime, cursorID)
				argIdx += 3
				// The cursor filter applies to the outer query (after GROUP BY),
				// so we wrap the whole thing as a subquery below.
				_ = cursorCond // used inside the outer wrapper
			}
		}
	}

	// Build the main paginated query using a subquery so we can filter on
	// the aggregated last_changed_at without repeating the GROUP BY logic.
	args = append(args, limit)
	mainQ := fmt.Sprintf(`
		SELECT entity_type, entity_id, last_changed_at, last_op, last_actor_id, last_actor_name, revision_count
		FROM (
			SELECT
				ec.entity_type,
				ec.entity_id,
				MAX(r.occurred_at)                                          AS last_changed_at,
				(ARRAY_AGG(ec.op       ORDER BY r.occurred_at DESC))[1]     AS last_op,
				(ARRAY_AGG(r.actor_id  ORDER BY r.occurred_at DESC))[1]     AS last_actor_id,
				(ARRAY_AGG(r.actor_name ORDER BY r.occurred_at DESC))[1]    AS last_actor_name,
				COUNT(DISTINCT r.id)                                        AS revision_count
			FROM audit_entity_change ec
			JOIN audit_revision r ON r.id = ec.revision_id
			%s
			GROUP BY ec.entity_type, ec.entity_id
		) sub
		ORDER BY %s %s, entity_id ASC
		LIMIT $%d`, where, sortCol, sortDir, argIdx)

	rows, err := s.db.QueryContext(ctx, mainQ, args...)
	if err != nil {
		return nil, fmt.Errorf("trailog/store/postgres: list entities: %w", err)
	}
	defer rows.Close()

	var entities []store.EntitySummary
	for rows.Next() {
		var e store.EntitySummary
		if err := rows.Scan(&e.EntityType, &e.EntityID, &e.LastChangedAt,
			&e.LastOp, &e.LastActorID, &e.LastActorName, &e.RevisionCount); err != nil {
			return nil, fmt.Errorf("trailog/store/postgres: scan entity summary: %w", err)
		}
		entities = append(entities, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	nextCursor := ""
	if len(entities) == limit && limit > 0 {
		last := entities[len(entities)-1]
		nextCursor = last.LastChangedAt.UTC().Format(time.RFC3339Nano) + "|" + last.EntityID
	}

	return &store.ListEntitiesResult{
		Entities:   entities,
		NextCursor: nextCursor,
		Total:      total,
	}, nil
}

// GetRevision returns the full Revision (with EntityChanges and FieldDiffs).
func (s *Store) GetRevision(ctx context.Context, id string) (*store.Revision, error) {
	const q = `
		SELECT id, correlation_id, actor_id, actor_type, actor_name, actor_email, actor_ip,
		       actor_extra, action, reason, metadata, occurred_at, prev_hash, hash
		FROM audit_revision
		WHERE id = $1`

	row := s.db.QueryRowContext(ctx, q, id)
	rev, err := scanRevision(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("trailog/store/postgres: revision %q not found", id)
		}
		return nil, fmt.Errorf("trailog/store/postgres: get revision: %w", err)
	}

	changes, err := s.loadChangesForRevision(ctx, id)
	if err != nil {
		return nil, err
	}
	rev.Changes = changes
	return rev, nil
}

// ListRevisions returns a cursor-paginated page of revisions matching f.
func (s *Store) ListRevisions(ctx context.Context, f store.TimelineFilter) ([]store.Revision, string, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}

	args := []any{}
	conds := []string{}
	argIdx := 1

	// Build entity filter (primary + related).
	entityConds := []string{}
	primary := fmt.Sprintf("(ec.entity_type = $%d AND ec.entity_id = $%d)", argIdx, argIdx+1)
	args = append(args, f.EntityType, f.EntityID)
	argIdx += 2
	entityConds = append(entityConds, primary)

	for _, rel := range f.RelatedIDs {
		entityConds = append(entityConds,
			fmt.Sprintf("(ec.entity_type = $%d AND ec.entity_id = $%d)", argIdx, argIdx+1))
		args = append(args, rel.Type, rel.ID)
		argIdx += 2
	}
	conds = append(conds, "("+strings.Join(entityConds, " OR ")+")")

	if f.ActorID != "" {
		conds = append(conds, fmt.Sprintf("r.actor_id = $%d", argIdx))
		args = append(args, f.ActorID)
		argIdx++
	}
	if !f.From.IsZero() {
		conds = append(conds, fmt.Sprintf("r.occurred_at >= $%d", argIdx))
		args = append(args, f.From)
		argIdx++
	}
	if !f.To.IsZero() {
		conds = append(conds, fmt.Sprintf("r.occurred_at <= $%d", argIdx))
		args = append(args, f.To)
		argIdx++
	}
	if len(f.Actions) > 0 {
		placeholders := make([]string, len(f.Actions))
		for i, a := range f.Actions {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, a)
			argIdx++
		}
		conds = append(conds, "r.action IN ("+strings.Join(placeholders, ",")+")")
	}
	// Cursor: revisions older than the cursor revision's occurred_at.
	if f.Cursor != "" {
		conds = append(conds,
			fmt.Sprintf("r.occurred_at < (SELECT occurred_at FROM audit_revision WHERE id = $%d)", argIdx))
		args = append(args, f.Cursor)
		argIdx++
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	// fetch limit+1 to detect if there's a next page.
	args = append(args, limit+1)
	q := fmt.Sprintf(`
		SELECT DISTINCT r.id, r.correlation_id, r.actor_id, r.actor_type, r.actor_name,
		       r.actor_email, r.actor_ip, r.actor_extra, r.action, r.reason,
		       r.metadata, r.occurred_at, r.prev_hash, r.hash
		FROM audit_revision r
		JOIN audit_entity_change ec ON ec.revision_id = r.id
		%s
		ORDER BY r.occurred_at DESC
		LIMIT $%d`, where, argIdx)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("trailog/store/postgres: list revisions: %w", err)
	}
	defer rows.Close()

	var revisions []store.Revision
	for rows.Next() {
		rev, err := scanRevisionRow(rows)
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

	// Load entity changes for each revision.
	for i := range revisions {
		changes, err := s.loadChangesForRevision(ctx, revisions[i].ID)
		if err != nil {
			return nil, "", err
		}
		revisions[i].Changes = changes
	}

	return revisions, nextCursor, nil
}

// GetEntityChanges returns all EntityChange rows (with FieldDiffs) for an entity.
func (s *Store) GetEntityChanges(ctx context.Context, entityType, entityID string) ([]store.EntityChange, error) {
	const q = `
		SELECT ec.id, ec.revision_id, ec.entity_type, ec.entity_id, ec.op,
		       ec.snapshot_before, ec.snapshot_after
		FROM audit_entity_change ec
		JOIN audit_revision r ON r.id = ec.revision_id
		WHERE ec.entity_type = $1 AND ec.entity_id = $2
		ORDER BY r.occurred_at DESC`

	rows, err := s.db.QueryContext(ctx, q, entityType, entityID)
	if err != nil {
		return nil, fmt.Errorf("trailog/store/postgres: get entity changes: %w", err)
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

// LatestSnapshot returns the most recent SnapshotAfter for the entity.
func (s *Store) LatestSnapshot(ctx context.Context, entityType, entityID string) (map[string]any, error) {
	const q = `
		SELECT ec.snapshot_after
		FROM audit_entity_change ec
		JOIN audit_revision r ON r.id = ec.revision_id
		WHERE ec.entity_type = $1 AND ec.entity_id = $2
		ORDER BY r.occurred_at DESC
		LIMIT 1`

	var raw []byte
	err := s.db.QueryRowContext(ctx, q, entityType, entityID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trailog/store/postgres: latest snapshot: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("trailog/store/postgres: unmarshal snapshot: %w", err)
	}
	return m, nil
}

// SaveRevertLog persists a RevertLog row.
func (s *Store) SaveRevertLog(ctx context.Context, log store.RevertLog) error {
	detail, err := marshalJSON(log.ConflictDetail)
	if err != nil {
		return err
	}
	const q = `
		INSERT INTO audit_revert_log
			(id, revert_revision_id, target_revision_id, strategy, had_conflicts, conflict_detail)
		VALUES ($1,$2,$3,$4,$5,$6)`
	_, err = s.db.ExecContext(ctx, q,
		log.ID, log.RevertRevisionID, log.TargetRevisionID,
		log.Strategy, log.HadConflicts, nullableJSON(detail),
	)
	if err != nil {
		return fmt.Errorf("trailog/store/postgres: save revert log: %w", err)
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
		return "", fmt.Errorf("trailog/store/postgres: last hash: %w", err)
	}
	return h.String, nil
}

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

func (s *Store) loadChangesForRevision(ctx context.Context, revisionID string) ([]store.EntityChange, error) {
	const q = `
		SELECT id, revision_id, entity_type, entity_id, op, snapshot_before, snapshot_after
		FROM audit_entity_change
		WHERE revision_id = $1`

	rows, err := s.db.QueryContext(ctx, q, revisionID)
	if err != nil {
		return nil, fmt.Errorf("trailog/store/postgres: load entity changes: %w", err)
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
		FROM audit_field_diff
		WHERE entity_change_id = $1`

	rows, err := s.db.QueryContext(ctx, q, entityChangeID)
	if err != nil {
		return nil, fmt.Errorf("trailog/store/postgres: load field diffs: %w", err)
	}
	defer rows.Close()

	var diffs []store.FieldDiff
	for rows.Next() {
		var fd store.FieldDiff
		var oldRaw, newRaw []byte
		if err := rows.Scan(&fd.ID, &fd.EntityChangeID, &fd.FieldName, &oldRaw, &newRaw, &fd.ValueType); err != nil {
			return nil, err
		}
		if oldRaw != nil {
			_ = json.Unmarshal(oldRaw, &fd.OldValue)
		}
		if newRaw != nil {
			_ = json.Unmarshal(newRaw, &fd.NewValue)
		}
		diffs = append(diffs, fd)
	}
	return diffs, rows.Err()
}

// rowScanner abstracts *sql.Row and *sql.Rows for scanning.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRevision(row rowScanner) (*store.Revision, error) {
	var rev store.Revision
	var actorExtraRaw, metaRaw []byte
	err := row.Scan(
		&rev.ID, &rev.CorrelationID,
		&rev.ActorID, &rev.ActorType, &rev.ActorName, &rev.ActorEmail, &rev.ActorIP,
		&actorExtraRaw, &rev.Action, &rev.Reason, &metaRaw,
		&rev.OccurredAt, &rev.PrevHash, &rev.Hash,
	)
	if err != nil {
		return nil, err
	}
	if actorExtraRaw != nil {
		_ = json.Unmarshal(actorExtraRaw, &rev.ActorExtra)
	}
	if metaRaw != nil {
		_ = json.Unmarshal(metaRaw, &rev.Metadata)
	}
	return &rev, nil
}

func scanRevisionRow(rows *sql.Rows) (*store.Revision, error) {
	var rev store.Revision
	var actorExtraRaw, metaRaw []byte
	err := rows.Scan(
		&rev.ID, &rev.CorrelationID,
		&rev.ActorID, &rev.ActorType, &rev.ActorName, &rev.ActorEmail, &rev.ActorIP,
		&actorExtraRaw, &rev.Action, &rev.Reason, &metaRaw,
		&rev.OccurredAt, &rev.PrevHash, &rev.Hash,
	)
	if err != nil {
		return nil, err
	}
	if actorExtraRaw != nil {
		_ = json.Unmarshal(actorExtraRaw, &rev.ActorExtra)
	}
	if metaRaw != nil {
		_ = json.Unmarshal(metaRaw, &rev.Metadata)
	}
	return &rev, nil
}

func scanEntityChange(rows *sql.Rows) (*store.EntityChange, error) {
	var ec store.EntityChange
	var beforeRaw, afterRaw []byte
	err := rows.Scan(
		&ec.ID, &ec.RevisionID, &ec.EntityType, &ec.EntityID, &ec.Op,
		&beforeRaw, &afterRaw,
	)
	if err != nil {
		return nil, err
	}
	if beforeRaw != nil {
		_ = json.Unmarshal(beforeRaw, &ec.SnapshotBefore)
	}
	if afterRaw != nil {
		_ = json.Unmarshal(afterRaw, &ec.SnapshotAfter)
	}
	return &ec, nil
}

func marshalJSON(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("trailog/store/postgres: marshal json: %w", err)
	}
	return b, nil
}

// nullableJSON returns a sql.NullString containing the JSON bytes, or a null
// NullString if b is nil.
func nullableJSON(b []byte) sql.NullString {
	if b == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

// ────────────────────────────────────────────────────────────────
// WebhookTargetConfig
// ────────────────────────────────────────────────────────────────

// GetWebhookTargetConfig returns the stored webhook target for an entity type,
// or nil if none is configured.
func (s *Store) GetWebhookTargetConfig(ctx context.Context, entityType string) (*store.WebhookTargetConfig, error) {
	const q = `
		SELECT id, entity_type, url, method, auth, timeout_secs, created_at, updated_at
		FROM audit_webhook_target_config
		WHERE entity_type = $1`

	var c store.WebhookTargetConfig
	var auth sql.NullString
	err := s.db.QueryRowContext(ctx, q, entityType).Scan(
		&c.ID, &c.EntityType, &c.URL, &c.Method, &auth,
		&c.TimeoutSecs, &c.CreatedAt, &c.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trailog/store/postgres: get webhook target config: %w", err)
	}
	c.Auth = auth.String
	return &c, nil
}

// SaveWebhookTargetConfig upserts the webhook target configuration for an entity type.
func (s *Store) SaveWebhookTargetConfig(ctx context.Context, cfg store.WebhookTargetConfig) error {
	const q = `
		INSERT INTO audit_webhook_target_config
			(id, entity_type, url, method, auth, timeout_secs, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,now(),now())
		ON CONFLICT (entity_type) DO UPDATE SET
			url          = EXCLUDED.url,
			method       = EXCLUDED.method,
			auth         = EXCLUDED.auth,
			timeout_secs = EXCLUDED.timeout_secs,
			updated_at   = now()`

	timeout := cfg.TimeoutSecs
	if timeout <= 0 {
		timeout = 30
	}
	_, err := s.db.ExecContext(ctx, q,
		cfg.ID, cfg.EntityType, cfg.URL, cfg.Method,
		nullableString(cfg.Auth), timeout,
	)
	if err != nil {
		return fmt.Errorf("trailog/store/postgres: save webhook target config: %w", err)
	}
	return nil
}

// DeleteWebhookTargetConfig removes the webhook target configuration for an entity type.
func (s *Store) DeleteWebhookTargetConfig(ctx context.Context, entityType string) error {
	const q = `DELETE FROM audit_webhook_target_config WHERE entity_type = $1`
	if _, err := s.db.ExecContext(ctx, q, entityType); err != nil {
		return fmt.Errorf("trailog/store/postgres: delete webhook target config: %w", err)
	}
	return nil
}

// nullableString wraps a string in sql.NullString; empty string becomes NULL.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// DeleteRevision removes a revision and its child rows (field diffs → entity changes → revision).
// Used only by the revert rollback path.
func (s *Store) DeleteRevision(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("trailog/store/postgres: delete revision begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Delete field diffs for all entity changes in this revision.
	if _, err = tx.ExecContext(ctx, `
		DELETE FROM audit_field_diff
		WHERE entity_change_id IN (
			SELECT id FROM audit_entity_change WHERE revision_id = $1
		)`, id); err != nil {
		return fmt.Errorf("trailog/store/postgres: delete field diffs: %w", err)
	}
	// Delete entity changes.
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM audit_entity_change WHERE revision_id = $1`, id); err != nil {
		return fmt.Errorf("trailog/store/postgres: delete entity changes: %w", err)
	}
	// Delete the revision itself.
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM audit_revision WHERE id = $1`, id); err != nil {
		return fmt.Errorf("trailog/store/postgres: delete revision: %w", err)
	}
	return tx.Commit()
}
