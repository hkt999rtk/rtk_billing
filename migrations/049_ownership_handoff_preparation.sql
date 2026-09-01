-- Account Manager remains the ownership authority. No historical owner is
-- inferred or backfilled by this migration; bootstrap needs reviewed evidence.
CREATE TABLE billing_responsibility_periods (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    owner_user_id UUID NOT NULL,
    ownership_version BIGINT NOT NULL CHECK (ownership_version > 0),
    effective_from TIMESTAMPTZ NOT NULL,
    effective_until TIMESTAMPTZ,
    source_evidence_sha256 TEXT NOT NULL CHECK (source_evidence_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (effective_until IS NULL OR effective_until > effective_from),
    UNIQUE (account_id, ownership_version),
    UNIQUE (id, account_id, owner_user_id, ownership_version)
);
CREATE UNIQUE INDEX billing_responsibility_current ON billing_responsibility_periods(account_id)
    WHERE effective_until IS NULL;

CREATE TABLE billing_ownership_handoffs (
    id UUID PRIMARY KEY,
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    source_period_id UUID NOT NULL,
    source_user_id UUID NOT NULL,
    target_user_id UUID NOT NULL,
    ownership_version BIGINT NOT NULL,
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    cutoff TIMESTAMPTZ NOT NULL,
    phase TEXT NOT NULL DEFAULT 'preparing' CHECK (phase IN
        ('preparing', 'prepared', 'commit_authorized', 'finalizing', 'finalized', 'abort_pending', 'aborted')),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (source_user_id <> target_user_id),
    FOREIGN KEY (source_period_id, account_id, source_user_id, ownership_version)
        REFERENCES billing_responsibility_periods(id, account_id, owner_user_id, ownership_version) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX billing_handoff_one_active_per_account ON billing_ownership_handoffs(account_id)
    WHERE phase NOT IN ('finalized', 'aborted');

-- A pending setup invalidated by preparation is never usable again, even after
-- cancellation. Keep its provider reconciliation evidence without restoring it.
ALTER TABLE payment_method_setup_sessions ADD COLUMN invalidated_by_handoff UUID
    REFERENCES billing_ownership_handoffs(id) ON DELETE RESTRICT;
CREATE TABLE billing_handoff_setup_observations (
    session_id UUID NOT NULL REFERENCES payment_method_setup_sessions(id) ON DELETE RESTRICT,
    result_sha256 TEXT NOT NULL CHECK (result_sha256 ~ '^[0-9a-f]{64}$'),
    provider_state TEXT NOT NULL CHECK (provider_state IN ('requires_action', 'succeeded', 'failed')),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, result_sha256)
);

COMMENT ON TABLE billing_ownership_handoffs IS
    'Durable monetary fence. Preparing is NOT settlement proof or permission to commit ownership. No timer releases the fence.';
COMMENT ON TABLE billing_responsibility_periods IS
    'Evidence-backed Account Manager responsibility projection, not tenant-editable ownership. Historical gaps remain unknown.';
