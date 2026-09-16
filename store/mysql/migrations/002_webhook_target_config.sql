-- Webhook target config table (migration 002)
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
