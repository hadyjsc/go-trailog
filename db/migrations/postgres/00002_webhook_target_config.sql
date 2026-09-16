-- +goose Up
CREATE TABLE IF NOT EXISTS audit_webhook_target_config (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_type  TEXT         NOT NULL UNIQUE,
    url          TEXT         NOT NULL,
    method       TEXT         NOT NULL DEFAULT 'POST',
    auth         TEXT,
    timeout_secs INT          NOT NULL DEFAULT 30,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_webhook_target_entity_type
    ON audit_webhook_target_config (entity_type);

-- +goose Down
DROP INDEX IF EXISTS idx_webhook_target_entity_type;
DROP TABLE IF EXISTS audit_webhook_target_config;
