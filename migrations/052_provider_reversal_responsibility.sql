ALTER TABLE billing_responsibility_periods ADD CONSTRAINT responsibility_period_account_unique UNIQUE (id,account_id);
ALTER TABLE payment_intents ADD CONSTRAINT payment_intent_account_unique UNIQUE (id,account_id);

CREATE TABLE billing_payment_responsibility (
    intent_id UUID PRIMARY KEY,
    account_id UUID NOT NULL,
    period_id UUID NOT NULL,
    evidence_sha256 TEXT NOT NULL CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
    provenance TEXT NOT NULL CHECK (provenance IN ('creation','reviewed_migration')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (intent_id,account_id) REFERENCES payment_intents(id,account_id) ON DELETE RESTRICT,
    FOREIGN KEY (period_id,account_id) REFERENCES billing_responsibility_periods(id,account_id) ON DELETE RESTRICT
);
CREATE TRIGGER billing_payment_responsibility_append_only BEFORE UPDATE OR DELETE ON billing_payment_responsibility
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();

CREATE FUNCTION bind_payment_responsibility_at_creation() RETURNS TRIGGER AS $$
BEGIN
    PERFORM id FROM commercial_accounts WHERE id=NEW.account_id FOR UPDATE;
    IF EXISTS (SELECT 1 FROM billing_ownership_handoffs WHERE account_id=NEW.account_id AND phase NOT IN ('finalized','aborted')) THEN
        RAISE EXCEPTION 'new payment is fenced by ownership handoff'
            USING ERRCODE='55000', CONSTRAINT='billing_handoff_commit_barrier';
    END IF;
    INSERT INTO billing_payment_responsibility(intent_id,account_id,period_id,evidence_sha256,provenance)
        SELECT NEW.id,NEW.account_id,id,source_evidence_sha256,'creation'
        FROM billing_responsibility_periods WHERE account_id=NEW.account_id AND effective_until IS NULL;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER payment_intent_responsibility AFTER INSERT ON payment_intents
    FOR EACH ROW EXECUTE FUNCTION bind_payment_responsibility_at_creation();

-- Deliberately no historical backfill from today's owner or timestamps.
CREATE TABLE billing_provider_reversal_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    provider TEXT NOT NULL CHECK (btrim(provider)<>'' AND provider=lower(provider)),
    event_reference TEXT NOT NULL CHECK (length(event_reference) BETWEEN 1 AND 200),
    original_intent_id UUID NOT NULL,
    amount_minor BIGINT NOT NULL CHECK (amount_minor>0),
    currency TEXT NOT NULL CHECK (currency='TWD'),
    reason TEXT NOT NULL CHECK (reason IN ('refund_debit','chargeback_debit')),
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    provider_payload_sha256 TEXT NOT NULL CHECK (provider_payload_sha256 ~ '^[0-9a-f]{64}$'),
    request_id TEXT NOT NULL CHECK (btrim(request_id)<>''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider,event_reference),
    UNIQUE (id,account_id)
);
CREATE TABLE billing_provider_reversal_allocations (
    event_id UUID PRIMARY KEY,
    account_id UUID NOT NULL,
    period_id UUID NOT NULL,
    disposition TEXT NOT NULL CHECK (disposition IN ('current_balance','predecessor_adjustment')),
    ledger_entry_id UUID UNIQUE REFERENCES balance_ledger_entries(id) ON DELETE RESTRICT,
    evidence_sha256 TEXT NOT NULL CHECK (evidence_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((disposition='current_balance')=(ledger_entry_id IS NOT NULL)),
    FOREIGN KEY (event_id,account_id) REFERENCES billing_provider_reversal_events(id,account_id) ON DELETE RESTRICT,
    FOREIGN KEY (period_id,account_id) REFERENCES billing_responsibility_periods(id,account_id) ON DELETE RESTRICT
);
CREATE TABLE billing_provider_reversal_reviews (
    event_id UUID PRIMARY KEY REFERENCES billing_provider_reversal_events(id) ON DELETE RESTRICT,
    reason_code TEXT NOT NULL CHECK (btrim(reason_code)<>''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER provider_reversal_events_append_only BEFORE UPDATE OR DELETE ON billing_provider_reversal_events
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER provider_reversal_allocations_append_only BEFORE UPDATE OR DELETE ON billing_provider_reversal_allocations
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER provider_reversal_reviews_append_only BEFORE UPDATE OR DELETE ON billing_provider_reversal_reviews
    FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE INDEX provider_reversal_original_payment ON billing_provider_reversal_events(account_id,original_intent_id);
COMMENT ON TABLE billing_provider_reversal_allocations IS
    'Predecessor-period adjustment ledger is separate from spendable balance. Current-period reversals link their balance ledger entry; predecessor adjustments never do.';
