-- Provider-neutral commercial settlement. Monetary history is append-only and
-- remains separate from service usage facts and operational logs.

CREATE TABLE IF NOT EXISTS commercial_accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Account Manager owns organizations. Billing stores only the immutable
    -- external UUID and never joins across service databases.
    organization_id UUID NOT NULL,
    currency CHAR(3) NOT NULL,
    available_balance_minor BIGINT NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'active',
    version BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT commercial_accounts_currency_check CHECK (currency = upper(currency) AND currency = 'TWD'),
    CONSTRAINT commercial_accounts_state_check CHECK (state IN ('active', 'attention_required', 'suspended', 'closed')),
    CONSTRAINT commercial_accounts_version_check CHECK (version >= 0),
    CONSTRAINT commercial_accounts_org_currency_key UNIQUE (organization_id, currency)
);

CREATE TABLE IF NOT EXISTS balance_ledger_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    direction TEXT NOT NULL,
    amount_minor BIGINT NOT NULL,
    currency CHAR(3) NOT NULL,
    reason TEXT NOT NULL,
    idempotency_scope TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    external_type TEXT,
    external_id TEXT,
    balance_after_minor BIGINT NOT NULL,
    actor_type TEXT,
    actor_id TEXT,
    request_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT balance_ledger_direction_check CHECK (direction IN ('credit', 'debit')),
    CONSTRAINT balance_ledger_amount_check CHECK (amount_minor > 0),
    CONSTRAINT balance_ledger_currency_check CHECK (currency = upper(currency) AND currency = 'TWD'),
    CONSTRAINT balance_ledger_reason_check CHECK (reason IN (
        'invoice_debit',
        'usage_adjustment_debit',
        'manual_adjustment_debit',
        'payment_top_up_credit',
        'manual_adjustment_credit',
        'refund_debit',
        'chargeback_debit'
    )),
    CONSTRAINT balance_ledger_reason_direction_check CHECK (
        (direction = 'credit' AND reason IN ('payment_top_up_credit', 'manual_adjustment_credit'))
        OR
        (direction = 'debit' AND reason IN ('invoice_debit', 'usage_adjustment_debit', 'manual_adjustment_debit', 'refund_debit', 'chargeback_debit'))
    ),
    CONSTRAINT balance_ledger_idempotency_scope_not_blank CHECK (btrim(idempotency_scope) <> ''),
    CONSTRAINT balance_ledger_idempotency_key_not_blank CHECK (btrim(idempotency_key) <> ''),
    CONSTRAINT balance_ledger_external_pair_check CHECK ((external_type IS NULL) = (external_id IS NULL)),
    CONSTRAINT balance_ledger_account_idempotency_key UNIQUE (account_id, idempotency_scope, idempotency_key)
);

CREATE TABLE IF NOT EXISTS payment_consents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    consent_type TEXT NOT NULL,
    text_version TEXT NOT NULL,
    text_sha256 CHAR(64) NOT NULL,
    accepted_actor_type TEXT NOT NULL,
    accepted_actor_id TEXT NOT NULL,
    accepted_at TIMESTAMPTZ NOT NULL,
    locale TEXT NOT NULL,
    source TEXT NOT NULL,
    revoked_at TIMESTAMPTZ,
    revocation_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_consents_type_check CHECK (consent_type IN ('payment_method', 'auto_topup')),
    CONSTRAINT payment_consents_text_version_not_blank CHECK (btrim(text_version) <> ''),
    CONSTRAINT payment_consents_text_sha256_check CHECK (text_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT payment_consents_actor_not_blank CHECK (btrim(accepted_actor_type) <> '' AND btrim(accepted_actor_id) <> ''),
    CONSTRAINT payment_consents_locale_not_blank CHECK (btrim(locale) <> ''),
    CONSTRAINT payment_consents_source_not_blank CHECK (btrim(source) <> ''),
    CONSTRAINT payment_consents_revocation_check CHECK ((revoked_at IS NULL AND revocation_reason IS NULL) OR (revoked_at IS NOT NULL AND btrim(revocation_reason) <> ''))
);

