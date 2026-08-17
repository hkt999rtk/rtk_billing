-- Versioned pricing, immutable invoices, customer billing profiles, and
-- customer-safe billing activity projection. Usage facts remain separate from
-- operational logs and payment-provider state.

CREATE TABLE IF NOT EXISTS billing_profiles (
    organization_id UUID PRIMARY KEY,
    legal_name TEXT NOT NULL,
    tax_identifier TEXT,
    billing_address TEXT,
    contact_email TEXT,
    locale TEXT NOT NULL DEFAULT 'zh-TW',
    timezone TEXT NOT NULL DEFAULT 'Asia/Taipei',
    delivery_preference TEXT NOT NULL DEFAULT 'portal',
    version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_profiles_legal_name_not_blank CHECK (btrim(legal_name) <> ''),
    CONSTRAINT billing_profiles_tax_identifier_length CHECK (tax_identifier IS NULL OR length(tax_identifier) <= 64),
    CONSTRAINT billing_profiles_contact_email_check CHECK (contact_email IS NULL OR position('@' in contact_email) > 1),
    CONSTRAINT billing_profiles_locale_not_blank CHECK (btrim(locale) <> ''),
    CONSTRAINT billing_profiles_timezone_not_blank CHECK (btrim(timezone) <> ''),
    CONSTRAINT billing_profiles_delivery_check CHECK (delivery_preference IN ('portal', 'portal_and_email')),
    CONSTRAINT billing_profiles_version_check CHECK (version > 0)
);

CREATE TABLE IF NOT EXISTS pricing_plan_versions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_key TEXT NOT NULL,
    version BIGINT NOT NULL,
    currency CHAR(3) NOT NULL,
    status TEXT NOT NULL DEFAULT 'draft',
    effective_from TIMESTAMPTZ NOT NULL,
    effective_until TIMESTAMPTZ,
    activated_at TIMESTAMPTZ,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pricing_plan_key_not_blank CHECK (btrim(plan_key) <> ''),
    CONSTRAINT pricing_plan_version_check CHECK (version > 0),
    CONSTRAINT pricing_plan_currency_check CHECK (currency = 'TWD'),
    CONSTRAINT pricing_plan_status_check CHECK (status IN ('draft', 'active', 'retired')),
    CONSTRAINT pricing_plan_effective_check CHECK (effective_until IS NULL OR effective_until > effective_from),
    CONSTRAINT pricing_plan_actor_not_blank CHECK (btrim(created_by) <> ''),
    CONSTRAINT pricing_plan_key_version_unique UNIQUE (plan_key, version)
);

CREATE UNIQUE INDEX IF NOT EXISTS pricing_plan_active_interval_idx
    ON pricing_plan_versions (plan_key, effective_from)
    WHERE status = 'active';

CREATE TABLE IF NOT EXISTS pricing_rates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pricing_version_id UUID NOT NULL REFERENCES pricing_plan_versions(id) ON DELETE RESTRICT,
    service_code TEXT NOT NULL,
    metric_code TEXT NOT NULL,
    description TEXT NOT NULL,
    unit TEXT NOT NULL,
    unit_price_minor BIGINT NOT NULL,
    unit_price_scale SMALLINT NOT NULL DEFAULT 0,
    rounding_mode TEXT NOT NULL DEFAULT 'half_up',
    tax_rate_basis_points INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pricing_rates_codes_not_blank CHECK (btrim(service_code) <> '' AND btrim(metric_code) <> ''),
    CONSTRAINT pricing_rates_description_not_blank CHECK (btrim(description) <> ''),
    CONSTRAINT pricing_rates_unit_not_blank CHECK (btrim(unit) <> ''),
    CONSTRAINT pricing_rates_amount_check CHECK (unit_price_minor >= 0),
    CONSTRAINT pricing_rates_scale_check CHECK (unit_price_scale BETWEEN 0 AND 9),
    CONSTRAINT pricing_rates_rounding_check CHECK (rounding_mode IN ('half_up', 'down', 'up')),
    CONSTRAINT pricing_rates_tax_check CHECK (tax_rate_basis_points BETWEEN 0 AND 10000),
    CONSTRAINT pricing_rates_metric_unique UNIQUE (pricing_version_id, service_code, metric_code, unit)
);

