-- Preserve the original Product OTA authorization with accepted source facts.
-- Old facts remain audit-only until a future, explicitly reviewed OTA cutover.
ALTER TABLE billing_usage_facts
    ADD COLUMN ota_grant_revision BIGINT,
    ADD COLUMN ota_grant_sha256 CHAR(64),
    ADD COLUMN ota_grant_authorized_at TIMESTAMPTZ;

ALTER TABLE billing_usage_facts
    ADD CONSTRAINT billing_usage_ota_grant_shape_check CHECK (
        (ota_grant_revision IS NULL AND ota_grant_sha256 IS NULL AND ota_grant_authorized_at IS NULL)
        OR (service_code = 'ota' AND metric_code IN ('device_task', 'successful_download_gib', 'artifact_write')
            AND ota_grant_revision IS NOT NULL AND ota_grant_revision > 0
            AND ota_grant_sha256 IS NOT NULL AND ota_grant_sha256 ~ '^[0-9a-f]{64}$'
            AND ota_grant_authorized_at IS NOT NULL)
    );
