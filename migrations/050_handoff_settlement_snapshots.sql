-- Evidence and confirmations are append-only protocol records. Receipt hashes
-- name independently reconciled producer/invoice/provider checkpoints, never
-- tenant assertions or a guessed "no rows means complete" checkpoint.
CREATE TABLE billing_handoff_settlement_receipts (
    id UUID PRIMARY KEY,
    operation_id UUID NOT NULL REFERENCES billing_ownership_handoffs(id) ON DELETE RESTRICT,
    operation_version BIGINT NOT NULL CHECK (operation_version > 1),
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    state_sha256 TEXT NOT NULL CHECK (state_sha256 ~ '^[0-9a-f]{64}$'),
    usage_checkpoint_sha256 TEXT CHECK (usage_checkpoint_sha256 ~ '^[0-9a-f]{64}$'),
    invoice_checkpoint_sha256 TEXT CHECK (invoice_checkpoint_sha256 ~ '^[0-9a-f]{64}$'),
    provider_checkpoint_sha256 TEXT CHECK (provider_checkpoint_sha256 ~ '^[0-9a-f]{64}$'),
    financial_evidence JSONB NOT NULL CHECK (jsonb_typeof(financial_evidence) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (operation_id, operation_version),
    UNIQUE (id, operation_id, operation_version)
);

CREATE TABLE billing_handoff_balance_snapshots (
    operation_id UUID NOT NULL,
    version BIGINT NOT NULL,
    receipt_id UUID NOT NULL,
    balance_minor BIGINT NOT NULL CHECK (balance_minor >= 0),
    currency TEXT NOT NULL CHECK (currency = 'TWD'),
    cutoff TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (operation_id, version),
    FOREIGN KEY (receipt_id, operation_id, version)
        REFERENCES billing_handoff_settlement_receipts(id, operation_id, operation_version) ON DELETE RESTRICT
);

CREATE TABLE billing_handoff_confirmations (
    operation_id UUID NOT NULL,
    snapshot_version BIGINT NOT NULL,
    user_id UUID NOT NULL,
    confirmed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (operation_id, snapshot_version, user_id),
    FOREIGN KEY (operation_id, snapshot_version)
        REFERENCES billing_handoff_balance_snapshots(operation_id, version) ON DELETE RESTRICT
);

COMMENT ON TABLE billing_handoff_settlement_receipts IS
    'Trusted collector evidence bound to a specific local financial-state digest and frozen operation cutoff. Does not authorize owner commit.';

CREATE FUNCTION reject_handoff_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = '23514';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER handoff_receipts_append_only BEFORE UPDATE OR DELETE ON billing_handoff_settlement_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER handoff_snapshots_append_only BEFORE UPDATE OR DELETE ON billing_handoff_balance_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER handoff_confirmations_append_only BEFORE UPDATE OR DELETE ON billing_handoff_confirmations
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
