-- Existing rate cards predate explicit meter precision and tax review. Leave
-- those fields unknown for historical rows; future publication must supply
-- reviewed values rather than treating a zero tax rate as an exemption.
ALTER TABLE pricing_rates
    ADD COLUMN IF NOT EXISTS quantity_scale SMALLINT,
    ADD COLUMN IF NOT EXISTS tax_category TEXT;

ALTER TABLE pricing_rates
    ADD CONSTRAINT pricing_rates_quantity_scale_check
        CHECK (quantity_scale IS NULL OR quantity_scale BETWEEN 0 AND 9),
    ADD CONSTRAINT pricing_rates_tax_category_not_blank
        CHECK (tax_category IS NULL OR btrim(tax_category) <> '');
