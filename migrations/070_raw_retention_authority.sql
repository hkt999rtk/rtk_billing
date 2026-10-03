-- Raw billing evidence is retained in Logger, not in these authority tables.
-- One durable fence per logical store survives controller crashes and timeout.
CREATE TABLE billing_raw_retention_policies (
    policy_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version>0),
    environment TEXT NOT NULL CHECK(environment IN ('dev','staging','prod')),
    store_id TEXT NOT NULL CHECK(store_id ~ '^[a-f0-9]{32}$'),
    specification JSONB NOT NULL,
    specification_sha256 TEXT NOT NULL CHECK(specification_sha256 ~ '^[a-f0-9]{64}$'),
    recovery_approval_ref TEXT,
    active BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(policy_id,version)
);
CREATE UNIQUE INDEX billing_raw_retention_active_policy
    ON billing_raw_retention_policies(environment,store_id) WHERE active;
CREATE TABLE billing_raw_retention_clearances (
    clearance_id TEXT PRIMARY KEY,
    policy_id TEXT NOT NULL,
    policy_version BIGINT NOT NULL,
    environment TEXT NOT NULL,
    store_id TEXT NOT NULL,
    from_sequence BIGINT NOT NULL CHECK(from_sequence>0),
    through_sequence BIGINT NOT NULL CHECK(through_sequence>=from_sequence),
    attestation JSONB NOT NULL,
    attestation_sha256 TEXT NOT NULL CHECK(attestation_sha256 ~ '^[a-f0-9]{64}$'),
    revoked BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY(policy_id,policy_version) REFERENCES billing_raw_retention_policies(policy_id,version)
);
CREATE TABLE billing_raw_retention_operations (
    operation_id TEXT PRIMARY KEY,
    environment TEXT NOT NULL,
    store_id TEXT NOT NULL,
    from_sequence BIGINT NOT NULL CHECK(from_sequence>0),
    through_sequence BIGINT NOT NULL CHECK(through_sequence>=from_sequence),
    plan_sha256 TEXT NOT NULL CHECK(plan_sha256 ~ '^[a-f0-9]{64}$'),
    request_sha256 TEXT NOT NULL CHECK(request_sha256 ~ '^[a-f0-9]{64}$'),
    plan JSONB NOT NULL,
    consumer_proofs JSONB NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('ACTIVE','ABORT_REQUESTED','COMPLETED','ABORTED')),
    terminal_receipt JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    resolved_at TIMESTAMPTZ,
    CHECK((status IN ('COMPLETED','ABORTED'))=(terminal_receipt IS NOT NULL)),
    CHECK((status IN ('COMPLETED','ABORTED'))=(resolved_at IS NOT NULL))
);
CREATE UNIQUE INDEX billing_raw_retention_one_fence
    ON billing_raw_retention_operations(environment,store_id) WHERE status IN ('ACTIVE','ABORT_REQUESTED');
