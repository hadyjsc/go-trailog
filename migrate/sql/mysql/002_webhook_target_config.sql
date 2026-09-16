-- Migration 002 — webhook target config
-- Stores per-entity-type webhook endpoint configuration for the revert engine.

CREATE TABLE IF NOT EXISTS audit_webhook_target_config (
    id           CHAR(36)      NOT NULL,
    entity_type  VARCHAR(128)  NOT NULL,
    url          TEXT          NOT NULL,
    method       VARCHAR(8)    NOT NULL DEFAULT 'POST',
    auth         TEXT          NULL,
    timeout_secs INT           NOT NULL DEFAULT 30,
    created_at   DATETIME(6)   NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
    updated_at   DATETIME(6)   NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
    PRIMARY KEY (id),
    UNIQUE KEY uq_webhook_target_entity_type (entity_type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE INDEX idx_webhook_target_entity_type ON audit_webhook_target_config (entity_type);
