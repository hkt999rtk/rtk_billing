-- Metadata-only follow-up. Do not rewrite the applied 070/071 migrations.
INSERT INTO schema_metadata(object_kind,schema_name,object_name,details) VALUES
('group','public','Raw billing retention',
 '{"purpose":"Authoritative financial and recovery decisions for verified Logger raw-data retirement.","scenario":"Approve automatic retention policies and source-complete financial clearances, preserve legal holds and durable per-store fences, and audit authenticated terminal decisions.","service":"Billing","source":"repos/rtk_billing/migrations/072_raw_retention_schema_metadata.sql"}'::jsonb),
('table','public','billing_raw_retention_policies',
 '{"group":"Raw billing retention","purpose":"Immutable, versioned financial policies and independent recovery approvals for raw-payload retirement.","scenario":"Authorize new automatic retirement admissions only while the exact policy version is active and recovery-approved; deactivation does not release accepted fences.","service":"Billing","source":"repos/rtk_billing/migrations/072_raw_retention_schema_metadata.sql"}'::jsonb),
('table','public','billing_raw_retention_clearances',
 '{"group":"Raw billing retention","purpose":"Immutable financial attestations binding an exact policy version to a source-complete, reconciled Logger sequence range.","scenario":"Permit automatic raw-data retirement only after explicit period clearance; closed period state or an empty outbox alone is insufficient.","service":"Billing","source":"repos/rtk_billing/migrations/072_raw_retention_schema_metadata.sql"}'::jsonb),
('table','public','billing_raw_retention_operations',
 '{"group":"Raw billing retention","purpose":"Crash-safe retirement decisions, exact evidence bindings and permanent terminal provenance.","scenario":"Maintain one ACTIVE or ABORT_REQUESTED fence per environment/store until authenticated Logger terminal evidence resolves it; separately identified never-accepted cancellations cannot authorize deletion.","service":"Billing","source":"repos/rtk_billing/migrations/072_raw_retention_schema_metadata.sql"}'::jsonb),
('table','public','billing_raw_retention_holds',
 '{"group":"Raw billing retention","purpose":"Durable financial or legal holds protecting raw-data archive evidence and decryption keys.","scenario":"Block new overlapping retirement admissions and record holds arriving after an accepted fence as PENDING_FENCED without reversing the earlier local retirement decision.","service":"Billing","source":"repos/rtk_billing/migrations/072_raw_retention_schema_metadata.sql"}'::jsonb),
('table','public','billing_raw_retention_audit',
 '{"group":"Raw billing retention","purpose":"Append-only authorization, approval and retirement decision binding records.","scenario":"Audit policy approvals, clearances, holds and immutable operation outcomes without storing credentials or raw tenant payloads.","service":"Billing","source":"repos/rtk_billing/migrations/072_raw_retention_schema_metadata.sql"}'::jsonb)
ON CONFLICT(object_kind,schema_name,object_name) DO UPDATE SET details=EXCLUDED.details;

COMMENT ON TABLE billing_raw_retention_policies IS 'Immutable, versioned financial policies and independent recovery approvals for raw-payload retirement.';
COMMENT ON TABLE billing_raw_retention_clearances IS 'Immutable financial attestations binding an exact policy version to a source-complete, reconciled Logger sequence range.';
COMMENT ON TABLE billing_raw_retention_operations IS 'Crash-safe retirement decisions, exact evidence bindings and permanent terminal provenance.';
COMMENT ON TABLE billing_raw_retention_holds IS 'Durable financial or legal holds protecting raw-data archive evidence and decryption keys.';
COMMENT ON TABLE billing_raw_retention_audit IS 'Append-only authorization, approval and retirement decision binding records.';

INSERT INTO schema_metadata(object_kind,schema_name,object_name,details) VALUES
('column','public','billing_raw_retention_policies.policy_id',
 '{"critical":"key","description":"Stable financial policy identifier; policy_id and version form the immutable composite primary key."}'::jsonb),
('column','public','billing_raw_retention_policies.version',
 '{"critical":"key","description":"Positive immutable policy version; paired with policy_id and referenced by the clearance composite foreign key."}'::jsonb),
('column','public','billing_raw_retention_policies.active',
 '{"semantic_kind":"state","description":"Admission switch: false blocks new operations only; existing ACTIVE or ABORT_REQUESTED fences remain recoverable."}'::jsonb),
('column','public','billing_raw_retention_clearances.clearance_id',
 '{"critical":"key","description":"Immutable primary key of the explicit financial source-complete and reconciled range attestation."}'::jsonb),
