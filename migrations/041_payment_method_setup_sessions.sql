CREATE TABLE IF NOT EXISTS payment_method_setup_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    provider TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_sha256 CHAR(64) NOT NULL,
    correlation_id TEXT NOT NULL,
    payment_method_id UUID NOT NULL REFERENCES payment_methods(id) ON DELETE RESTRICT,
    state TEXT NOT NULL DEFAULT 'created',
    provider_code TEXT,
    hosted_url_sha256 CHAR(64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_method_setup_provider_check CHECK (btrim(provider) <> '' AND provider = lower(provider)),
    CONSTRAINT payment_method_setup_idempotency_check CHECK (btrim(idempotency_key) <> ''),
    CONSTRAINT payment_method_setup_request_sha_check CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT payment_method_setup_correlation_check CHECK (btrim(correlation_id) <> ''),
    CONSTRAINT payment_method_setup_state_check CHECK (state IN ('created', 'requires_action', 'succeeded', 'failed', 'unknown')),
    CONSTRAINT payment_method_setup_provider_code_check CHECK (provider_code IS NULL OR btrim(provider_code) <> ''),
    CONSTRAINT payment_method_setup_hosted_url_sha_check CHECK (hosted_url_sha256 IS NULL OR hosted_url_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT payment_method_setup_account_provider_key_unique UNIQUE (account_id, provider, idempotency_key)
);

CREATE INDEX IF NOT EXISTS payment_method_setup_method_idx
    ON payment_method_setup_sessions (payment_method_id, created_at DESC);

DROP TRIGGER IF EXISTS payment_method_setup_sessions_set_updated_at ON payment_method_setup_sessions;
CREATE TRIGGER payment_method_setup_sessions_set_updated_at
    BEFORE UPDATE ON payment_method_setup_sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON COLUMN payment_method_setup_sessions.hosted_url_sha256 IS 'SHA-256 only; the short-lived hosted URL and session token are never persisted.';
