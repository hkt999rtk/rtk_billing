ALTER TABLE payment_intents ALTER COLUMN payment_method_id DROP NOT NULL;
ALTER TABLE payment_intents DROP CONSTRAINT IF EXISTS payment_intents_twd_amount_check;
ALTER TABLE auto_topup_policies DROP CONSTRAINT IF EXISTS auto_topup_policy_twd_amounts_check;

CREATE TABLE IF NOT EXISTS payment_simulator_newebpay_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id TEXT NOT NULL,
    merchant_id TEXT NOT NULL,
    merchant_order_no TEXT NOT NULL,
    trade_no TEXT NOT NULL,
    amount_minor BIGINT NOT NULL,
    scenario TEXT NOT NULL DEFAULT 'success',
    trade_status TEXT NOT NULL DEFAULT '0',
    captured_amount_minor BIGINT NOT NULL DEFAULT 0,
    refunded_amount_minor BIGINT NOT NULL DEFAULT 0,
    public_token_sha256 CHAR(64) NOT NULL UNIQUE,
    notify_url TEXT,
    return_url TEXT,
    callback_attempts INTEGER NOT NULL DEFAULT 0,
    callback_status TEXT NOT NULL DEFAULT 'pending',
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_simulator_newebpay_order UNIQUE (run_id, merchant_id, merchant_order_no),
    CONSTRAINT payment_simulator_newebpay_amount CHECK (amount_minor > 0),
    CONSTRAINT payment_simulator_newebpay_captured_amount CHECK (captured_amount_minor >= 0 AND captured_amount_minor <= amount_minor),
    CONSTRAINT payment_simulator_newebpay_refunded_amount CHECK (refunded_amount_minor >= 0 AND refunded_amount_minor <= captured_amount_minor),
    CONSTRAINT payment_simulator_newebpay_scenario CHECK (scenario IN ('success', 'declined', 'temporary_error', 'requires_action', 'unknown')),
    CONSTRAINT payment_simulator_newebpay_trade_status CHECK (trade_status IN ('0', '1', '2', '3', '6')),
    CONSTRAINT payment_simulator_newebpay_callback_status CHECK (callback_status IN ('pending', 'succeeded', 'failed'))
);

DROP TRIGGER IF EXISTS payment_simulator_newebpay_set_updated_at ON payment_simulator_newebpay_transactions;
CREATE TRIGGER payment_simulator_newebpay_set_updated_at
    BEFORE UPDATE ON payment_simulator_newebpay_transactions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE payment_simulator_newebpay_transactions IS 'Non-production NewebPay wire-protocol simulator state only.';
COMMENT ON COLUMN payment_simulator_newebpay_transactions.public_token_sha256 IS 'SHA-256 only; the raw hosted-payment token is never persisted.';
