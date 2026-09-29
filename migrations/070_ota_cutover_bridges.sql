-- A bridge closes the last local month at the first published OTA UTC boundary.
-- Keep its reviewed input and original invoice for idempotent operator retries.
CREATE TABLE ota_cutover_bridges (
    organization_id UUID NOT NULL,
    pricing_version_id UUID NOT NULL REFERENCES ota_pricing_publications(pricing_version_id) ON DELETE RESTRICT,
    invoice_id UUID NOT NULL UNIQUE REFERENCES billing_invoices(id) ON DELETE RESTRICT,
    review_sha256 CHAR(64) NOT NULL CHECK (review_sha256 ~ '^[0-9a-f]{64}$'),
    review JSONB NOT NULL,
    created_by TEXT NOT NULL CHECK (btrim(created_by) <> ''),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (organization_id, pricing_version_id)
);
CREATE TRIGGER ota_cutover_bridges_immutable
BEFORE UPDATE OR DELETE ON ota_cutover_bridges
FOR EACH ROW EXECUTE FUNCTION reject_ota_pricing_approval_mutation();
