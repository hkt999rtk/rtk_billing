ALTER TABLE billing_usage_facts ADD COLUMN product_id UUID;
CREATE INDEX billing_usage_product_window_idx ON billing_usage_facts (organization_id, product_id, window_start, window_end) WHERE product_id IS NOT NULL;

ALTER TABLE billing_invoice_lines ADD COLUMN product_id UUID;
ALTER TABLE billing_invoice_lines DROP CONSTRAINT billing_invoice_line_rate_unique;
CREATE UNIQUE INDEX billing_invoice_line_rate_product_unique
    ON billing_invoice_lines (invoice_id, pricing_rate_id, COALESCE(product_id::text, ''));
