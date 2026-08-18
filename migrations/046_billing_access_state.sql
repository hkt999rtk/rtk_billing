-- Billing owns whether an otherwise valid Account Manager organization may use
-- monetary operations. Membership and role assignment remain in Account
-- Manager; this state is the final fail-closed commercial access gate.

CREATE TABLE IF NOT EXISTS billing_access_states (
    organization_id UUID PRIMARY KEY,
    state TEXT NOT NULL DEFAULT 'active',
    reason_code TEXT,
    version BIGINT NOT NULL DEFAULT 1,
    updated_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_access_state_check CHECK (state IN ('active', 'read_only', 'suspended', 'closed')),
    CONSTRAINT billing_access_reason_check CHECK (reason_code IS NULL OR btrim(reason_code) <> ''),
    CONSTRAINT billing_access_version_check CHECK (version > 0),
    CONSTRAINT billing_access_actor_check CHECK (btrim(updated_by) <> '')
);
