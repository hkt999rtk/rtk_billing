-- New-cloud provisioning only. Never backfill historical responsibility from
-- the current owner; legacy accounts require separately reviewed migration.
CREATE TABLE billing_cloud_creation_receipts (
    event_id UUID PRIMARY KEY,
    organization_id UUID NOT NULL UNIQUE,
    account_id UUID NOT NULL UNIQUE REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    owner_user_id UUID NOT NULL,
    ownership_version BIGINT NOT NULL CHECK (ownership_version=1),
    occurred_at TIMESTAMPTZ NOT NULL,
    evidence_sha256 TEXT NOT NULL CHECK (evidence_sha256 ~ '^[a-f0-9]{64}$'),
    received_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER billing_cloud_creation_receipts_immutable BEFORE UPDATE OR DELETE
ON billing_cloud_creation_receipts FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