CREATE TABLE billing_raw_retention_holds (
    hold_id TEXT PRIMARY KEY,
    environment TEXT NOT NULL,
    store_id TEXT NOT NULL,
    from_sequence BIGINT NOT NULL CHECK(from_sequence>=0),
    through_sequence BIGINT NOT NULL CHECK(through_sequence>=from_sequence),
    request JSONB NOT NULL,
    request_sha256 TEXT NOT NULL CHECK(request_sha256 ~ '^[a-f0-9]{64}$'),
    status TEXT NOT NULL CHECK(status IN ('ACTIVE','PENDING_FENCED','RELEASE_PENDING','RELEASED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK((from_sequence=0)=(through_sequence=0))
);
CREATE TABLE billing_raw_retention_audit (
    id BIGSERIAL PRIMARY KEY,
    environment TEXT NOT NULL,
    store_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    action TEXT NOT NULL,
    binding_sha256 TEXT NOT NULL CHECK(binding_sha256 ~ '^[a-f0-9]{64}$'),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION guard_raw_retention_operation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR NEW.operation_id<>OLD.operation_id OR NEW.environment<>OLD.environment
       OR NEW.store_id<>OLD.store_id OR NEW.from_sequence<>OLD.from_sequence OR NEW.through_sequence<>OLD.through_sequence
       OR NEW.plan_sha256<>OLD.plan_sha256 OR NEW.request_sha256<>OLD.request_sha256 OR NEW.plan<>OLD.plan
       OR NEW.consumer_proofs<>OLD.consumer_proofs OR NEW.created_at<>OLD.created_at
       OR OLD.status IN ('COMPLETED','ABORTED')
       OR (OLD.status='ABORT_REQUESTED' AND NEW.status='ACTIVE')
    THEN RAISE EXCEPTION 'raw retention decision is immutable or cannot regress' USING ERRCODE='23514'; END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER billing_raw_retention_operation_progress BEFORE UPDATE OR DELETE ON billing_raw_retention_operations
    FOR EACH ROW EXECUTE FUNCTION guard_raw_retention_operation();
CREATE FUNCTION guard_raw_retention_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR NEW.policy_id<>OLD.policy_id OR NEW.version<>OLD.version
       OR NEW.environment<>OLD.environment OR NEW.store_id<>OLD.store_id OR NEW.specification<>OLD.specification
       OR NEW.specification_sha256<>OLD.specification_sha256 OR NEW.created_at<>OLD.created_at
       OR (OLD.recovery_approval_ref IS NOT NULL AND NEW.recovery_approval_ref IS DISTINCT FROM OLD.recovery_approval_ref)
    THEN RAISE EXCEPTION 'raw retention policy identity is immutable' USING ERRCODE='23514'; END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER billing_raw_retention_policy_progress BEFORE UPDATE OR DELETE ON billing_raw_retention_policies
    FOR EACH ROW EXECUTE FUNCTION guard_raw_retention_policy();
CREATE FUNCTION guard_raw_retention_clearance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR ROW(NEW.clearance_id,NEW.policy_id,NEW.policy_version,NEW.environment,NEW.store_id,
       NEW.from_sequence,NEW.through_sequence,NEW.attestation,NEW.attestation_sha256,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.clearance_id,OLD.policy_id,OLD.policy_version,OLD.environment,OLD.store_id,
       OLD.from_sequence,OLD.through_sequence,OLD.attestation,OLD.attestation_sha256,OLD.created_at)
       OR (OLD.revoked AND NOT NEW.revoked)
    THEN RAISE EXCEPTION 'raw retention clearance cannot be rewritten or restored' USING ERRCODE='23514'; END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER billing_raw_retention_clearance_progress BEFORE UPDATE OR DELETE ON billing_raw_retention_clearances
    FOR EACH ROW EXECUTE FUNCTION guard_raw_retention_clearance();
CREATE FUNCTION reject_raw_retention_audit_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'raw retention audit is immutable' USING ERRCODE='23514'; END $$;
CREATE TRIGGER billing_raw_retention_audit_immutable BEFORE UPDATE OR DELETE ON billing_raw_retention_audit
    FOR EACH ROW EXECUTE FUNCTION reject_raw_retention_audit_mutation();
CREATE FUNCTION guard_raw_retention_hold() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR ROW(NEW.hold_id,NEW.environment,NEW.store_id,NEW.from_sequence,NEW.through_sequence,
       NEW.request,NEW.request_sha256,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.hold_id,OLD.environment,OLD.store_id,OLD.from_sequence,OLD.through_sequence,
       OLD.request,OLD.request_sha256,OLD.created_at)
       OR (OLD.status='RELEASED' AND NEW.status<>'RELEASED')
    THEN RAISE EXCEPTION 'raw retention hold identity is immutable' USING ERRCODE='23514'; END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER billing_raw_retention_hold_progress BEFORE UPDATE OR DELETE ON billing_raw_retention_holds
    FOR EACH ROW EXECUTE FUNCTION guard_raw_retention_hold();

INSERT INTO schema_metadata(object_kind,schema_name,object_name,details) VALUES
('table','public','billing_raw_retention_policies','{"group":"Raw billing retention","scenario":"Versioned financial and independent recovery approvals for automatic hot-payload retirement.","service":"Billing","source":"repos/rtk_billing/migrations/070_raw_retention_authority.sql"}'),
('table','public','billing_raw_retention_clearances','{"group":"Raw billing retention","scenario":"Explicit source-complete and reconciled period attestations; closed state alone is insufficient.","service":"Billing","source":"repos/rtk_billing/migrations/070_raw_retention_authority.sql"}'),
('table','public','billing_raw_retention_operations','{"group":"Raw billing retention","scenario":"Durable one-per-store fence released only by an exact authenticated Logger terminal receipt.","service":"Billing","source":"repos/rtk_billing/migrations/070_raw_retention_authority.sql"}'),
('table','public','billing_raw_retention_holds','{"group":"Raw billing retention","scenario":"Financial/legal holds, including requests queued after an irrevocable cleanup decision.","service":"Billing","source":"repos/rtk_billing/migrations/070_raw_retention_authority.sql"}'),
('table','public','billing_raw_retention_audit','{"group":"Raw billing retention","scenario":"Append-only authorization and decision binding receipts without credentials or raw tenant payloads.","service":"Billing","source":"repos/rtk_billing/migrations/070_raw_retention_authority.sql"}')
ON CONFLICT(object_kind,schema_name,object_name) DO UPDATE SET details=EXCLUDED.details;
