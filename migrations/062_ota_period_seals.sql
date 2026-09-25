-- Period seals are trusted source evidence, not invoice lines. Both the
-- Platform grant history and the OTA producer must attest every UTC month,
-- including an explicitly empty month, before an OTA-priced period can close.
CREATE TABLE ota_period_seals (
    organization_id UUID NOT NULL,
    period_start TIMESTAMPTZ NOT NULL,
    period_end TIMESTAMPTZ NOT NULL,
    issuer_kind TEXT NOT NULL CHECK (issuer_kind IN ('platform_grants', 'ota_producer')),
    seal_id UUID NOT NULL UNIQUE,
    source_high_water JSONB NOT NULL CHECK (source_high_water <> 'null'::jsonb),
    product_ids JSONB NOT NULL CHECK (jsonb_typeof(product_ids) = 'array'),
    metric_counts JSONB NOT NULL CHECK (jsonb_typeof(metric_counts) = 'object'),
    fact_set_sha256 CHAR(64),
    source_sha256 CHAR(64) NOT NULL CHECK (source_sha256 ~ '^[0-9a-f]{64}$'),
    sealed_at TIMESTAMPTZ NOT NULL,
    content_sha256 CHAR(64) NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, period_start, period_end, issuer_kind),
    CHECK (period_end > period_start),
    CHECK ((issuer_kind = 'platform_grants' AND fact_set_sha256 IS NULL) OR
           (issuer_kind = 'ota_producer' AND fact_set_sha256 ~ '^[0-9a-f]{64}$'))
);

CREATE FUNCTION reject_ota_period_seal_mutation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'OTA period seal is immutable' USING ERRCODE='23514';
END $$;

CREATE TRIGGER ota_period_seal_immutable BEFORE UPDATE OR DELETE ON ota_period_seals
    FOR EACH ROW EXECUTE FUNCTION reject_ota_period_seal_mutation();
