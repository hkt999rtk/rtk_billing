-- Advisory, current-owner financial observations. These do not authorize a
-- closure or an ownership commit, which require their own fenced protocol.
CREATE TABLE billing_cloud_preflight_receipts (
    id UUID PRIMARY KEY,
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    owner_user_id UUID NOT NULL,
    ownership_version BIGINT NOT NULL CHECK (ownership_version > 0),
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    state_sha256 TEXT NOT NULL CHECK (state_sha256 ~ '^[0-9a-f]{64}$'),
    usage_checkpoint_sha256 TEXT CHECK (usage_checkpoint_sha256 ~ '^[0-9a-f]{64}$'),
    invoice_checkpoint_sha256 TEXT CHECK (invoice_checkpoint_sha256 ~ '^[0-9a-f]{64}$'),
    provider_checkpoint_sha256 TEXT CHECK (provider_checkpoint_sha256 ~ '^[0-9a-f]{64}$'),
    financial_evidence JSONB NOT NULL CHECK (jsonb_typeof(financial_evidence) = 'object'),
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (expires_at > observed_at AND expires_at <= observed_at + interval '5 minutes')
);
CREATE INDEX billing_cloud_preflight_latest ON billing_cloud_preflight_receipts
    (account_id, ownership_version, observed_at DESC, created_at DESC, id);
CREATE TRIGGER billing_cloud_preflight_receipts_immutable BEFORE UPDATE OR DELETE ON billing_cloud_preflight_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
