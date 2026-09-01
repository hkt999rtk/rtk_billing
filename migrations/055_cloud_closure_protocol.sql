CREATE TABLE billing_cloud_closures (
    id UUID PRIMARY KEY,
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    source_period_id UUID NOT NULL,
    owner_user_id UUID NOT NULL,
    ownership_version BIGINT NOT NULL,
    cutoff TIMESTAMPTZ NOT NULL,
    am_request_sha256 TEXT NOT NULL CHECK (am_request_sha256 ~ '^[0-9a-f]{64}$'),
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    phase TEXT NOT NULL DEFAULT 'preparing' CHECK (phase IN ('preparing','closed','canceling','canceled')),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version>0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY(source_period_id,account_id,owner_user_id,ownership_version)
        REFERENCES billing_responsibility_periods(id,account_id,owner_user_id,ownership_version) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX billing_one_cloud_closure ON billing_cloud_closures(account_id) WHERE phase<>'canceled';
ALTER TABLE payment_method_setup_sessions ADD COLUMN invalidated_by_closure UUID REFERENCES billing_cloud_closures(id) ON DELETE RESTRICT;

-- Immutable provider work inventory. Method/setup identifiers only, never card
-- credentials. Only a verified provider response can acknowledge each task.
CREATE TABLE billing_cloud_closure_revocations (
    operation_id UUID NOT NULL REFERENCES billing_cloud_closures(id) ON DELETE RESTRICT,
    subject_type TEXT NOT NULL CHECK(subject_type IN ('method','setup')),
    subject_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id,subject_type,subject_id)
);
CREATE TABLE billing_cloud_closure_revocation_acks (
    operation_id UUID NOT NULL,
    subject_type TEXT NOT NULL,
    subject_id UUID NOT NULL,
    receipt_sha256 TEXT NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id,subject_type,subject_id),
    FOREIGN KEY(operation_id,subject_type,subject_id) REFERENCES billing_cloud_closure_revocations(operation_id,subject_type,subject_id) ON DELETE RESTRICT
);
CREATE TABLE billing_cloud_closure_settlements (
    id UUID PRIMARY KEY,
    operation_id UUID NOT NULL REFERENCES billing_cloud_closures(id) ON DELETE RESTRICT,
    request_sha256 TEXT NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
    state_sha256 TEXT NOT NULL CHECK(state_sha256 ~ '^[0-9a-f]{64}$'),
    financial_evidence JSONB NOT NULL CHECK(jsonb_typeof(financial_evidence)='object'),
    usage_checkpoint_sha256 TEXT CHECK(usage_checkpoint_sha256 ~ '^[0-9a-f]{64}$'),
    invoice_checkpoint_sha256 TEXT CHECK(invoice_checkpoint_sha256 ~ '^[0-9a-f]{64}$'),
    provider_checkpoint_sha256 TEXT CHECK(provider_checkpoint_sha256 ~ '^[0-9a-f]{64}$'),
    covered_through TIMESTAMPTZ NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK(covered_through<=observed_at AND expires_at>observed_at AND expires_at<=observed_at+interval '5 minutes'),
    UNIQUE(id,operation_id)
);
CREATE INDEX billing_closure_latest_settlement ON billing_cloud_closure_settlements(operation_id,observed_at DESC,created_at DESC,id);
CREATE TABLE billing_cloud_closure_completions (
    operation_id UUID PRIMARY KEY REFERENCES billing_cloud_closures(id) ON DELETE RESTRICT,
    settlement_id UUID NOT NULL,
    am_readiness_sha256 TEXT NOT NULL CHECK(am_readiness_sha256 ~ '^[0-9a-f]{64}$'),
    request_sha256 TEXT NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
    receipt_sha256 TEXT NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
    closed_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY(settlement_id,operation_id) REFERENCES billing_cloud_closure_settlements(id,operation_id) ON DELETE RESTRICT
);
CREATE TABLE billing_cloud_closure_cancellations (
    operation_id UUID PRIMARY KEY REFERENCES billing_cloud_closures(id) ON DELETE RESTRICT,
    cancellation_id UUID NOT NULL UNIQUE,
    am_cancellation_sha256 TEXT NOT NULL CHECK(am_cancellation_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE billing_cloud_closure_release_acks (
    operation_id UUID PRIMARY KEY REFERENCES billing_cloud_closure_cancellations(operation_id) ON DELETE RESTRICT,
    receipt_sha256 TEXT NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TRIGGER cloud_closure_tasks_immutable BEFORE UPDATE OR DELETE ON billing_cloud_closure_revocations FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER cloud_closure_acks_immutable BEFORE UPDATE OR DELETE ON billing_cloud_closure_revocation_acks FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER cloud_closure_settlements_immutable BEFORE UPDATE OR DELETE ON billing_cloud_closure_settlements FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER cloud_closure_completions_immutable BEFORE UPDATE OR DELETE ON billing_cloud_closure_completions FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER cloud_closure_cancellations_immutable BEFORE UPDATE OR DELETE ON billing_cloud_closure_cancellations FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();
CREATE TRIGGER cloud_closure_releases_immutable BEFORE UPDATE OR DELETE ON billing_cloud_closure_release_acks FOR EACH ROW EXECUTE FUNCTION reject_handoff_evidence_mutation();

CREATE FUNCTION guard_billing_cloud_closure() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN RAISE EXCEPTION 'closure history cannot be deleted' USING ERRCODE='23514'; END IF;
    PERFORM id FROM commercial_accounts WHERE id=NEW.account_id FOR UPDATE;
    IF TG_OP='INSERT' THEN
        IF NEW.phase<>'preparing' OR NEW.version<>1 OR EXISTS(SELECT 1 FROM billing_ownership_handoffs WHERE account_id=NEW.account_id AND phase NOT IN ('finalized','aborted'))
            OR NOT EXISTS(SELECT 1 FROM billing_responsibility_periods WHERE id=NEW.source_period_id AND effective_until IS NULL AND effective_from<=NEW.cutoff)
            OR NOT EXISTS(SELECT 1 FROM commercial_accounts WHERE id=NEW.account_id AND state='active') THEN
            RAISE EXCEPTION 'invalid closure preparation' USING ERRCODE='23514';
        END IF;
    ELSE
        IF ROW(NEW.id,NEW.account_id,NEW.source_period_id,NEW.owner_user_id,NEW.ownership_version,NEW.cutoff,NEW.am_request_sha256,NEW.request_sha256,NEW.created_at)
            IS DISTINCT FROM ROW(OLD.id,OLD.account_id,OLD.source_period_id,OLD.owner_user_id,OLD.ownership_version,OLD.cutoff,OLD.am_request_sha256,OLD.request_sha256,OLD.created_at)
            OR NEW.version<>OLD.version+1 OR NOT ((OLD.phase='preparing' AND NEW.phase IN ('closed','canceling')) OR (OLD.phase='canceling' AND NEW.phase='canceled')) THEN
            RAISE EXCEPTION 'invalid closure transition' USING ERRCODE='23514';
        END IF;
        IF NEW.phase='closed' AND NOT EXISTS(SELECT 1 FROM billing_cloud_closure_completions WHERE operation_id=NEW.id) THEN
            RAISE EXCEPTION 'closure completion evidence required' USING ERRCODE='23514';
        END IF;
        IF NEW.phase IN ('canceling','canceled') AND (EXISTS(SELECT 1 FROM billing_cloud_closure_completions WHERE operation_id=NEW.id)
            OR NOT EXISTS(SELECT 1 FROM billing_cloud_closure_cancellations WHERE operation_id=NEW.id)) THEN
            RAISE EXCEPTION 'closed cloud cannot be canceled' USING ERRCODE='23514';
        END IF;
        IF NEW.phase='canceled' AND (NOT EXISTS(SELECT 1 FROM billing_cloud_closure_release_acks WHERE operation_id=NEW.id)
            OR EXISTS(SELECT 1 FROM billing_cloud_closure_revocations t WHERE t.operation_id=NEW.id AND NOT EXISTS(
                SELECT 1 FROM billing_cloud_closure_revocation_acks a WHERE (a.operation_id,a.subject_type,a.subject_id)=(t.operation_id,t.subject_type,t.subject_id)))) THEN
            RAISE EXCEPTION 'closure revocations and release not acknowledged' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER cloud_closure_transition BEFORE INSERT OR UPDATE OR DELETE ON billing_cloud_closures FOR EACH ROW EXECUTE FUNCTION guard_billing_cloud_closure();
CREATE TRIGGER cloud_closure_authority_guard AFTER INSERT OR UPDATE OF phase ON billing_cloud_closures FOR EACH ROW EXECUTE FUNCTION touch_billing_authority_guard();

CREATE FUNCTION prevent_handoff_during_closure() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM id FROM commercial_accounts WHERE id=NEW.account_id FOR UPDATE;
    IF EXISTS(SELECT 1 FROM billing_cloud_closures WHERE account_id=NEW.account_id AND phase<>'canceled') THEN
        RAISE EXCEPTION 'cloud closure is active' USING ERRCODE='55000',CONSTRAINT='billing_cloud_closure_barrier';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER handoff_closure_exclusion BEFORE INSERT ON billing_ownership_handoffs FOR EACH ROW EXECUTE FUNCTION prevent_handoff_during_closure();

-- New payer authority is forbidden throughout preparation/cancellation/closure.
-- Revocation updates and provider audit/query evidence remain writable.
CREATE FUNCTION enforce_cloud_closure_payer_barrier() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE target_account UUID; fenced BOOLEAN;
BEGIN
    IF TG_TABLE_NAME='payment_attempts' THEN
        SELECT account_id INTO target_account FROM payment_intents WHERE id=NEW.intent_id;
        IF NEW.operation<>'charge' THEN RETURN NEW; END IF;
    ELSE target_account:=NEW.account_id; END IF;
    PERFORM id FROM commercial_accounts WHERE id=target_account FOR UPDATE;
    IF TG_OP='UPDATE' THEN
        IF TG_TABLE_NAME='payment_methods' THEN
            IF NEW.status<>'revoked' AND EXISTS(SELECT 1 FROM billing_cloud_closure_revocations WHERE subject_type='method' AND subject_id=NEW.id) THEN
                RAISE EXCEPTION 'closure-revoked method cannot be restored' USING ERRCODE='55000',CONSTRAINT='billing_cloud_closure_barrier';
            END IF;
        ELSIF TG_TABLE_NAME='payment_consents' THEN
            IF OLD.revocation_reason='cloud_closure' AND (NEW.revoked_at IS NULL OR NEW.revocation_reason IS DISTINCT FROM OLD.revocation_reason) THEN
                RAISE EXCEPTION 'closure-revoked consent cannot be restored' USING ERRCODE='55000',CONSTRAINT='billing_cloud_closure_barrier';
            END IF;
        ELSIF TG_TABLE_NAME='payment_method_setup_sessions' THEN
            IF OLD.invalidated_by_closure IS NOT NULL AND (NEW.invalidated_by_closure IS DISTINCT FROM OLD.invalidated_by_closure OR NEW.state<>'failed') THEN
                RAISE EXCEPTION 'closure-invalidated setup cannot be restored' USING ERRCODE='55000',CONSTRAINT='billing_cloud_closure_barrier';
            END IF;
        END IF;
    END IF;
    SELECT EXISTS(SELECT 1 FROM billing_cloud_closures WHERE account_id=target_account AND phase<>'canceled') INTO fenced;
    IF NOT fenced THEN RETURN NEW; END IF;
    IF TG_OP='UPDATE' THEN
        IF TG_TABLE_NAME='payment_methods' THEN
            IF NEW.status='revoked' AND (to_jsonb(NEW)-ARRAY['status','updated_at'])=(to_jsonb(OLD)-ARRAY['status','updated_at']) THEN RETURN NEW; END IF;
        ELSIF TG_TABLE_NAME='payment_consents' THEN
            IF NEW.revoked_at IS NOT NULL AND (to_jsonb(NEW)-ARRAY['revoked_at','revocation_reason','updated_at'])=(to_jsonb(OLD)-ARRAY['revoked_at','revocation_reason','updated_at']) THEN RETURN NEW; END IF;
        ELSIF TG_TABLE_NAME='auto_topup_policies' THEN
            IF NOT NEW.enabled AND NOT NEW.armed AND (to_jsonb(NEW)-ARRAY['enabled','armed','version','updated_at','updated_by'])=(to_jsonb(OLD)-ARRAY['enabled','armed','version','updated_at','updated_by']) THEN RETURN NEW; END IF;
        ELSIF TG_TABLE_NAME='payment_method_setup_sessions' THEN
            IF NEW.state='failed' AND NEW.invalidated_by_closure IS NOT NULL AND (to_jsonb(NEW)-ARRAY['state','invalidated_by_closure','updated_at'])=(to_jsonb(OLD)-ARRAY['state','invalidated_by_closure','updated_at']) THEN RETURN NEW; END IF;
        END IF;
    END IF;
    RAISE EXCEPTION 'new payer authority is fenced by cloud closure' USING ERRCODE='55000',CONSTRAINT='billing_cloud_closure_barrier';
END $$;
CREATE TRIGGER closure_new_intent BEFORE INSERT ON payment_intents FOR EACH ROW EXECUTE FUNCTION enforce_cloud_closure_payer_barrier();
CREATE TRIGGER closure_new_charge BEFORE INSERT ON payment_attempts FOR EACH ROW EXECUTE FUNCTION enforce_cloud_closure_payer_barrier();
CREATE TRIGGER closure_method_barrier BEFORE INSERT OR UPDATE ON payment_methods FOR EACH ROW EXECUTE FUNCTION enforce_cloud_closure_payer_barrier();
CREATE TRIGGER closure_consent_barrier BEFORE INSERT OR UPDATE ON payment_consents FOR EACH ROW EXECUTE FUNCTION enforce_cloud_closure_payer_barrier();
CREATE TRIGGER closure_policy_barrier BEFORE INSERT OR UPDATE ON auto_topup_policies FOR EACH ROW EXECUTE FUNCTION enforce_cloud_closure_payer_barrier();
CREATE TRIGGER closure_setup_barrier BEFORE INSERT OR UPDATE ON payment_method_setup_sessions FOR EACH ROW EXECUTE FUNCTION enforce_cloud_closure_payer_barrier();

CREATE FUNCTION enforce_closed_cloud_financial_barrier() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE target_account UUID; target_org UUID;
BEGIN
    IF TG_TABLE_NAME='commercial_accounts' THEN
        target_account:=NEW.id;
        IF NEW.available_balance_minor=OLD.available_balance_minor AND NEW.state=OLD.state THEN RETURN NEW; END IF;
    ELSIF TG_TABLE_NAME IN ('balance_ledger_entries','billing_invoices') THEN
        IF TG_OP='DELETE' THEN target_account:=OLD.account_id; ELSE target_account:=NEW.account_id; END IF;
    ELSIF TG_TABLE_NAME='invoice_settlement_links' THEN
        SELECT account_id INTO target_account FROM billing_invoices WHERE id=CASE WHEN TG_OP='DELETE' THEN OLD.invoice_id ELSE NEW.invoice_id END;
    ELSE
        IF TG_OP='DELETE' THEN target_org:=OLD.organization_id; ELSE target_org:=NEW.organization_id; END IF;
        SELECT id INTO target_account FROM commercial_accounts WHERE organization_id=target_org AND currency='TWD';
    END IF;
    PERFORM id FROM commercial_accounts WHERE id=target_account FOR UPDATE;
    IF EXISTS(SELECT 1 FROM billing_cloud_closures WHERE account_id=target_account AND phase='closed') THEN
        IF TG_TABLE_NAME='commercial_accounts' THEN
            IF NEW.state='closed' AND NEW.available_balance_minor=0 AND OLD.available_balance_minor=0 THEN RETURN NEW; END IF;
        ELSIF TG_TABLE_NAME='billing_access_states' THEN
            IF TG_OP<>'DELETE' AND NEW.state='closed' THEN RETURN NEW; END IF;
        END IF;
        RAISE EXCEPTION 'closed cloud financial history is immutable' USING ERRCODE='55000',CONSTRAINT='billing_cloud_closure_barrier';
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
END $$;
CREATE TRIGGER closed_cloud_balance BEFORE UPDATE OF available_balance_minor,state ON commercial_accounts FOR EACH ROW EXECUTE FUNCTION enforce_closed_cloud_financial_barrier();
CREATE TRIGGER closed_cloud_ledger BEFORE INSERT ON balance_ledger_entries FOR EACH ROW EXECUTE FUNCTION enforce_closed_cloud_financial_barrier();
CREATE TRIGGER closed_cloud_usage BEFORE INSERT OR UPDATE OR DELETE ON billing_usage_facts FOR EACH ROW EXECUTE FUNCTION enforce_closed_cloud_financial_barrier();
CREATE TRIGGER closed_cloud_period BEFORE INSERT OR UPDATE OR DELETE ON billing_periods FOR EACH ROW EXECUTE FUNCTION enforce_closed_cloud_financial_barrier();
CREATE TRIGGER closed_cloud_invoice BEFORE INSERT OR UPDATE OR DELETE ON billing_invoices FOR EACH ROW EXECUTE FUNCTION enforce_closed_cloud_financial_barrier();
CREATE TRIGGER closed_cloud_settlement BEFORE INSERT OR UPDATE OR DELETE ON invoice_settlement_links FOR EACH ROW EXECUTE FUNCTION enforce_closed_cloud_financial_barrier();
CREATE TRIGGER closed_cloud_access BEFORE INSERT OR UPDATE OR DELETE ON billing_access_states FOR EACH ROW EXECUTE FUNCTION enforce_closed_cloud_financial_barrier();

CREATE FUNCTION verify_billing_cloud_closure_completion() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS(SELECT 1 FROM billing_cloud_closures c JOIN commercial_accounts a ON a.id=c.account_id
        JOIN billing_responsibility_periods p ON p.id=c.source_period_id
        JOIN billing_access_states b ON b.organization_id=a.organization_id
        WHERE c.id=NEW.operation_id AND c.phase='closed' AND a.state='closed' AND a.available_balance_minor=0
          AND p.effective_until=NEW.closed_at AND b.state='closed'
          AND NOT EXISTS(SELECT 1 FROM payment_methods WHERE account_id=a.id AND status<>'revoked')
          AND NOT EXISTS(SELECT 1 FROM payment_consents WHERE account_id=a.id AND revoked_at IS NULL)
          AND NOT EXISTS(SELECT 1 FROM auto_topup_policies WHERE account_id=a.id AND (enabled OR armed))
          AND NOT EXISTS(SELECT 1 FROM billing_cloud_closure_revocations t WHERE t.operation_id=c.id AND NOT EXISTS(
              SELECT 1 FROM billing_cloud_closure_revocation_acks k WHERE (k.operation_id,k.subject_type,k.subject_id)=(t.operation_id,t.subject_type,t.subject_id)))
          AND NOT EXISTS(SELECT 1 FROM payment_methods m WHERE m.account_id=a.id AND NOT EXISTS(
              SELECT 1 FROM billing_cloud_closure_revocations t WHERE t.operation_id=c.id AND t.subject_type='method' AND t.subject_id=m.id))) THEN
        RAISE EXCEPTION 'closure completion does not match closed account and revocations' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER cloud_closure_completion_consistency AFTER INSERT ON billing_cloud_closure_completions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION verify_billing_cloud_closure_completion();

CREATE FUNCTION preserve_closed_cloud_responsibility() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE closing_time TIMESTAMPTZ;
BEGIN
    IF TG_OP='INSERT' THEN
        PERFORM id FROM commercial_accounts WHERE id=NEW.account_id FOR UPDATE;
        IF EXISTS(SELECT 1 FROM billing_cloud_closures WHERE account_id=NEW.account_id AND phase='closed') THEN
            RAISE EXCEPTION 'closed cloud cannot acquire a new responsibility period' USING ERRCODE='55000',CONSTRAINT='billing_cloud_closure_barrier';
        END IF;
        RETURN NEW;
    END IF;
    SELECT d.closed_at INTO closing_time FROM billing_cloud_closures c JOIN billing_cloud_closure_completions d ON d.operation_id=c.id
        WHERE c.source_period_id=OLD.id AND c.phase='closed';
    IF closing_time IS NOT NULL THEN
        IF TG_OP='UPDATE' THEN
            IF NEW.effective_until=closing_time AND (to_jsonb(NEW)-'effective_until')=(to_jsonb(OLD)-'effective_until') THEN RETURN NEW; END IF;
        END IF;
        RAISE EXCEPTION 'closed cloud responsibility history cannot be changed' USING ERRCODE='55000',CONSTRAINT='billing_cloud_closure_barrier';
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
END $$;
CREATE TRIGGER closed_cloud_responsibility BEFORE INSERT OR UPDATE OR DELETE ON billing_responsibility_periods FOR EACH ROW EXECUTE FUNCTION preserve_closed_cloud_responsibility();