CREATE TABLE IF NOT EXISTS payment_methods (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    provider TEXT NOT NULL,
    provider_customer_ref_ciphertext BYTEA,
    provider_method_ref_ciphertext BYTEA,
    provider_method_ref_sha256 CHAR(64),
    card_brand TEXT,
    last_four CHAR(4),
    expiry_month SMALLINT,
    expiry_year SMALLINT,
    capabilities JSONB NOT NULL DEFAULT '{}'::jsonb,
    status TEXT NOT NULL DEFAULT 'pending',
    consent_id UUID NOT NULL REFERENCES payment_consents(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_methods_provider_not_blank CHECK (btrim(provider) <> '' AND provider = lower(provider)),
    CONSTRAINT payment_methods_status_check CHECK (status IN ('pending', 'active', 'expired', 'revoked', 'failed')),
    CONSTRAINT payment_methods_ref_sha256_check CHECK (provider_method_ref_sha256 IS NULL OR provider_method_ref_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT payment_methods_last_four_check CHECK (last_four IS NULL OR last_four ~ '^[0-9]{4}$'),
    CONSTRAINT payment_methods_expiry_pair_check CHECK ((expiry_month IS NULL) = (expiry_year IS NULL)),
    CONSTRAINT payment_methods_expiry_month_check CHECK (expiry_month IS NULL OR expiry_month BETWEEN 1 AND 12),
    CONSTRAINT payment_methods_expiry_year_check CHECK (expiry_year IS NULL OR expiry_year BETWEEN 2000 AND 9999),
    CONSTRAINT payment_methods_active_ref_check CHECK (
        status <> 'active'
        OR (provider_customer_ref_ciphertext IS NOT NULL AND provider_method_ref_ciphertext IS NOT NULL AND provider_method_ref_sha256 IS NOT NULL)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS payment_methods_provider_ref_unique_idx
    ON payment_methods (account_id, provider, provider_method_ref_sha256)
    WHERE provider_method_ref_sha256 IS NOT NULL;

CREATE TABLE IF NOT EXISTS auto_topup_policies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL UNIQUE REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    enabled BOOLEAN NOT NULL DEFAULT false,
    threshold_minor BIGINT NOT NULL,
    top_up_amount_minor BIGINT NOT NULL,
    currency CHAR(3) NOT NULL,
    payment_method_id UUID NOT NULL REFERENCES payment_methods(id) ON DELETE RESTRICT,
    daily_attempt_limit INTEGER NOT NULL,
    daily_amount_limit_minor BIGINT NOT NULL,
    cooldown_seconds BIGINT NOT NULL,
    generation BIGINT NOT NULL DEFAULT 1,
    version BIGINT NOT NULL DEFAULT 1,
    armed BOOLEAN NOT NULL DEFAULT true,
    last_triggered_at TIMESTAMPTZ,
    last_succeeded_at TIMESTAMPTZ,
    consent_id UUID NOT NULL REFERENCES payment_consents(id) ON DELETE RESTRICT,
    created_by TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT auto_topup_policy_amounts_check CHECK (threshold_minor > 0 AND top_up_amount_minor > 0 AND daily_amount_limit_minor > 0),
    CONSTRAINT auto_topup_policy_twd_amounts_check CHECK (currency <> 'TWD' OR (threshold_minor % 100 = 0 AND top_up_amount_minor % 100 = 0 AND daily_amount_limit_minor % 100 = 0)),
    CONSTRAINT auto_topup_policy_currency_check CHECK (currency = upper(currency) AND currency = 'TWD'),
    CONSTRAINT auto_topup_policy_attempt_limit_check CHECK (daily_attempt_limit BETWEEN 1 AND 10),
    CONSTRAINT auto_topup_policy_daily_amount_check CHECK (top_up_amount_minor <= daily_amount_limit_minor),
    CONSTRAINT auto_topup_policy_cooldown_check CHECK (cooldown_seconds >= 300),
    CONSTRAINT auto_topup_policy_generation_check CHECK (generation > 0 AND version > 0),
    CONSTRAINT auto_topup_policy_actor_not_blank CHECK (btrim(created_by) <> '' AND btrim(updated_by) <> '')
);

CREATE TABLE IF NOT EXISTS payment_intents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    amount_minor BIGINT NOT NULL,
    currency CHAR(3) NOT NULL,
    reason TEXT NOT NULL,
    policy_generation BIGINT,
    trigger_ledger_entry_id UUID REFERENCES balance_ledger_entries(id) ON DELETE RESTRICT,
    provider TEXT NOT NULL,
    payment_method_id UUID NOT NULL REFERENCES payment_methods(id) ON DELETE RESTRICT,
    state TEXT NOT NULL DEFAULT 'created',
    idempotency_key TEXT NOT NULL,
    merchant_order_reference TEXT,
    provider_transaction_reference TEXT,
    correlation_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    CONSTRAINT payment_intents_amount_check CHECK (amount_minor > 0),
    CONSTRAINT payment_intents_twd_amount_check CHECK (currency <> 'TWD' OR amount_minor % 100 = 0),
    CONSTRAINT payment_intents_currency_check CHECK (currency = upper(currency) AND currency = 'TWD'),
    CONSTRAINT payment_intents_reason_check CHECK (reason IN ('manual_top_up', 'auto_top_up')),
    CONSTRAINT payment_intents_auto_trigger_check CHECK (
        (reason = 'auto_top_up' AND policy_generation IS NOT NULL AND trigger_ledger_entry_id IS NOT NULL)
        OR
        (reason = 'manual_top_up' AND policy_generation IS NULL AND trigger_ledger_entry_id IS NULL)
    ),
    CONSTRAINT payment_intents_provider_not_blank CHECK (btrim(provider) <> '' AND provider = lower(provider)),
    CONSTRAINT payment_intents_state_check CHECK (state IN ('created', 'processing', 'authorized', 'requires_action', 'unknown', 'succeeded', 'failed', 'canceled')),
    CONSTRAINT payment_intents_idempotency_key_not_blank CHECK (btrim(idempotency_key) <> ''),
    CONSTRAINT payment_intents_correlation_id_not_blank CHECK (btrim(correlation_id) <> ''),
    CONSTRAINT payment_intents_completed_check CHECK ((state IN ('succeeded', 'failed', 'canceled')) = (completed_at IS NOT NULL)),
    CONSTRAINT payment_intents_account_idempotency_key UNIQUE (account_id, idempotency_key),
    CONSTRAINT payment_intents_trigger_ledger_entry_key UNIQUE (trigger_ledger_entry_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS payment_intents_provider_order_unique_idx
    ON payment_intents (provider, merchant_order_reference)
    WHERE merchant_order_reference IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS payment_intents_provider_transaction_unique_idx
    ON payment_intents (provider, provider_transaction_reference)
    WHERE provider_transaction_reference IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS payment_intents_auto_open_unique_idx
    ON payment_intents (account_id, policy_generation)
    WHERE reason = 'auto_top_up' AND state IN ('created', 'processing', 'authorized', 'requires_action', 'unknown');

CREATE TABLE IF NOT EXISTS payment_attempts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    intent_id UUID NOT NULL REFERENCES payment_intents(id) ON DELETE RESTRICT,
    operation TEXT NOT NULL,
    attempt_number INTEGER NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    normalized_result TEXT NOT NULL,
    provider_code TEXT,
    request_sha256 CHAR(64),
    response_sha256 CHAR(64),
    next_reconciliation_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_attempts_operation_check CHECK (operation IN ('setup', 'charge', 'query', 'cancel', 'refund')),
    CONSTRAINT payment_attempts_number_check CHECK (attempt_number > 0),
    CONSTRAINT payment_attempts_result_check CHECK (normalized_result IN ('started', 'authorized', 'requires_action', 'unknown', 'succeeded', 'failed', 'canceled')),
    CONSTRAINT payment_attempts_request_sha256_check CHECK (request_sha256 IS NULL OR request_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT payment_attempts_response_sha256_check CHECK (response_sha256 IS NULL OR response_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT payment_attempts_intent_number_key UNIQUE (intent_id, attempt_number)
);

CREATE TABLE IF NOT EXISTS payment_webhook_inbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider TEXT NOT NULL,
    provider_event_reference TEXT,
    payload_sha256 CHAR(64) NOT NULL,
    payload_ciphertext BYTEA,
    verification_result TEXT NOT NULL,
    intent_id UUID REFERENCES payment_intents(id) ON DELETE RESTRICT,
    normalized_event_type TEXT,
    processing_state TEXT NOT NULL DEFAULT 'received',
    redacted_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
    received_at TIMESTAMPTZ NOT NULL,
    processed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_webhook_provider_not_blank CHECK (btrim(provider) <> '' AND provider = lower(provider)),
    CONSTRAINT payment_webhook_payload_sha256_check CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT payment_webhook_verification_check CHECK (verification_result IN ('pending', 'verified', 'rejected')),
    CONSTRAINT payment_webhook_processing_state_check CHECK (processing_state IN ('received', 'scheduled', 'processed', 'quarantined')),
    CONSTRAINT payment_webhook_processed_check CHECK ((processing_state IN ('processed', 'quarantined')) = (processed_at IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS payment_webhook_provider_event_unique_idx
    ON payment_webhook_inbox (provider, provider_event_reference)
    WHERE provider_event_reference IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS payment_webhook_provider_digest_unique_idx
    ON payment_webhook_inbox (provider, payload_sha256);

CREATE TABLE IF NOT EXISTS payment_reconciliation_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    intent_id UUID NOT NULL REFERENCES payment_intents(id) ON DELETE RESTRICT,
    reason TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    due_at TIMESTAMPTZ NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    leased_at TIMESTAMPTZ,
    lease_owner TEXT,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_reconciliation_reason_check CHECK (reason IN ('charge', 'unknown', 'webhook', 'credit', 'refund')),
    CONSTRAINT payment_reconciliation_status_check CHECK (status IN ('pending', 'leased', 'completed', 'failed')),
    CONSTRAINT payment_reconciliation_attempt_count_check CHECK (attempt_count >= 0),
    CONSTRAINT payment_reconciliation_lease_pair_check CHECK ((leased_at IS NULL) = (lease_owner IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS payment_reconciliation_open_unique_idx
    ON payment_reconciliation_jobs (intent_id, reason)
    WHERE status IN ('pending', 'leased');

CREATE INDEX IF NOT EXISTS balance_ledger_account_created_idx
    ON balance_ledger_entries (account_id, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS payment_consents_account_created_idx
    ON payment_consents (account_id, created_at DESC);

CREATE INDEX IF NOT EXISTS payment_methods_account_created_idx
    ON payment_methods (account_id, created_at DESC);

CREATE INDEX IF NOT EXISTS payment_intents_account_created_idx
    ON payment_intents (account_id, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS payment_attempts_intent_created_idx
    ON payment_attempts (intent_id, created_at, attempt_number);

CREATE INDEX IF NOT EXISTS payment_webhook_processing_idx
    ON payment_webhook_inbox (processing_state, received_at);

CREATE INDEX IF NOT EXISTS payment_reconciliation_due_idx
    ON payment_reconciliation_jobs (status, due_at)
    WHERE status IN ('pending', 'leased');

CREATE OR REPLACE FUNCTION reject_balance_ledger_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'balance_ledger_entries is append-only';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS balance_ledger_entries_append_only ON balance_ledger_entries;
CREATE TRIGGER balance_ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON balance_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_balance_ledger_mutation();

DROP TRIGGER IF EXISTS commercial_accounts_set_updated_at ON commercial_accounts;
CREATE TRIGGER commercial_accounts_set_updated_at
    BEFORE UPDATE ON commercial_accounts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS payment_consents_set_updated_at ON payment_consents;
CREATE TRIGGER payment_consents_set_updated_at
    BEFORE UPDATE ON payment_consents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS payment_methods_set_updated_at ON payment_methods;
CREATE TRIGGER payment_methods_set_updated_at
    BEFORE UPDATE ON payment_methods
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS auto_topup_policies_set_updated_at ON auto_topup_policies;
CREATE TRIGGER auto_topup_policies_set_updated_at
    BEFORE UPDATE ON auto_topup_policies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS payment_intents_set_updated_at ON payment_intents;
CREATE TRIGGER payment_intents_set_updated_at
    BEFORE UPDATE ON payment_intents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS payment_webhook_inbox_set_updated_at ON payment_webhook_inbox;
CREATE TRIGGER payment_webhook_inbox_set_updated_at
    BEFORE UPDATE ON payment_webhook_inbox
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS payment_reconciliation_jobs_set_updated_at ON payment_reconciliation_jobs;
CREATE TRIGGER payment_reconciliation_jobs_set_updated_at
    BEFORE UPDATE ON payment_reconciliation_jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE commercial_accounts IS 'Provider-neutral Brand Cloud commercial account and transactionally maintained balance projection.';
COMMENT ON TABLE balance_ledger_entries IS 'Immutable monetary source of truth; corrections are compensating entries.';
COMMENT ON COLUMN payment_methods.provider_customer_ref_ciphertext IS 'Encrypted opaque provider reference; never PAN or CVV.';
COMMENT ON COLUMN payment_methods.provider_method_ref_ciphertext IS 'Encrypted opaque chargeable method reference; never PAN or CVV.';
COMMENT ON COLUMN payment_webhook_inbox.payload_ciphertext IS 'Optional encrypted provider event body; never stored in plaintext.';
