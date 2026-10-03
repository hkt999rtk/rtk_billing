-- A controller may cancel a locally journaled intent that Billing has never
-- accepted. This is not a Logger terminal receipt and never grants deletion.
ALTER TABLE billing_raw_retention_operations
    ADD COLUMN decision_origin TEXT NOT NULL DEFAULT 'accepted'
        CHECK(decision_origin IN ('accepted','cancelled-before-acceptance'));
ALTER TABLE billing_raw_retention_operations
    DROP CONSTRAINT billing_raw_retention_operations_check1;
ALTER TABLE billing_raw_retention_operations
    ADD CONSTRAINT billing_raw_retention_terminal_provenance CHECK(
        (decision_origin='accepted' AND
            ((status IN ('COMPLETED','ABORTED'))=(terminal_receipt IS NOT NULL)))
        OR (decision_origin='cancelled-before-acceptance' AND status='ABORTED'
            AND terminal_receipt IS NULL AND consumer_proofs='[]'::jsonb)
    );

CREATE OR REPLACE FUNCTION guard_raw_retention_operation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' OR NEW.operation_id<>OLD.operation_id OR NEW.environment<>OLD.environment
       OR NEW.store_id<>OLD.store_id OR NEW.from_sequence<>OLD.from_sequence OR NEW.through_sequence<>OLD.through_sequence
       OR NEW.plan_sha256<>OLD.plan_sha256 OR NEW.request_sha256<>OLD.request_sha256 OR NEW.plan<>OLD.plan
       OR NEW.consumer_proofs<>OLD.consumer_proofs OR NEW.created_at<>OLD.created_at
       OR NEW.decision_origin<>OLD.decision_origin
       OR OLD.status IN ('COMPLETED','ABORTED')
       OR (OLD.status='ABORT_REQUESTED' AND NEW.status='ACTIVE')
    THEN RAISE EXCEPTION 'raw retention decision is immutable or cannot regress' USING ERRCODE='23514'; END IF;
    RETURN NEW;
END $$;

UPDATE schema_metadata SET details=details||
    '{"scenario":"Durable one-per-store fence released only by authenticated Logger terminal evidence; separately identified never-accepted cancellations cannot authorize deletion.","source":"repos/rtk_billing/migrations/071_raw_retention_unaccepted_cancellation.sql"}'::jsonb
WHERE object_kind='table' AND schema_name='public' AND object_name='billing_raw_retention_operations';