CREATE TABLE IF NOT EXISTS billing_usage_facts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    usage_id TEXT NOT NULL UNIQUE,
    organization_id UUID NOT NULL,
    service_code TEXT NOT NULL,
    metric_code TEXT NOT NULL,
    quantity BIGINT NOT NULL,
    quantity_scale SMALLINT NOT NULL DEFAULT 0,
    unit TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    source TEXT NOT NULL,
    source_sha256 CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_usage_codes_not_blank CHECK (btrim(service_code) <> '' AND btrim(metric_code) <> ''),
    CONSTRAINT billing_usage_quantity_check CHECK (quantity >= 0 AND quantity_scale BETWEEN 0 AND 9),
    CONSTRAINT billing_usage_unit_not_blank CHECK (btrim(unit) <> ''),
    CONSTRAINT billing_usage_window_check CHECK (window_end > window_start),
    CONSTRAINT billing_usage_source_not_blank CHECK (btrim(source) <> ''),
    CONSTRAINT billing_usage_source_sha256_check CHECK (source_sha256 ~ '^[0-9a-f]{64}$')
);

CREATE INDEX IF NOT EXISTS billing_usage_org_window_idx
    ON billing_usage_facts (organization_id, window_start, window_end);

CREATE TABLE IF NOT EXISTS billing_periods (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    currency CHAR(3) NOT NULL,
    period_start TIMESTAMPTZ NOT NULL,
    period_end TIMESTAMPTZ NOT NULL,
    state TEXT NOT NULL DEFAULT 'open',
    pricing_version_id UUID REFERENCES pricing_plan_versions(id) ON DELETE RESTRICT,
    usage_locked_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,
    close_error_code TEXT,
    version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_period_currency_check CHECK (currency = 'TWD'),
    CONSTRAINT billing_period_window_check CHECK (period_end > period_start),
    CONSTRAINT billing_period_state_check CHECK (state IN ('open', 'closing', 'closed', 'incomplete')),
    CONSTRAINT billing_period_version_check CHECK (version > 0),
    CONSTRAINT billing_period_unique UNIQUE (organization_id, currency, period_start, period_end)
);

CREATE SEQUENCE IF NOT EXISTS billing_invoice_number_seq AS BIGINT START WITH 1;

CREATE TABLE IF NOT EXISTS billing_invoices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_number TEXT NOT NULL UNIQUE,
    organization_id UUID NOT NULL,
    account_id UUID NOT NULL REFERENCES commercial_accounts(id) ON DELETE RESTRICT,
    period_id UUID NOT NULL UNIQUE REFERENCES billing_periods(id) ON DELETE RESTRICT,
    pricing_version_id UUID NOT NULL REFERENCES pricing_plan_versions(id) ON DELETE RESTRICT,
    currency CHAR(3) NOT NULL,
    state TEXT NOT NULL DEFAULT 'draft',
    period_start TIMESTAMPTZ NOT NULL,
    period_end TIMESTAMPTZ NOT NULL,
    subtotal_minor BIGINT NOT NULL DEFAULT 0,
    tax_minor BIGINT NOT NULL DEFAULT 0,
    total_minor BIGINT NOT NULL DEFAULT 0,
    amount_settled_minor BIGINT NOT NULL DEFAULT 0,
    amount_due_minor BIGINT NOT NULL DEFAULT 0,
    recipient_snapshot JSONB NOT NULL,
    issued_at TIMESTAMPTZ,
    due_at TIMESTAMPTZ,
    settled_at TIMESTAMPTZ,
    voided_at TIMESTAMPTZ,
    version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_invoice_number_not_blank CHECK (btrim(invoice_number) <> ''),
    CONSTRAINT billing_invoice_currency_check CHECK (currency = 'TWD'),
    CONSTRAINT billing_invoice_state_check CHECK (state IN ('draft', 'issued', 'settled', 'partially_settled', 'overdue', 'void')),
    CONSTRAINT billing_invoice_window_check CHECK (period_end > period_start),
    CONSTRAINT billing_invoice_amounts_check CHECK (
        subtotal_minor >= 0 AND tax_minor >= 0 AND total_minor >= 0 AND
        total_minor = subtotal_minor + tax_minor AND
        amount_settled_minor >= 0 AND amount_due_minor >= 0 AND
        amount_settled_minor + amount_due_minor = total_minor
    ),
    CONSTRAINT billing_invoice_issue_check CHECK ((state = 'draft' AND issued_at IS NULL) OR state <> 'draft'),
    CONSTRAINT billing_invoice_version_check CHECK (version > 0)
);

CREATE INDEX IF NOT EXISTS billing_invoice_org_created_idx
    ON billing_invoices (organization_id, created_at DESC);

