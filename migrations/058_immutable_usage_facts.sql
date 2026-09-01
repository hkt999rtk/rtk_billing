-- Usage corrections are new, auditable facts; accepted evidence is immutable.
-- Existing history is retained unchanged. No ownership or completeness is inferred.
CREATE FUNCTION reject_billing_usage_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'accepted usage evidence is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER billing_usage_immutable BEFORE UPDATE OR DELETE ON billing_usage_facts
    FOR EACH ROW EXECUTE FUNCTION reject_billing_usage_mutation();
