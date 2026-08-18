CREATE TABLE IF NOT EXISTS billing_audit_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    event_type TEXT NOT NULL,
    actor_type TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    subject_type TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_audit_event_type_check CHECK (btrim(event_type) <> ''),
    CONSTRAINT billing_audit_actor_check CHECK (btrim(actor_type) <> '' AND btrim(actor_id) <> ''),
    CONSTRAINT billing_audit_subject_check CHECK (btrim(subject_type) <> '' AND btrim(subject_id) <> ''),
    CONSTRAINT billing_audit_request_check CHECK (btrim(request_id) <> '')
);

CREATE INDEX IF NOT EXISTS billing_audit_org_created_idx
    ON billing_audit_events (organization_id, created_at DESC);
