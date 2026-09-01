-- An HTTP rejection alone cannot retire a delayed command. Persist retirement
-- under the same account lock as closure and reject every later replay.
CREATE TABLE billing_cloud_closure_retired_commands (
    operation_id UUID NOT NULL REFERENCES billing_cloud_closures(id) ON DELETE RESTRICT,
    request_sha256 TEXT NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
    settlement_id UUID NOT NULL,
    am_readiness_sha256 TEXT NOT NULL CHECK(am_readiness_sha256 ~ '^[0-9a-f]{64}$'),
    receipt_sha256 TEXT NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
    retired_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(operation_id,request_sha256)
);
CREATE TRIGGER closure_retirement_immutable BEFORE UPDATE OR DELETE ON billing_cloud_closure_retired_commands
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE FUNCTION guard_cloud_closure_command_retirement() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE account UUID;
BEGIN
    SELECT account_id INTO account FROM billing_cloud_closures WHERE id=NEW.operation_id;
    PERFORM id FROM commercial_accounts WHERE id=account FOR UPDATE;
    IF EXISTS(SELECT 1 FROM billing_cloud_closure_completions WHERE operation_id=NEW.operation_id)
        OR NOT EXISTS(SELECT 1 FROM billing_cloud_closures WHERE id=NEW.operation_id AND phase='preparing') THEN
        RAISE EXCEPTION 'closed or canceled command cannot be retired' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER cloud_closure_retirement_guard BEFORE INSERT ON billing_cloud_closure_retired_commands FOR EACH ROW EXECUTE FUNCTION guard_cloud_closure_command_retirement();
CREATE FUNCTION reject_retired_cloud_closure_completion() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE account UUID;
BEGIN
    SELECT account_id INTO account FROM billing_cloud_closures WHERE id=NEW.operation_id;
    PERFORM id FROM commercial_accounts WHERE id=account FOR UPDATE;
    IF EXISTS(SELECT 1 FROM billing_cloud_closure_retired_commands WHERE operation_id=NEW.operation_id AND request_sha256=NEW.request_sha256) THEN
        RAISE EXCEPTION 'retired command cannot close Billing' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER retired_cloud_closure_completion BEFORE INSERT ON billing_cloud_closure_completions FOR EACH ROW EXECUTE FUNCTION reject_retired_cloud_closure_completion();
