-- Producer freezes and authenticates a complete shared-ledger UTC month.
-- This is pre-issue source evidence, not a financial settlement checkpoint.
CREATE TABLE logger_period_seals (
    organization_id UUID NOT NULL,
    period_start TIMESTAMPTZ NOT NULL,
    period_end TIMESTAMPTZ NOT NULL,
    issuer_kind TEXT NOT NULL CHECK (issuer_kind='logger_producer'),
    seal_id UUID NOT NULL UNIQUE,
    source_high_water JSONB NOT NULL CHECK (jsonb_typeof(source_high_water)='object' AND source_high_water<>'{}'::jsonb),
    product_ids JSONB NOT NULL CHECK (jsonb_typeof(product_ids)='array'),
    metric_counts JSONB NOT NULL CHECK (jsonb_typeof(metric_counts)='object'),
    fact_set_sha256 CHAR(64) NOT NULL CHECK (fact_set_sha256 ~ '^[0-9a-f]{64}$'),
    source_sha256 CHAR(64) NOT NULL CHECK (source_sha256 ~ '^[0-9a-f]{64}$'),
    sealed_at TIMESTAMPTZ NOT NULL CHECK (sealed_at>=period_end+interval '24 hours'),
    content_sha256 CHAR(64) NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id,period_start,period_end),
    CHECK (period_start=date_trunc('month',period_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'),
    CHECK (period_end=(period_start AT TIME ZONE 'UTC'+interval '1 month') AT TIME ZONE 'UTC')
);
CREATE FUNCTION reject_logger_period_seal_mutation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Logger period seal is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER logger_period_seal_immutable BEFORE UPDATE OR DELETE ON logger_period_seals
    FOR EACH ROW EXECUTE FUNCTION reject_logger_period_seal_mutation();

CREATE FUNCTION enforce_logger_sealed_fact_barrier() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.service_code='logger' AND EXISTS (SELECT 1 FROM logger_period_seals
        WHERE organization_id=NEW.organization_id
        AND tstzrange(period_start,period_end,'[)') && tstzrange(NEW.window_start,NEW.window_end,'[)')) THEN
        RAISE EXCEPTION 'Logger fact overlaps a sealed source month'
            USING ERRCODE='55000',CONSTRAINT='billing_logger_sealed_fact_barrier';
    END IF;
    RETURN NEW;
END $$;
-- Existing closed-cloud/handoff usage triggers first acquire the commercial
-- account lock. This trigger runs afterward and observes any committed seal
-- when an INSERT resumes after waiting on the same row lock.
CREATE TRIGGER logger_sealed_fact_barrier BEFORE INSERT ON billing_usage_facts
    FOR EACH ROW EXECUTE FUNCTION enforce_logger_sealed_fact_barrier();
