-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_revision — one row per logical change action ("commit")
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_revision (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    correlation_id  UUID        NOT NULL,
    actor_id        TEXT,
    actor_type      TEXT,
    actor_name      TEXT,
    actor_email     TEXT,
    actor_ip        TEXT,
    actor_extra     JSONB       DEFAULT '{}',
    action          TEXT        NOT NULL,
    reason          TEXT,
    metadata        JSONB       DEFAULT '{}',
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    prev_hash       TEXT,
    hash            TEXT
);

CREATE INDEX IF NOT EXISTS idx_revision_correlation ON audit_revision (correlation_id);
CREATE INDEX IF NOT EXISTS idx_revision_occurred_at ON audit_revision (occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_revision_actor       ON audit_revision (actor_id);
CREATE INDEX IF NOT EXISTS idx_revision_action      ON audit_revision (action);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_entity_change — one row per entity touched in a revision
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_entity_change (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    revision_id     UUID        NOT NULL REFERENCES audit_revision(id),
    entity_type     TEXT        NOT NULL,
    entity_id       TEXT        NOT NULL,
    op              TEXT        NOT NULL,
    snapshot_before JSONB,
    snapshot_after  JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_entity_change_entity   ON audit_entity_change (entity_type, entity_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_entity_change_revision ON audit_entity_change (revision_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_field_diff — one row per changed field
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_field_diff (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_change_id UUID NOT NULL REFERENCES audit_entity_change(id),
    field_name       TEXT NOT NULL,
    old_value        JSONB,
    new_value        JSONB,
    value_type       TEXT
);

CREATE INDEX IF NOT EXISTS idx_field_diff_entity_change ON audit_field_diff (entity_change_id);
CREATE INDEX IF NOT EXISTS idx_field_diff_field_name    ON audit_field_diff (field_name);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_relation — type-level links for timeline merging & revert ordering
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_relation (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_type   TEXT        NOT NULL,
    parent_id     TEXT        NOT NULL,
    child_type    TEXT        NOT NULL,
    child_id      TEXT        NOT NULL,
    relation_name TEXT        NOT NULL,
    dependency    TEXT        NOT NULL DEFAULT 'child_depends_on_parent',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_relation_parent ON audit_relation (parent_type, parent_id);
CREATE INDEX IF NOT EXISTS idx_relation_child  ON audit_relation (child_type,  child_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_revert_log — traceability for every revert operation
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_revert_log (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    revert_revision_id  UUID        NOT NULL REFERENCES audit_revision(id),
    target_revision_id  UUID        NOT NULL REFERENCES audit_revision(id),
    strategy            TEXT        NOT NULL,
    had_conflicts       BOOLEAN     NOT NULL DEFAULT false,
    conflict_detail     JSONB,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_revert_log_target  ON audit_revert_log (target_revision_id);
CREATE INDEX IF NOT EXISTS idx_revert_log_created ON audit_revert_log (created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_outbox — transactional outbox for reliable async writes
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

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS audit_outbox;
DROP TABLE IF EXISTS audit_revert_log;
DROP TABLE IF EXISTS audit_field_diff;
DROP TABLE IF EXISTS audit_entity_change;
DROP TABLE IF EXISTS audit_relation;
DROP TABLE IF EXISTS audit_revision;
-- +goose StatementEnd
