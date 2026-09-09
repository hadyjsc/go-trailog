-- Trailog audit history schema — SQL Server dialect
-- Requires SQL Server 2016+ (for JSON support via NVARCHAR(MAX) + JSON functions).
-- Apply with: sqlcmd -S <server> -d <dbname> -i 001_initial_schema.sql
--
-- UUIDs stored as NVARCHAR(36) for maximum driver compatibility.
-- JSON stored as NVARCHAR(MAX) — use ISJSON() constraint to enforce validity.
-- Timestamps stored as DATETIMEOFFSET(6) for timezone-aware storage.

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_revision
-- ─────────────────────────────────────────────────────────────────────────────
IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'audit_revision')
BEGIN
    CREATE TABLE audit_revision (
        id              NVARCHAR(36)        NOT NULL,
        correlation_id  NVARCHAR(36)        NOT NULL,
        actor_id        NVARCHAR(255)       NULL,
        actor_type      NVARCHAR(64)        NULL,
        actor_name      NVARCHAR(255)       NULL,
        actor_email     NVARCHAR(255)       NULL,
        actor_ip        NVARCHAR(64)        NULL,
        actor_extra     NVARCHAR(MAX)       NULL, -- JSON
        action          NVARCHAR(64)        NOT NULL,
        reason          NVARCHAR(MAX)       NULL,
        metadata        NVARCHAR(MAX)       NULL, -- JSON
        occurred_at     DATETIMEOFFSET(6)   NOT NULL DEFAULT SYSDATETIMEOFFSET(),
        prev_hash       NVARCHAR(64)        NULL,
        hash            NVARCHAR(64)        NULL,
        CONSTRAINT PK_audit_revision PRIMARY KEY (id)
    );

    CREATE INDEX idx_revision_correlation ON audit_revision (correlation_id);
    CREATE INDEX idx_revision_occurred_at ON audit_revision (occurred_at DESC);
    CREATE INDEX idx_revision_actor       ON audit_revision (actor_id);
    CREATE INDEX idx_revision_action      ON audit_revision (action);
END
GO

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_entity_change
-- ─────────────────────────────────────────────────────────────────────────────
IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'audit_entity_change')
BEGIN
    CREATE TABLE audit_entity_change (
        id              NVARCHAR(36)        NOT NULL,
        revision_id     NVARCHAR(36)        NOT NULL,
        entity_type     NVARCHAR(128)       NOT NULL,
        entity_id       NVARCHAR(255)       NOT NULL,
        op              NVARCHAR(16)        NOT NULL,  -- create | update | delete
        snapshot_before NVARCHAR(MAX)       NULL,      -- JSON
        snapshot_after  NVARCHAR(MAX)       NULL,      -- JSON
        created_at      DATETIMEOFFSET(6)   NOT NULL DEFAULT SYSDATETIMEOFFSET(),
        CONSTRAINT PK_audit_entity_change PRIMARY KEY (id),
        CONSTRAINT FK_ec_revision FOREIGN KEY (revision_id) REFERENCES audit_revision(id)
    );

    CREATE INDEX idx_entity_change_entity   ON audit_entity_change (entity_type, entity_id, created_at DESC);
    CREATE INDEX idx_entity_change_revision ON audit_entity_change (revision_id);
END
GO

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_field_diff
-- ─────────────────────────────────────────────────────────────────────────────
IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'audit_field_diff')
BEGIN
    CREATE TABLE audit_field_diff (
        id                NVARCHAR(36)    NOT NULL,
        entity_change_id  NVARCHAR(36)    NOT NULL,
        field_name        NVARCHAR(255)   NOT NULL,
        old_value         NVARCHAR(MAX)   NULL,  -- JSON
        new_value         NVARCHAR(MAX)   NULL,  -- JSON
        value_type        NVARCHAR(16)    NULL,
        CONSTRAINT PK_audit_field_diff PRIMARY KEY (id),
        CONSTRAINT FK_fd_entity_change FOREIGN KEY (entity_change_id) REFERENCES audit_entity_change(id)
    );

    CREATE INDEX idx_field_diff_entity_change ON audit_field_diff (entity_change_id);
    CREATE INDEX idx_field_diff_field_name    ON audit_field_diff (field_name);
END
GO

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_relation
-- ─────────────────────────────────────────────────────────────────────────────
IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'audit_relation')
BEGIN
    CREATE TABLE audit_relation (
        id            NVARCHAR(36)    NOT NULL,
        parent_type   NVARCHAR(128)   NOT NULL,
        parent_id     NVARCHAR(255)   NOT NULL,
        child_type    NVARCHAR(128)   NOT NULL,
        child_id      NVARCHAR(255)   NOT NULL,
        relation_name NVARCHAR(128)   NOT NULL,
        dependency    NVARCHAR(32)    NOT NULL DEFAULT 'child_depends_on_parent',
        created_at    DATETIMEOFFSET(6) NOT NULL DEFAULT SYSDATETIMEOFFSET(),
        CONSTRAINT PK_audit_relation PRIMARY KEY (id)
    );

    CREATE INDEX idx_relation_parent ON audit_relation (parent_type, parent_id);
    CREATE INDEX idx_relation_child  ON audit_relation (child_type, child_id);
END
GO

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_revert_log
-- ─────────────────────────────────────────────────────────────────────────────
IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'audit_revert_log')
BEGIN
    CREATE TABLE audit_revert_log (
        id                  NVARCHAR(36)        NOT NULL,
        revert_revision_id  NVARCHAR(36)        NOT NULL,
        target_revision_id  NVARCHAR(36)        NOT NULL,
        strategy            NVARCHAR(16)        NOT NULL,
        had_conflicts       BIT                 NOT NULL DEFAULT 0,
        conflict_detail     NVARCHAR(MAX)       NULL, -- JSON
        created_at          DATETIMEOFFSET(6)   NOT NULL DEFAULT SYSDATETIMEOFFSET(),
        CONSTRAINT PK_audit_revert_log PRIMARY KEY (id),
        CONSTRAINT FK_rl_revert FOREIGN KEY (revert_revision_id) REFERENCES audit_revision(id),
        CONSTRAINT FK_rl_target FOREIGN KEY (target_revision_id) REFERENCES audit_revision(id)
    );

    CREATE INDEX idx_revert_log_target  ON audit_revert_log (target_revision_id);
    CREATE INDEX idx_revert_log_created ON audit_revert_log (created_at DESC);
END
GO

-- ─────────────────────────────────────────────────────────────────────────────
-- audit_outbox  (transactional outbox — Phase 4)
-- ─────────────────────────────────────────────────────────────────────────────
IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'audit_outbox')
BEGIN
    CREATE TABLE audit_outbox (
        id           NVARCHAR(36)        NOT NULL,
        payload      NVARCHAR(MAX)       NOT NULL, -- JSON
        created_at   DATETIMEOFFSET(6)   NOT NULL DEFAULT SYSDATETIMEOFFSET(),
        delivered    BIT                 NOT NULL DEFAULT 0,
        delivered_at DATETIMEOFFSET(6)   NULL,
        CONSTRAINT PK_audit_outbox PRIMARY KEY (id)
    );

    CREATE INDEX idx_outbox_undelivered ON audit_outbox (delivered, created_at)
        WHERE delivered = 0;
END
GO
