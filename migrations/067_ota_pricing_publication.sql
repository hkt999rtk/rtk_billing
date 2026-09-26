-- A reviewed OTA publication binds the complete card, prior effective card,
-- approved tax treatment, scope, and reviewers before a future UTC cutover.
CREATE TABLE ota_pricing_publications (
    pricing_version_id UUID PRIMARY KEY REFERENCES pricing_plan_versions(id) ON DELETE RESTRICT,
    base_version_id UUID NOT NULL REFERENCES pricing_plan_versions(id) ON DELETE RESTRICT,
    rate_set_sha256 CHAR(64) NOT NULL CHECK (rate_set_sha256 ~ '^[0-9a-f]{64}$'),
    scope_code TEXT NOT NULL CHECK (scope_code = 'commercial_active_product_ota'),
    first_reviewer TEXT NOT NULL CHECK (btrim(first_reviewer) <> ''),
    second_reviewer TEXT NOT NULL CHECK (btrim(second_reviewer) <> '' AND second_reviewer <> first_reviewer),
    approved_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ NOT NULL,
    effective_from TIMESTAMPTZ NOT NULL,
    tax_mode TEXT NOT NULL CHECK (tax_mode = 'invoice_total'),
    tax_rate_basis_points INTEGER NOT NULL CHECK (tax_rate_basis_points = 500),
    tax_rounding_mode TEXT NOT NULL CHECK (tax_rounding_mode = 'half_up'),
    tax_category TEXT NOT NULL CHECK (tax_category = 'standard')
);
