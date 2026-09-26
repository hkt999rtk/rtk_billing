-- Chargeable OTA storage facts carry one bounded physical object contribution.
-- Legacy Product/month aggregates remain in the ledger but cannot satisfy
-- the future priced-close evidence check.
ALTER TABLE billing_usage_facts
    ADD COLUMN ota_storage_object_sha256 CHAR(64),
    ADD COLUMN ota_storage_byte_microseconds TEXT;

ALTER TABLE billing_usage_facts DROP CONSTRAINT billing_usage_ota_grant_shape_check;
ALTER TABLE billing_usage_facts
    ADD CONSTRAINT billing_usage_ota_grant_shape_check CHECK (
        (ota_grant_revision IS NULL AND ota_grant_sha256 IS NULL AND ota_grant_authorized_at IS NULL)
        OR (service_code = 'ota' AND
            (metric_code IN ('device_task', 'successful_download_gib', 'artifact_write')
             OR (metric_code = 'artifact_storage_gib_month' AND ota_storage_object_sha256 IS NOT NULL))
            AND ota_grant_revision IS NOT NULL AND ota_grant_revision > 0
            AND ota_grant_sha256 IS NOT NULL AND ota_grant_sha256 ~ '^[0-9a-f]{64}$'
            AND ota_grant_authorized_at IS NOT NULL)
    );
ALTER TABLE billing_usage_facts
    ADD CONSTRAINT billing_usage_ota_storage_object_shape_check CHECK (
        (ota_storage_object_sha256 IS NULL AND ota_storage_byte_microseconds IS NULL)
        OR (service_code = 'ota' AND metric_code = 'artifact_storage_gib_month'
            AND ota_storage_object_sha256 IS NOT NULL
            AND ota_storage_object_sha256 ~ '^[0-9a-f]{64}$'
            AND ota_storage_byte_microseconds IS NOT NULL
            AND ota_storage_byte_microseconds ~ '^(0|[1-9][0-9]{0,39})$'
            AND ota_grant_revision IS NOT NULL)
    );

CREATE UNIQUE INDEX billing_ota_storage_object_month_unique
    ON billing_usage_facts (organization_id, window_start, ota_storage_object_sha256)
    WHERE service_code = 'ota' AND metric_code = 'artifact_storage_gib_month'
      AND ota_storage_object_sha256 IS NOT NULL;
