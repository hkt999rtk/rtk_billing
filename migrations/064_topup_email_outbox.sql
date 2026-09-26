-- A payment and its email work enter durable storage in the same transaction.
-- The email worker may retry delivery; a unique intent ID prevents duplicate work.
CREATE TABLE billing_topup_email_outbox (
    intent_id UUID PRIMARY KEY REFERENCES payment_intents(id) ON DELETE RESTRICT,
    organization_id UUID NOT NULL,
    recipient_email TEXT NOT NULL CHECK (position('@' in recipient_email) > 1),
    recipient_name TEXT NOT NULL,
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency CHAR(3) NOT NULL CHECK (currency = 'TWD'),
    provider TEXT NOT NULL CHECK (provider = 'paypal'),
    payment_created_at TIMESTAMPTZ NOT NULL,
    payment_completed_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sending', 'sent')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until TIMESTAMPTZ,
    sent_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX billing_topup_email_ready_idx ON billing_topup_email_outbox (available_at, created_at)
    WHERE status IN ('pending', 'sending');
