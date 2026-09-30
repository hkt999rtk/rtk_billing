-- The first complete TWD card may use the approved invoice-total tax policy.
-- Its immutable review record prevents an unreviewed draft from taking effect.
CREATE TABLE reviewed_initial_pricing_publications (
    pricing_version_id UUID PRIMARY KEY REFERENCES pricing_plan_versions(id) ON DELETE RESTRICT,
    rate_set_sha256 CHAR(64) NOT NULL CHECK (rate_set_sha256 ~ '^[0-9a-f]{64}$'),
    approval_reference TEXT NOT NULL CHECK (btrim(approval_reference) <> ''),
    approved_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ NOT NULL,
    effective_from TIMESTAMPTZ NOT NULL,
    tax_mode TEXT NOT NULL CHECK (tax_mode = 'invoice_total'),
    tax_rate_basis_points INTEGER NOT NULL CHECK (tax_rate_basis_points = 500),
    tax_rounding_mode TEXT NOT NULL CHECK (tax_rounding_mode = 'half_up'),
    tax_category TEXT NOT NULL CHECK (tax_category = 'standard')
);

CREATE FUNCTION reject_initial_pricing_publication_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'initial pricing publication is immutable' USING ERRCODE='23514';
END;
$$;

CREATE TRIGGER reviewed_initial_pricing_publications_immutable
BEFORE UPDATE OR DELETE ON reviewed_initial_pricing_publications
FOR EACH ROW EXECUTE FUNCTION reject_initial_pricing_publication_mutation();
