-- Preserve the published card and its approval record when a future OTA
-- cutover is canceled. The previous card resumes without rewriting prices.
ALTER TABLE pricing_plan_versions DROP CONSTRAINT pricing_plan_status_check;
ALTER TABLE pricing_plan_versions ADD CONSTRAINT pricing_plan_status_check
    CHECK (status IN ('draft', 'active', 'retired', 'canceled'));

CREATE TABLE ota_pricing_cancellations (
    pricing_version_id UUID PRIMARY KEY REFERENCES ota_pricing_publications(pricing_version_id) ON DELETE RESTRICT,
    base_version_id UUID NOT NULL REFERENCES pricing_plan_versions(id) ON DELETE RESTRICT,
    first_reviewer TEXT NOT NULL CHECK (btrim(first_reviewer) <> ''),
    second_reviewer TEXT NOT NULL CHECK (btrim(second_reviewer) <> '' AND second_reviewer <> first_reviewer),
    reason TEXT NOT NULL CHECK (btrim(reason) <> ''),
    approved_at TIMESTAMPTZ NOT NULL,
    canceled_at TIMESTAMPTZ NOT NULL
);

CREATE FUNCTION reject_ota_pricing_approval_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'OTA pricing approval records are immutable' USING ERRCODE='23514';
END;
$$;

CREATE TRIGGER ota_pricing_publications_immutable
BEFORE UPDATE OR DELETE ON ota_pricing_publications
FOR EACH ROW EXECUTE FUNCTION reject_ota_pricing_approval_mutation();

CREATE TRIGGER ota_pricing_cancellations_immutable
BEFORE UPDATE OR DELETE ON ota_pricing_cancellations
FOR EACH ROW EXECUTE FUNCTION reject_ota_pricing_approval_mutation();
