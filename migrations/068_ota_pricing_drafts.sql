-- The reviewed OTA draft is assembled from the active complete rate card in
-- one transaction. Its technical manifest stays immutable through publication.
CREATE TABLE ota_pricing_drafts (
    pricing_version_id UUID PRIMARY KEY REFERENCES pricing_plan_versions(id) ON DELETE RESTRICT,
    base_version_id UUID NOT NULL REFERENCES pricing_plan_versions(id) ON DELETE RESTRICT,
    rate_set_sha256 CHAR(64) NOT NULL CHECK (rate_set_sha256 ~ '^[0-9a-f]{64}$'),
    scope_code TEXT NOT NULL CHECK (scope_code = 'commercial_active_product_ota'),
    effective_from TIMESTAMPTZ NOT NULL,
    created_by TEXT NOT NULL CHECK (btrim(created_by) <> ''),
    created_at TIMESTAMPTZ NOT NULL,
    tax_mode TEXT NOT NULL CHECK (tax_mode = 'invoice_total'),
    tax_rate_basis_points INTEGER NOT NULL CHECK (tax_rate_basis_points = 500),
    tax_rounding_mode TEXT NOT NULL CHECK (tax_rounding_mode = 'half_up'),
    tax_category TEXT NOT NULL CHECK (tax_category = 'standard')
);

CREATE FUNCTION reject_ota_pricing_draft_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'OTA pricing draft manifests are immutable';
END;
$$;

CREATE TRIGGER ota_pricing_drafts_immutable
BEFORE UPDATE OR DELETE ON ota_pricing_drafts
FOR EACH ROW EXECUTE FUNCTION reject_ota_pricing_draft_mutation();
