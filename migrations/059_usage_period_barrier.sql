-- Existing closed-cloud and handoff guards acquire the account row first.
-- This guard runs afterward (PostgreSQL orders same-kind triggers by name),
-- then reads a fresh READ COMMITTED snapshot, including a close that committed
-- while the INSERT was waiting for its account lock. The application precheck
-- is only an early response; it is not the authoritative write barrier.
CREATE FUNCTION enforce_usage_period_barrier() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS(SELECT 1 FROM billing_periods
        WHERE organization_id=NEW.organization_id AND state IN ('closing','closed')
        AND tstzrange(period_start,period_end,'[)') && tstzrange(NEW.window_start,NEW.window_end,'[)')) THEN
        RAISE EXCEPTION 'usage overlaps a locked billing period'
            USING ERRCODE='55000',CONSTRAINT='billing_usage_period_barrier';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER usage_period_barrier BEFORE INSERT ON billing_usage_facts
    FOR EACH ROW EXECUTE FUNCTION enforce_usage_period_barrier();
