-- Migration 002 — webhook target config
-- Stores per-entity-type webhook endpoint configuration for the revert engine.
-- When no per-request target is supplied, the reverter looks up the config here.

CREATE TABLE IF NOT EXISTS audit_webhook_target_config (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_type  TEXT         NOT NULL UNIQUE,
    url          TEXT         NOT NULL,
    method       TEXT         NOT NULL DEFAULT 'POST',  -- POST | PUT | PATCH
    auth         TEXT,                                  -- verbatim Authorization header value; NULL = no auth
    timeout_secs INT          NOT NULL DEFAULT 30,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_webhook_target_entity_type
    ON audit_webhook_target_config (entity_type);
