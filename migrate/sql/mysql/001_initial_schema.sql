-- Trailog audit history schema — MySQL dialect
-- Requires MySQL 5.7.8+ (JSON type) or MySQL 8.0+ (recommended).
-- Apply with: mysql -u<user> -p <dbname> < 001_initial_schema.sql
--
-- All audit tables are append-only. The application role should have
-- INSERT + SELECT only (no UPDATE/DELETE) on these tables.

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_revision
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_revision (
    id              CHAR(36)        NOT NULL,
    correlation_id  CHAR(36)        NOT NULL,
    actor_id        VARCHAR(255)    NULL,
    actor_type      VARCHAR(64)     NULL,
    actor_name      VARCHAR(255)    NULL,
    actor_email     VARCHAR(255)    NULL,
    actor_ip        VARCHAR(64)     NULL,
    actor_extra     JSON            NULL,
    action          VARCHAR(64)     NOT NULL,
    reason          TEXT            NULL,
    metadata        JSON            NULL,
    occurred_at     DATETIME(6)     NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
    prev_hash       VARCHAR(64)     NULL,
    hash            VARCHAR(64)     NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_revision_correlation ON audit_revision (correlation_id);
CREATE INDEX idx_revision_occurred_at ON audit_revision (occurred_at DESC);
CREATE INDEX idx_revision_actor       ON audit_revision (actor_id);
CREATE INDEX idx_revision_action      ON audit_revision (action);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_entity_change
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_entity_change (
    id              CHAR(36)        NOT NULL,
    revision_id     CHAR(36)        NOT NULL,
    entity_type     VARCHAR(128)    NOT NULL,
    entity_id       VARCHAR(255)    NOT NULL,
    op              VARCHAR(16)     NOT NULL,   -- create | update | delete
    snapshot_before JSON            NULL,
    snapshot_after  JSON            NULL,
    created_at      DATETIME(6)     NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
    PRIMARY KEY (id),
    CONSTRAINT fk_ec_revision FOREIGN KEY (revision_id) REFERENCES audit_revision(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_entity_change_entity   ON audit_entity_change (entity_type, entity_id, created_at DESC);
CREATE INDEX idx_entity_change_revision ON audit_entity_change (revision_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_field_diff
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_field_diff (
    id                CHAR(36)        NOT NULL,
    entity_change_id  CHAR(36)        NOT NULL,
    field_name        VARCHAR(255)    NOT NULL,
    old_value         JSON            NULL,
    new_value         JSON            NULL,
    value_type        VARCHAR(16)     NULL,    -- string | number | bool | json | array
    PRIMARY KEY (id),
    CONSTRAINT fk_fd_entity_change FOREIGN KEY (entity_change_id) REFERENCES audit_entity_change(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_field_diff_entity_change ON audit_field_diff (entity_change_id);
CREATE INDEX idx_field_diff_field_name    ON audit_field_diff (field_name);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_relation
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_relation (
    id            CHAR(36)        NOT NULL,
    parent_type   VARCHAR(128)    NOT NULL,
    parent_id     VARCHAR(255)    NOT NULL,
    child_type    VARCHAR(128)    NOT NULL,
    child_id      VARCHAR(255)    NOT NULL,
    relation_name VARCHAR(128)    NOT NULL,
    dependency    VARCHAR(32)     NOT NULL DEFAULT 'child_depends_on_parent',
    created_at    DATETIME(6)     NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_relation_parent ON audit_relation (parent_type, parent_id);
CREATE INDEX idx_relation_child  ON audit_relation (child_type, child_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_revert_log
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_revert_log (
    id                  CHAR(36)        NOT NULL,
    revert_revision_id  CHAR(36)        NOT NULL,
    target_revision_id  CHAR(36)        NOT NULL,
    strategy            VARCHAR(16)     NOT NULL,
    had_conflicts       TINYINT(1)      NOT NULL DEFAULT 0,
    conflict_detail     JSON            NULL,
    created_at          DATETIME(6)     NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
    PRIMARY KEY (id),
    CONSTRAINT fk_rl_revert FOREIGN KEY (revert_revision_id) REFERENCES audit_revision(id),
    CONSTRAINT fk_rl_target FOREIGN KEY (target_revision_id) REFERENCES audit_revision(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_revert_log_target  ON audit_revert_log (target_revision_id);
CREATE INDEX idx_revert_log_created ON audit_revert_log (created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_outbox  (transactional outbox — Phase 4)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_outbox (
    id           CHAR(36)    NOT NULL,
    payload      JSON        NOT NULL,
    created_at   DATETIME(6) NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
    delivered    TINYINT(1)  NOT NULL DEFAULT 0,
    delivered_at DATETIME(6) NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_outbox_undelivered ON audit_outbox (delivered, created_at);
