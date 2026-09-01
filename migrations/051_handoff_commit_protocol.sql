CREATE TABLE billing_handoff_commit_authorizations (
    operation_id UUID PRIMARY KEY REFERENCES billing_ownership_handoffs(id) ON DELETE RESTRICT,
    authorization_id UUID NOT NULL UNIQUE,
    snapshot_version BIGINT NOT NULL,
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    state_sha256 TEXT NOT NULL CHECK (state_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (operation_id,snapshot_version)
        REFERENCES billing_handoff_balance_snapshots(operation_id,version) ON DELETE RESTRICT
);
CREATE TABLE billing_handoff_committed_decisions (
    operation_id UUID PRIMARY KEY REFERENCES billing_handoff_commit_authorizations(operation_id) ON DELETE RESTRICT,
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    am_commit_sha256 TEXT NOT NULL CHECK (am_commit_sha256 ~ '^[0-9a-f]{64}$'),
    committed_ownership_version BIGINT NOT NULL CHECK (committed_ownership_version > 1),
    committed_at TIMESTAMPTZ NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE billing_handoff_finalizations (
    operation_id UUID PRIMARY KEY REFERENCES billing_handoff_committed_decisions(operation_id) ON DELETE RESTRICT,
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    am_commit_sha256 TEXT NOT NULL CHECK (am_commit_sha256 ~ '^[0-9a-f]{64}$'),
    committed_ownership_version BIGINT NOT NULL CHECK (committed_ownership_version > 1),
    committed_at TIMESTAMPTZ NOT NULL,
    finalized_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE billing_handoff_cancellations (
    operation_id UUID PRIMARY KEY REFERENCES billing_ownership_handoffs(id) ON DELETE RESTRICT,
    cancellation_id UUID NOT NULL UNIQUE,
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    am_cancellation_sha256 TEXT NOT NULL CHECK (am_cancellation_sha256 ~ '^[0-9a-f]{64}$'),
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE billing_handoff_abort_acknowledgments (
    operation_id UUID PRIMARY KEY REFERENCES billing_handoff_cancellations(operation_id) ON DELETE RESTRICT,
    hold_release_sha256 TEXT NOT NULL CHECK (hold_release_sha256 ~ '^[0-9a-f]{64}$'),
    completed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER handoff_authorizations_append_only BEFORE UPDATE OR DELETE ON billing_handoff_commit_authorizations
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER handoff_finalizations_append_only BEFORE UPDATE OR DELETE ON billing_handoff_finalizations
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER handoff_committed_decisions_append_only BEFORE UPDATE OR DELETE ON billing_handoff_committed_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER handoff_cancellations_append_only BEFORE UPDATE OR DELETE ON billing_handoff_cancellations
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER handoff_abort_acks_append_only BEFORE UPDATE OR DELETE ON billing_handoff_abort_acknowledgments
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();

ALTER TABLE billing_responsibility_periods ADD COLUMN opening_balance_minor BIGINT CHECK (opening_balance_minor >= 0);
ALTER TABLE billing_responsibility_periods ADD COLUMN opening_operation_id UUID UNIQUE
    REFERENCES billing_handoff_finalizations(operation_id) ON DELETE RESTRICT;
ALTER TABLE billing_responsibility_periods ADD CONSTRAINT responsibility_opening_evidence
    CHECK ((opening_operation_id IS NULL) = (opening_balance_minor IS NULL));

CREATE TABLE billing_retired_profiles (
    operation_id UUID PRIMARY KEY REFERENCES billing_handoff_finalizations(operation_id) ON DELETE RESTRICT,
    source_period_id UUID NOT NULL REFERENCES billing_responsibility_periods(id) ON DELETE RESTRICT,
    profile_snapshot JSONB NOT NULL CHECK (jsonb_typeof(profile_snapshot) = 'object'),
    retired_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER billing_retired_profiles_append_only BEFORE UPDATE OR DELETE ON billing_retired_profiles
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
ALTER TABLE billing_profiles ADD COLUMN ownership_version BIGINT CHECK (ownership_version > 0);
ALTER TABLE billing_profiles ADD COLUMN requires_configuration BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE billing_profiles DROP CONSTRAINT billing_profiles_legal_name_not_blank;
ALTER TABLE billing_profiles ADD CONSTRAINT billing_profiles_configuration_check CHECK (
    (NOT requires_configuration AND btrim(legal_name) <> '') OR
    (requires_configuration AND ownership_version IS NOT NULL AND legal_name = ''
        AND tax_identifier IS NULL AND billing_address IS NULL AND contact_email IS NULL AND delivery_preference = 'portal')
);

-- Account locks serialize authorization with last-moment ledger/usage/invoice
-- writers. Provider inbox/reconciliation evidence remains writable. A producer
-- must retain rejected writes in its durable outbox; no new money can silently
-- change the amount after Account Manager obtains a commit authorization.
CREATE FUNCTION enforce_billing_handoff_commit_barrier() RETURNS TRIGGER AS $$
DECLARE
    target_account UUID;
    target_org UUID;
BEGIN
    IF TG_TABLE_NAME = 'commercial_accounts' THEN
        IF NEW.available_balance_minor = OLD.available_balance_minor THEN RETURN NEW; END IF;
        target_account := NEW.id;
    ELSIF TG_TABLE_NAME IN ('balance_ledger_entries','billing_invoices') THEN
        IF TG_OP = 'DELETE' THEN target_account := OLD.account_id; ELSE target_account := NEW.account_id; END IF;
    ELSIF TG_TABLE_NAME = 'invoice_settlement_links' THEN
        SELECT account_id INTO target_account FROM billing_invoices WHERE id = CASE WHEN TG_OP='DELETE' THEN OLD.invoice_id ELSE NEW.invoice_id END;
    ELSE
        IF TG_OP = 'DELETE' THEN target_org := OLD.organization_id; ELSE target_org := NEW.organization_id; END IF;
        SELECT id INTO target_account FROM commercial_accounts WHERE organization_id=target_org AND currency='TWD';
    END IF;
    IF target_account IS NOT NULL THEN
        PERFORM id FROM commercial_accounts WHERE id=target_account FOR UPDATE;
        IF EXISTS (SELECT 1 FROM billing_ownership_handoffs WHERE account_id=target_account
            AND (phase IN ('commit_authorized','finalizing') OR
                (phase='abort_pending' AND EXISTS (SELECT 1 FROM billing_handoff_commit_authorizations a WHERE a.operation_id=billing_ownership_handoffs.id)))) THEN
            RAISE EXCEPTION 'billing ownership commit barrier is active' USING ERRCODE='55000', CONSTRAINT='billing_handoff_commit_barrier';
        END IF;
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER handoff_ledger_commit_barrier BEFORE INSERT ON balance_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION enforce_billing_handoff_commit_barrier();
CREATE TRIGGER handoff_balance_commit_barrier BEFORE UPDATE OF available_balance_minor ON commercial_accounts
    FOR EACH ROW EXECUTE FUNCTION enforce_billing_handoff_commit_barrier();
CREATE TRIGGER handoff_usage_commit_barrier BEFORE INSERT OR UPDATE OR DELETE ON billing_usage_facts
    FOR EACH ROW EXECUTE FUNCTION enforce_billing_handoff_commit_barrier();
CREATE TRIGGER handoff_invoice_commit_barrier BEFORE INSERT OR UPDATE OR DELETE ON billing_invoices
    FOR EACH ROW EXECUTE FUNCTION enforce_billing_handoff_commit_barrier();
CREATE TRIGGER handoff_period_commit_barrier BEFORE INSERT OR UPDATE OR DELETE ON billing_periods
    FOR EACH ROW EXECUTE FUNCTION enforce_billing_handoff_commit_barrier();
CREATE TRIGGER handoff_settlement_commit_barrier BEFORE INSERT OR UPDATE OR DELETE ON invoice_settlement_links
    FOR EACH ROW EXECUTE FUNCTION enforce_billing_handoff_commit_barrier();
