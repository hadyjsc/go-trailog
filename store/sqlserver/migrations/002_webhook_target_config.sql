-- Webhook target config table (migration 002)
IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'audit_webhook_target_config')
BEGIN
    CREATE TABLE audit_webhook_target_config (
        id           NVARCHAR(36)       NOT NULL,
        entity_type  NVARCHAR(128)      NOT NULL,
        url          NVARCHAR(MAX)      NOT NULL,
        method       NVARCHAR(8)        NOT NULL CONSTRAINT DF_webhook_method DEFAULT 'POST',
        auth         NVARCHAR(MAX)      NULL,
        timeout_secs INT                NOT NULL CONSTRAINT DF_webhook_timeout DEFAULT 30,
        created_at   DATETIMEOFFSET(6)  NOT NULL CONSTRAINT DF_webhook_created DEFAULT SYSDATETIMEOFFSET(),
        updated_at   DATETIMEOFFSET(6)  NOT NULL CONSTRAINT DF_webhook_updated DEFAULT SYSDATETIMEOFFSET(),
        CONSTRAINT PK_audit_webhook_target_config PRIMARY KEY (id),
        CONSTRAINT UQ_webhook_target_entity_type  UNIQUE (entity_type)
    );
    CREATE INDEX idx_webhook_target_entity_type
        ON audit_webhook_target_config (entity_type);
END
GO
