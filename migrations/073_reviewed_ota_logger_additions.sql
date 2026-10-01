-- The reviewed OTA full-card path may add only an explicitly approved Logger
-- pair. Keep its approval and pair digest alongside the existing immutable
-- complete-card manifests; historical OTA-only records remain unchanged.
ALTER TABLE ota_pricing_drafts
    ADD COLUMN logger_approval_reference TEXT,
    ADD COLUMN logger_rate_set_sha256 CHAR(64),
    ADD CONSTRAINT ota_draft_logger_approval_pair CHECK (
        (logger_approval_reference IS NULL AND logger_rate_set_sha256 IS NULL) OR
        (logger_approval_reference IS NOT NULL AND logger_rate_set_sha256 IS NOT NULL
         AND btrim(logger_approval_reference) <> ''
         AND logger_approval_reference = btrim(logger_approval_reference)
         AND logger_rate_set_sha256 ~ '^[0-9a-f]{64}$')
    );

ALTER TABLE ota_pricing_publications
    ADD COLUMN logger_approval_reference TEXT,
    ADD COLUMN logger_rate_set_sha256 CHAR(64),
    ADD CONSTRAINT ota_publication_logger_approval_pair CHECK (
        (logger_approval_reference IS NULL AND logger_rate_set_sha256 IS NULL) OR
        (logger_approval_reference IS NOT NULL AND logger_rate_set_sha256 IS NOT NULL
         AND btrim(logger_approval_reference) <> ''
         AND logger_approval_reference = btrim(logger_approval_reference)
         AND logger_rate_set_sha256 ~ '^[0-9a-f]{64}$')
    );
