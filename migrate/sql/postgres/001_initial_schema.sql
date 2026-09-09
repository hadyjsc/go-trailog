-- Trailog audit history schema — initial migration
-- Apply with: psql $DATABASE_URL -f 001_initial_schema.sql
-- All audit tables are append-only. The application role should have
-- INSERT + SELECT only (no UPDATE/DELETE) on these tables.

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_revision
-- One row per logical change action ("commit").
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_revision (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    correlation_id  UUID        NOT NULL,       -- groups entity changes from same request/tx
    actor_id        TEXT,
    actor_type      TEXT,
    actor_name      TEXT,
    actor_email     TEXT,
    actor_ip        TEXT,
    actor_extra     JSONB       DEFAULT '{}',
    action          TEXT        NOT NULL,       -- create | update | delete | revert | custom
    reason          TEXT,
    metadata        JSONB       DEFAULT '{}',
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    prev_hash       TEXT,                       -- tamper-evidence chain: hash of previous revision
    hash            TEXT                        -- SHA-256(id + action + occurred_at + prev_hash)
);

CREATE INDEX IF NOT EXISTS idx_revision_correlation  ON audit_revision (correlation_id);
CREATE INDEX IF NOT EXISTS idx_revision_occurred_at  ON audit_revision (occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_revision_actor        ON audit_revision (actor_id);
CREATE INDEX IF NOT EXISTS idx_revision_action       ON audit_revision (action);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_entity_change
-- One row per entity (table row) touched within a revision ("file changed").
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_entity_change (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    revision_id     UUID        NOT NULL REFERENCES audit_revision(id),
    entity_type     TEXT        NOT NULL,
    entity_id       TEXT        NOT NULL,
    op              TEXT        NOT NULL,       -- create | update | delete
    snapshot_before JSONB,                      -- full entity state before change (NULL for create)
    snapshot_after  JSONB,                      -- full entity state after change  (NULL for delete)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_entity_change_entity   ON audit_entity_change (entity_type, entity_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_entity_change_revision ON audit_entity_change (revision_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_field_diff
-- One row per changed field within an entity change ("diff line").
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_field_diff (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_change_id  UUID NOT NULL REFERENCES audit_entity_change(id),
    field_name        TEXT NOT NULL,
    old_value         JSONB,
    new_value         JSONB,
    value_type        TEXT                      -- string | number | bool | json | array
);

CREATE INDEX IF NOT EXISTS idx_field_diff_entity_change ON audit_field_diff (entity_change_id);
CREATE INDEX IF NOT EXISTS idx_field_diff_field_name    ON audit_field_diff (field_name);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_relation
-- Declares type-level links between entity types so timelines can be merged
-- and the revert engine can topologically sort multi-table writes.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_relation (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_type     TEXT        NOT NULL,
    parent_id       TEXT        NOT NULL,
    child_type      TEXT        NOT NULL,
    child_id        TEXT        NOT NULL,
    relation_name   TEXT        NOT NULL,
    dependency      TEXT        NOT NULL DEFAULT 'child_depends_on_parent',  -- or 'independent'
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_relation_parent ON audit_relation (parent_type, parent_id);
CREATE INDEX IF NOT EXISTS idx_relation_child  ON audit_relation (child_type,  child_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_revert_log
-- Links a revert revision back to the revision it undid.
-- Every revert IS a new audit_revision (git-revert style) — this table just
-- adds the traceability link and records which strategy was used.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_revert_log (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    revert_revision_id  UUID        NOT NULL REFERENCES audit_revision(id),  -- the NEW revision created by reverting
    target_revision_id  UUID        NOT NULL REFERENCES audit_revision(id),  -- the revision it undid
    strategy            TEXT        NOT NULL,   -- force | field_level | block
    had_conflicts       BOOLEAN     NOT NULL DEFAULT false,
    conflict_detail     JSONB,                  -- resolved / overridden conflicts (audit of the audit)
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_revert_log_target  ON audit_revert_log (target_revision_id);
CREATE INDEX IF NOT EXISTS idx_revert_log_created ON audit_revert_log (created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_outbox  (Phase 4 — transactional outbox for reliable async writes)
-- Written inside the same DB transaction as the business write.
-- A background flusher reads rows here and writes them to the audit tables.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_outbox (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    payload      JSONB       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered    BOOLEAN     NOT NULL DEFAULT false,
    delivered_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_outbox_undelivered ON audit_outbox (delivered, created_at)
    WHERE delivered = false;

-- ─────────────────────────────────────────────────────────────────────────────
-- Partitioning note (high-volume deployments)
-- ─────────────────────────────────────────────────────────────────────────────
-- For high write volumes, convert audit_entity_change and audit_field_diff
-- to range-partitioned tables on created_at (monthly or quarterly):
--
--   ALTER TABLE audit_entity_change RENAME TO audit_entity_change_legacy;
--   CREATE TABLE audit_entity_change (... created_at TIMESTAMPTZ ...)
--       PARTITION BY RANGE (created_at);
--   CREATE TABLE audit_entity_change_2026_q1
--       PARTITION OF audit_entity_change
--       FOR VALUES FROM ('2026-01-01') TO ('2026-04-01');
--   -- etc.
--
-- Partitioning is not applied here to keep the migration compatible with
-- all Postgres versions and hosting environments out of the box.
