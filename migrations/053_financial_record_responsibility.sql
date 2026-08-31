-- Bind new records at their trusted creation boundary. No historical attribution
-- is inferred from the current owner or a late record's insertion timestamp.
ALTER TABLE payment_methods ADD CONSTRAINT payment_method_account_unique UNIQUE(id,account_id);
ALTER TABLE balance_ledger_entries ADD CONSTRAINT ledger_entry_account_unique UNIQUE(id,account_id);

CREATE TABLE billing_payment_method_responsibility (
    method_id UUID PRIMARY KEY,
    account_id UUID NOT NULL,
    period_id UUID NOT NULL,
    evidence_sha256 TEXT NOT NULL CHECK(evidence_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY(method_id,account_id) REFERENCES payment_methods(id,account_id) ON DELETE RESTRICT,
    FOREIGN KEY(period_id,account_id) REFERENCES billing_responsibility_periods(id,account_id) ON DELETE RESTRICT
);
CREATE TABLE billing_ledger_responsibility (
    entry_id UUID PRIMARY KEY,
    account_id UUID NOT NULL,
    period_id UUID NOT NULL,
    evidence_sha256 TEXT NOT NULL CHECK(evidence_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY(entry_id,account_id) REFERENCES balance_ledger_entries(id,account_id) ON DELETE RESTRICT,
    FOREIGN KEY(period_id,account_id) REFERENCES billing_responsibility_periods(id,account_id) ON DELETE RESTRICT
);
CREATE TRIGGER method_responsibility_append_only BEFORE UPDATE OR DELETE ON billing_payment_method_responsibility
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER ledger_responsibility_append_only BEFORE UPDATE OR DELETE ON billing_ledger_responsibility
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();

CREATE FUNCTION bind_method_responsibility_at_creation() RETURNS TRIGGER AS $$
BEGIN
    PERFORM id FROM commercial_accounts WHERE id=NEW.account_id FOR UPDATE;
    INSERT INTO billing_payment_method_responsibility(method_id,account_id,period_id,evidence_sha256)
        SELECT NEW.id,NEW.account_id,id,source_evidence_sha256 FROM billing_responsibility_periods
        WHERE account_id=NEW.account_id AND effective_until IS NULL;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER payment_method_responsibility AFTER INSERT ON payment_methods
    FOR EACH ROW EXECUTE FUNCTION bind_method_responsibility_at_creation();

CREATE FUNCTION bind_ledger_responsibility_at_creation() RETURNS TRIGGER AS $$
BEGIN
    PERFORM id FROM commercial_accounts WHERE id=NEW.account_id FOR UPDATE;
    IF NEW.external_type='payment_intent' THEN
        INSERT INTO billing_ledger_responsibility(entry_id,account_id,period_id,evidence_sha256)
            SELECT NEW.id,NEW.account_id,period_id,evidence_sha256 FROM billing_payment_responsibility
            WHERE intent_id::text=NEW.external_id AND account_id=NEW.account_id;
    ELSIF NEW.external_type='invoice' THEN
        INSERT INTO billing_ledger_responsibility(entry_id,account_id,period_id,evidence_sha256)
            SELECT NEW.id,NEW.account_id,p.id,p.source_evidence_sha256
            FROM billing_invoices i JOIN billing_responsibility_periods p ON p.account_id=i.account_id
            AND i.recipient_snapshot->>'ownership_version'=p.ownership_version::text
            AND i.period_start>=p.effective_from AND i.period_end<=COALESCE(p.effective_until,'infinity'::timestamptz)
            WHERE i.id::text=NEW.external_id AND i.account_id=NEW.account_id;
    ELSIF NEW.reason IN ('manual_adjustment_credit','manual_adjustment_debit') THEN
        INSERT INTO billing_ledger_responsibility(entry_id,account_id,period_id,evidence_sha256)
            SELECT NEW.id,NEW.account_id,id,source_evidence_sha256 FROM billing_responsibility_periods
            WHERE account_id=NEW.account_id AND effective_until IS NULL;
    END IF;
    -- Unproven usage adjustments/legacy references stay unbound and tenant-hidden.
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER balance_ledger_responsibility AFTER INSERT ON balance_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION bind_ledger_responsibility_at_creation();

-- Retain restricted configuration evidence before replacing a policy's payment
-- method. The current projection may reset its creation metadata for a new owner;
-- no tenant endpoint reads this archive, and policy generations remain monotonic.
CREATE TABLE billing_retired_policy_evidence (
    policy_id UUID NOT NULL REFERENCES auto_topup_policies(id) ON DELETE RESTRICT,
    policy_version BIGINT NOT NULL,
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    snapshot JSONB NOT NULL,
    archived_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(policy_id,policy_version)
);
CREATE TRIGGER retired_policy_evidence_append_only BEFORE UPDATE OR DELETE ON billing_retired_policy_evidence
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE FUNCTION archive_replaced_policy() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.payment_method_id IS DISTINCT FROM NEW.payment_method_id THEN
        INSERT INTO billing_retired_policy_evidence(policy_id,policy_version,account_id,snapshot)
            VALUES(OLD.id,OLD.version,OLD.account_id,to_jsonb(OLD));
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER auto_topup_policy_replacement BEFORE UPDATE ON auto_topup_policies
    FOR EACH ROW EXECUTE FUNCTION archive_replaced_policy();

-- Merely locking the account does not invalidate an older REPEATABLE READ
-- snapshot waiting behind a handoff. Touch the guard row on every authority/fence
-- transition so such readers fail serialization, never observe the old owner.
-- This changes neither monetary version nor balance/settlement evidence.
CREATE FUNCTION touch_billing_authority_guard() RETURNS TRIGGER AS $$
BEGIN
    UPDATE commercial_accounts SET updated_at=now()
        WHERE id=CASE WHEN TG_OP='DELETE' THEN OLD.account_id ELSE NEW.account_id END;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER responsibility_authority_guard AFTER INSERT OR UPDATE OR DELETE ON billing_responsibility_periods
    FOR EACH ROW EXECUTE FUNCTION touch_billing_authority_guard();
CREATE TRIGGER handoff_authority_guard AFTER INSERT OR UPDATE OF phase OR DELETE ON billing_ownership_handoffs
    FOR EACH ROW EXECUTE FUNCTION touch_billing_authority_guard();