CREATE TABLE IF NOT EXISTS billing_invoice_lines (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id UUID NOT NULL REFERENCES billing_invoices(id) ON DELETE RESTRICT,
    pricing_rate_id UUID NOT NULL REFERENCES pricing_rates(id) ON DELETE RESTRICT,
    service_code TEXT NOT NULL,
    metric_code TEXT NOT NULL,
    description TEXT NOT NULL,
    quantity BIGINT NOT NULL,
    quantity_scale SMALLINT NOT NULL,
    unit TEXT NOT NULL,
    unit_price_minor BIGINT NOT NULL,
    unit_price_scale SMALLINT NOT NULL,
    subtotal_minor BIGINT NOT NULL,
    tax_minor BIGINT NOT NULL,
    total_minor BIGINT NOT NULL,
    rounding_mode TEXT NOT NULL,
    usage_fact_refs JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_invoice_line_quantity_check CHECK (quantity >= 0 AND quantity_scale BETWEEN 0 AND 9),
    CONSTRAINT billing_invoice_line_price_check CHECK (unit_price_minor >= 0 AND unit_price_scale BETWEEN 0 AND 9),
    CONSTRAINT billing_invoice_line_amounts_check CHECK (subtotal_minor >= 0 AND tax_minor >= 0 AND total_minor = subtotal_minor + tax_minor),
    CONSTRAINT billing_invoice_line_rounding_check CHECK (rounding_mode IN ('half_up', 'down', 'up')),
    CONSTRAINT billing_invoice_line_rate_unique UNIQUE (invoice_id, pricing_rate_id)
);

CREATE TABLE IF NOT EXISTS billing_invoice_documents (
    invoice_id UUID PRIMARY KEY REFERENCES billing_invoices(id) ON DELETE RESTRICT,
    content_type TEXT NOT NULL DEFAULT 'application/pdf',
    document_bytes BYTEA NOT NULL,
    byte_length BIGINT NOT NULL,
    sha256 CHAR(64) NOT NULL,
    renderer_version TEXT NOT NULL,
    invoice_version BIGINT NOT NULL,
    generated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT billing_invoice_document_type_check CHECK (content_type = 'application/pdf'),
    CONSTRAINT billing_invoice_document_length_check CHECK (byte_length > 0 AND octet_length(document_bytes) = byte_length),
    CONSTRAINT billing_invoice_document_sha_check CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT billing_invoice_renderer_not_blank CHECK (btrim(renderer_version) <> ''),
    CONSTRAINT billing_invoice_document_version_check CHECK (invoice_version > 0)
);

CREATE TABLE IF NOT EXISTS invoice_settlement_links (
    invoice_id UUID PRIMARY KEY REFERENCES billing_invoices(id) ON DELETE RESTRICT,
    ledger_entry_id UUID UNIQUE REFERENCES balance_ledger_entries(id) ON DELETE RESTRICT,
    state TEXT NOT NULL DEFAULT 'pending',
    last_error_code TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT invoice_settlement_state_check CHECK (state IN ('pending', 'posted', 'retrying', 'failed')),
    CONSTRAINT invoice_settlement_attempt_check CHECK (attempt_count >= 0)
);

CREATE TABLE IF NOT EXISTS billing_activity_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    customer_reference TEXT NOT NULL,
    activity_type TEXT NOT NULL,
    state TEXT NOT NULL,
    amount_minor BIGINT NOT NULL,
    currency CHAR(3) NOT NULL,
    balance_effect TEXT NOT NULL,
    action TEXT NOT NULL DEFAULT 'none',
    message_key TEXT,
    resource_type TEXT NOT NULL,
    resource_id UUID,
    correlation_id TEXT,
    retry_scheduled BOOLEAN NOT NULL DEFAULT false,
    next_retry_at TIMESTAMPTZ,
    occurred_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT billing_activity_reference_not_blank CHECK (btrim(customer_reference) <> ''),
    CONSTRAINT billing_activity_type_check CHECK (activity_type IN ('invoice', 'balance_adjustment', 'manual_top_up', 'automatic_top_up', 'refund', 'chargeback', 'legacy')),
    CONSTRAINT billing_activity_state_check CHECK (state IN ('action_required', 'processing', 'pending_reconciliation', 'completed', 'failed', 'unavailable')),
    CONSTRAINT billing_activity_amount_check CHECK (amount_minor >= 0),
    CONSTRAINT billing_activity_currency_check CHECK (currency = 'TWD'),
    CONSTRAINT billing_activity_balance_effect_check CHECK (balance_effect IN ('credit', 'debit', 'none', 'unknown')),
    CONSTRAINT billing_activity_action_check CHECK (action IN ('none', 'update_payment_method', 'enable_auto_topup', 'contact_support')),
    CONSTRAINT billing_activity_resource_not_blank CHECK (btrim(resource_type) <> '')
);

CREATE UNIQUE INDEX IF NOT EXISTS billing_activity_resource_unique_idx
    ON billing_activity_events (organization_id, resource_type, resource_id)
    WHERE resource_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS billing_activity_org_time_idx
    ON billing_activity_events (organization_id, occurred_at DESC);