('column','public','billing_raw_retention_clearances.policy_id',
 '{"critical":"key","description":"References billing_raw_retention_policies.policy_id together with policy_version; the approved policy identity cannot be rewritten."}'::jsonb),
('column','public','billing_raw_retention_clearances.policy_version',
 '{"critical":"key","description":"References billing_raw_retention_policies.version together with policy_id; clearance cannot silently adopt a later policy version."}'::jsonb),
('column','public','billing_raw_retention_clearances.revoked',
 '{"semantic_kind":"state","description":"One-way revocation flag for new admissions; revocation cannot regress or release an accepted retirement fence."}'::jsonb),
('column','public','billing_raw_retention_operations.operation_id',
 '{"critical":"key","description":"Permanent primary key and idempotency identity of one retirement decision; ambiguous retries reuse the exact operation and plan."}'::jsonb),
('column','public','billing_raw_retention_operations.status',
 '{"semantic_kind":"state","description":"ACTIVE and ABORT_REQUESTED retain the per-store fence; COMPLETED and ABORTED are immutable terminal states governed by decision_origin."}'::jsonb),
('column','public','billing_raw_retention_operations.decision_origin',
 '{"semantic_kind":"state","description":"accepted requires authenticated Logger evidence at termination; cancelled-before-acceptance is a permanent local ABORTED tombstone with no Logger receipt and no deletion authority."}'::jsonb),
('column','public','billing_raw_retention_holds.hold_id',
 '{"critical":"key","description":"Permanent primary key and immutable idempotency identity of a financial or legal hold request."}'::jsonb),
('column','public','billing_raw_retention_holds.status',
 '{"semantic_kind":"state","description":"ACTIVE, PENDING_FENCED and RELEASE_PENDING block new overlapping admissions and protect archive/key evidence; RELEASED is final, and PENDING_FENCED cannot reverse an earlier accepted local operation."}'::jsonb),
('column','public','billing_raw_retention_audit.id',
 '{"critical":"key","description":"Database-assigned primary key of an append-only authorization or decision binding audit record."}'::jsonb)
ON CONFLICT(object_kind,schema_name,object_name) DO UPDATE SET details=EXCLUDED.details;

COMMENT ON COLUMN billing_raw_retention_policies.policy_id IS 'Stable financial policy identifier; policy_id and version form the immutable composite primary key.';
COMMENT ON COLUMN billing_raw_retention_policies.version IS 'Positive immutable policy version; paired with policy_id and referenced by the clearance composite foreign key.';
COMMENT ON COLUMN billing_raw_retention_policies.active IS 'Admission switch: false blocks new operations only; existing ACTIVE or ABORT_REQUESTED fences remain recoverable.';
COMMENT ON COLUMN billing_raw_retention_clearances.clearance_id IS 'Immutable primary key of the explicit financial source-complete and reconciled range attestation.';
COMMENT ON COLUMN billing_raw_retention_clearances.policy_id IS 'References billing_raw_retention_policies.policy_id together with policy_version; the approved policy identity cannot be rewritten.';
COMMENT ON COLUMN billing_raw_retention_clearances.policy_version IS 'References billing_raw_retention_policies.version together with policy_id; clearance cannot silently adopt a later policy version.';
COMMENT ON COLUMN billing_raw_retention_clearances.revoked IS 'One-way revocation flag for new admissions; revocation cannot regress or release an accepted retirement fence.';
COMMENT ON COLUMN billing_raw_retention_operations.operation_id IS 'Permanent primary key and idempotency identity of one retirement decision; ambiguous retries reuse the exact operation and plan.';
COMMENT ON COLUMN billing_raw_retention_operations.status IS 'ACTIVE and ABORT_REQUESTED retain the per-store fence; COMPLETED and ABORTED are immutable terminal states governed by decision_origin.';
COMMENT ON COLUMN billing_raw_retention_operations.decision_origin IS 'accepted requires authenticated Logger evidence at termination; cancelled-before-acceptance is a permanent local ABORTED tombstone with no Logger receipt and no deletion authority.';
COMMENT ON COLUMN billing_raw_retention_holds.hold_id IS 'Permanent primary key and immutable idempotency identity of a financial or legal hold request.';
COMMENT ON COLUMN billing_raw_retention_holds.status IS 'ACTIVE, PENDING_FENCED and RELEASE_PENDING block new overlapping admissions and protect archive/key evidence; RELEASED is final, and PENDING_FENCED cannot reverse an earlier accepted local operation.';
COMMENT ON COLUMN billing_raw_retention_audit.id IS 'Database-assigned primary key of an append-only authorization or decision binding audit record.';
