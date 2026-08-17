-- Durable, synthetic payment-provider state. This table is used only by the
-- separately deployed non-production payment simulator process.

CREATE TABLE IF NOT EXISTS payment_simulator_setup_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id TEXT NOT NULL,
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    setup_session_id UUID NOT NULL UNIQUE REFERENCES payment_method_setup_sessions(id) ON DELETE RESTRICT,
    idempotency_key TEXT NOT NULL,
    token_sha256 CHAR(64) NOT NULL UNIQUE,
    scenario TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'requires_action',
    provider_customer_reference TEXT NOT NULL,
    provider_method_reference TEXT NOT NULL,
    callback_status TEXT NOT NULL DEFAULT 'pending',
    callback_attempts INTEGER NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_simulator_setup_key UNIQUE (run_id, account_id, idempotency_key),
    CONSTRAINT payment_simulator_setup_run_id_check CHECK (run_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    CONSTRAINT payment_simulator_token_sha_check CHECK (token_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT payment_simulator_scenario_check CHECK (scenario IN ('success', 'declined', 'temporary_error', 'requires_action', 'unknown')),
    CONSTRAINT payment_simulator_setup_state_check CHECK (state IN ('requires_action', 'succeeded', 'failed')),
    CONSTRAINT payment_simulator_callback_status_check CHECK (callback_status IN ('pending', 'succeeded', 'failed')),
    CONSTRAINT payment_simulator_callback_attempts_check CHECK (callback_attempts >= 0)
);

CREATE TABLE IF NOT EXISTS payment_simulator_operations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id TEXT NOT NULL,
    operation TEXT NOT NULL,
    intent_id UUID NOT NULL REFERENCES payment_intents(id) ON DELETE RESTRICT,
    idempotency_key TEXT NOT NULL,
    merchant_order_reference TEXT NOT NULL,
    provider_transaction_reference TEXT NOT NULL,
    amount_minor BIGINT NOT NULL,
    currency CHAR(3) NOT NULL,
    scenario TEXT NOT NULL,
    state TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_simulator_operation_check CHECK (operation IN ('charge', 'refund')),
    CONSTRAINT payment_simulator_operation_key UNIQUE (run_id, operation, idempotency_key),
    CONSTRAINT payment_simulator_order_key UNIQUE (run_id, operation, merchant_order_reference),
    CONSTRAINT payment_simulator_operation_run_id_check CHECK (run_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    CONSTRAINT payment_simulator_operation_amount_check CHECK (amount_minor > 0 AND currency = 'TWD'),
    CONSTRAINT payment_simulator_operation_scenario_check CHECK (scenario IN ('success', 'declined', 'temporary_error', 'requires_action', 'unknown')),
    CONSTRAINT payment_simulator_operation_state_check CHECK (state IN ('requires_action', 'unknown', 'succeeded', 'failed', 'canceled'))
);

DROP TRIGGER IF EXISTS payment_simulator_setup_sessions_set_updated_at ON payment_simulator_setup_sessions;
CREATE TRIGGER payment_simulator_setup_sessions_set_updated_at
    BEFORE UPDATE ON payment_simulator_setup_sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS payment_simulator_operations_set_updated_at ON payment_simulator_operations;
CREATE TRIGGER payment_simulator_operations_set_updated_at
    BEFORE UPDATE ON payment_simulator_operations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON COLUMN payment_simulator_setup_sessions.token_sha256 IS 'SHA-256 only; raw hosted-page tokens are never persisted.';
