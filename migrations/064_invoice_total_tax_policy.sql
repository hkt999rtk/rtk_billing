-- Preserve legacy line-tax invoices while allowing a future reviewed price
-- version to calculate one tax amount on the invoice subtotal.
ALTER TABLE pricing_plan_versions
    ADD COLUMN tax_mode TEXT NOT NULL DEFAULT 'line',
    ADD COLUMN invoice_tax_rate_basis_points INTEGER,
    ADD COLUMN invoice_tax_rounding_mode TEXT,
    ADD COLUMN invoice_tax_category TEXT;

ALTER TABLE pricing_plan_versions
    ADD CONSTRAINT pricing_plan_tax_policy_check CHECK (
        (tax_mode = 'line' AND invoice_tax_rate_basis_points IS NULL
            AND invoice_tax_rounding_mode IS NULL AND invoice_tax_category IS NULL)
        OR (tax_mode = 'invoice_total' AND invoice_tax_rate_basis_points BETWEEN 0 AND 10000
            AND invoice_tax_rounding_mode IN ('half_up', 'down', 'up')
            AND invoice_tax_category IS NOT NULL AND btrim(invoice_tax_category) <> '')
    );

ALTER TABLE billing_invoices
    ADD COLUMN tax_mode TEXT NOT NULL DEFAULT 'line',
    ADD COLUMN invoice_tax_rate_basis_points INTEGER,
    ADD COLUMN invoice_tax_rounding_mode TEXT,
    ADD COLUMN invoice_tax_category TEXT;

ALTER TABLE billing_invoices
    ADD CONSTRAINT billing_invoice_tax_policy_check CHECK (
        (tax_mode = 'line' AND invoice_tax_rate_basis_points IS NULL
            AND invoice_tax_rounding_mode IS NULL AND invoice_tax_category IS NULL)
        OR (tax_mode = 'invoice_total' AND invoice_tax_rate_basis_points BETWEEN 0 AND 10000
            AND invoice_tax_rounding_mode IN ('half_up', 'down', 'up')
            AND invoice_tax_category IS NOT NULL AND btrim(invoice_tax_category) <> '')
    );
