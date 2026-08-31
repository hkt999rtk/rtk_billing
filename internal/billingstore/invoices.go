package billingstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

type PrepareInvoiceInput struct {
	OrganizationID string
	AccountID      string
	Currency       billing.Currency
	PeriodStart    time.Time
	PeriodEnd      time.Time
	DueAt          time.Time
	Now            time.Time
}

type InvoiceFilter struct {
	State         billing.InvoiceState
	InvoiceNumber string
	PeriodStart   *time.Time
	PeriodEnd     *time.Time
	Limit         int
	Offset        int
}

type InvoicePage struct {
	Invoices []billing.Invoice `json:"invoices"`
	Page     Page              `json:"pagination"`
}

func (s *Store) PrepareInvoice(ctx context.Context, in PrepareInvoiceInput) (billing.Invoice, bool, error) {
	if !required(in.OrganizationID) || !required(in.AccountID) || in.Currency != billing.CurrencyTWD ||
		!in.PeriodEnd.After(in.PeriodStart) {
		return billing.Invoice{}, false, ErrConflict
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	if in.DueAt.IsZero() {
		in.DueAt = in.Now.AddDate(0, 0, 14)
	}
	var periodID, periodState string
	err := s.db.QueryRow(ctx, `
		INSERT INTO billing_periods (organization_id, currency, period_start, period_end, state, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'closing', $5, $5)
		ON CONFLICT (organization_id, currency, period_start, period_end)
		DO UPDATE SET updated_at = billing_periods.updated_at
		RETURNING id::text, state
	`, in.OrganizationID, in.Currency, in.PeriodStart.UTC(), in.PeriodEnd.UTC(), in.Now.UTC()).Scan(&periodID, &periodState)
	if err != nil {
		return billing.Invoice{}, false, err
	}
	if existing, err := s.GetInvoiceByPeriod(ctx, in.OrganizationID, periodID); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return billing.Invoice{}, false, err
	}
	if periodState == "closed" {
		return billing.Invoice{}, false, ErrIncomplete
	}
	pricing, err := s.ActivePricingVersion(ctx, in.PeriodStart, in.Currency)
	if err != nil {
		_ = s.markPeriodIncomplete(ctx, periodID, "pricing_unavailable", in.Now)
		return billing.Invoice{}, false, err
	}
	facts, err := s.ListUsageFacts(ctx, in.OrganizationID, in.PeriodStart, in.PeriodEnd)
	if err != nil {
		return billing.Invoice{}, false, err
	}
	if len(facts) == 0 {
		_ = s.markPeriodIncomplete(ctx, periodID, "usage_missing", in.Now)
		return billing.Invoice{}, false, ErrIncomplete
	}
	profile, _, err := s.EnsureBillingProfile(ctx, in.OrganizationID, in.Now)
	if err != nil {
		_ = s.markPeriodIncomplete(ctx, periodID, "billing_profile_missing", in.Now)
		return billing.Invoice{}, false, err
	}
	if profile.RequiresConfiguration {
		_ = s.markPeriodIncomplete(ctx, periodID, "billing_profile_configuration_required", in.Now)
		return billing.Invoice{}, false, billing.ErrProfileConfigurationRequired
	}
	draft, err := billing.BuildDraftInvoice(billing.Invoice{
		OrganizationID: in.OrganizationID, AccountID: in.AccountID, PeriodID: periodID,
		PricingVersionID: pricing.ID, Currency: in.Currency, PeriodStart: in.PeriodStart.UTC(), PeriodEnd: in.PeriodEnd.UTC(),
		Recipient: profile, Version: 1, CreatedAt: in.Now.UTC(), UpdatedAt: in.Now.UTC(),
	}, facts, pricing.Rates)
	if err != nil {
		_ = s.markPeriodIncomplete(ctx, periodID, "invoice_calculation_invalid", in.Now)
		return billing.Invoice{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Invoice{}, false, err
	}
	defer tx.Rollback(ctx)
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM billing_periods WHERE id = $1 FOR UPDATE`, periodID).Scan(&state); err != nil {
		return billing.Invoice{}, false, err
	}
	var existingID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM billing_invoices WHERE period_id = $1`, periodID).Scan(&existingID); err == nil {
		if err := tx.Commit(ctx); err != nil {
			return billing.Invoice{}, false, err
		}
		existing, err := s.GetInvoice(ctx, in.OrganizationID, existingID)
		return existing, false, err
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return billing.Invoice{}, false, err
	}
	var sequence int64
	if err := tx.QueryRow(ctx, `SELECT nextval('billing_invoice_number_seq')`).Scan(&sequence); err != nil {
		return billing.Invoice{}, false, err
	}
	number := fmt.Sprintf("INV-%04d-%06d", in.Now.UTC().Year(), sequence)
	issued, err := billing.IssueInvoice(draft, number, in.Now, in.DueAt)
	if err != nil {
		return billing.Invoice{}, false, err
	}
	recipientJSON, err := json.Marshal(issued.Recipient)
	if err != nil {
		return billing.Invoice{}, false, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO billing_invoices (invoice_number, organization_id, account_id, period_id, pricing_version_id,
		    currency, state, period_start, period_end, subtotal_minor, tax_minor, total_minor,
		    amount_settled_minor, amount_due_minor, recipient_snapshot, issued_at, due_at, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,0,$12,$13,$14,$15,$16,$17,$17)
		RETURNING id::text
	`, issued.InvoiceNumber, issued.OrganizationID, issued.AccountID, periodID, issued.PricingVersionID,
		issued.Currency, issued.State, issued.PeriodStart, issued.PeriodEnd, issued.SubtotalMinor, issued.TaxMinor,
		issued.TotalMinor, recipientJSON, issued.IssuedAt, issued.DueAt, issued.Version, in.Now.UTC()).Scan(&issued.ID)
	if err != nil {
		return billing.Invoice{}, false, err
	}
	for i := range issued.Lines {
		refs, err := json.Marshal(issued.Lines[i].UsageFactRefs)
		if err != nil {
			return billing.Invoice{}, false, err
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO billing_invoice_lines (invoice_id, pricing_rate_id, service_code, metric_code, description,
			    quantity, quantity_scale, unit, unit_price_minor, unit_price_scale, subtotal_minor, tax_minor,
			    total_minor, rounding_mode, usage_fact_refs, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			RETURNING id::text
		`, issued.ID, issued.Lines[i].PricingRateID, issued.Lines[i].ServiceCode, issued.Lines[i].MetricCode,
			issued.Lines[i].Description, issued.Lines[i].Quantity, issued.Lines[i].QuantityScale, issued.Lines[i].Unit,
			issued.Lines[i].UnitPriceMinor, issued.Lines[i].UnitPriceScale, issued.Lines[i].SubtotalMinor,
			issued.Lines[i].TaxMinor, issued.Lines[i].TotalMinor, issued.Lines[i].RoundingMode, refs, in.Now.UTC()).Scan(&issued.Lines[i].ID)
		if err != nil {
			return billing.Invoice{}, false, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE billing_periods SET state = 'closed', pricing_version_id = $2, usage_locked_at = $3,
		    closed_at = $3, close_error_code = NULL, version = version + 1, updated_at = $3 WHERE id = $1
	`, periodID, pricing.ID, in.Now.UTC()); err != nil {
		return billing.Invoice{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.Invoice{}, false, err
	}
	return issued, true, nil
}

func (s *Store) markPeriodIncomplete(ctx context.Context, periodID, code string, now time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE billing_periods SET state = 'incomplete', close_error_code = $2, version = version + 1, updated_at = $3 WHERE id = $1 AND state <> 'closed'`, periodID, code, now.UTC())
	return err
}

func (s *Store) GetInvoiceByPeriod(ctx context.Context, organizationID, periodID string) (billing.Invoice, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, organizationID, func(view *Store) (billing.Invoice, error) {
			return view.GetInvoiceByPeriod(ctx, organizationID, periodID)
		})
	}
	var id string
	err := s.db.QueryRow(ctx, `SELECT id::text FROM billing_invoices WHERE organization_id = $1 AND period_id = $2`, organizationID, periodID).Scan(&id)
	if err != nil {
		return billing.Invoice{}, mapNotFound(err)
	}
	return s.GetInvoice(ctx, organizationID, id)
}

func (s *Store) GetInvoice(ctx context.Context, organizationID, invoiceID string) (billing.Invoice, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, organizationID, func(view *Store) (billing.Invoice, error) { return view.GetInvoice(ctx, organizationID, invoiceID) })
	}
	args := []any{organizationID, invoiceID}
	visibility := invoiceVisibility(ctx, &args)
	invoice, err := scanInvoice(s.db.QueryRow(ctx, invoiceSelect+` WHERE invoices.organization_id = $1 AND invoices.id = $2 AND `+visibility, args...))
	if err != nil {
		return billing.Invoice{}, err
	}
	lines, err := s.listInvoiceLines(ctx, invoice.ID)
	if err != nil {
		return billing.Invoice{}, err
	}
	invoice.Lines = lines
	document, err := s.GetInvoiceDocument(ctx, organizationID, invoice.ID)
	if err == nil {
		invoice.Document = &document.Metadata
	} else if !errors.Is(err, ErrNotFound) {
		return billing.Invoice{}, err
	}
	return invoice, nil
}

func (s *Store) ListInvoices(ctx context.Context, organizationID string, filter InvoiceFilter) (InvoicePage, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, organizationID, func(view *Store) (InvoicePage, error) { return view.ListInvoices(ctx, organizationID, filter) })
	}
	if filter.Limit <= 0 || filter.Limit > 100 {
		filter.Limit = 25
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	args := []any{organizationID}
	conditions := []string{"invoices.organization_id = $1"}
	conditions = append(conditions, invoiceVisibility(ctx, &args))
	appendCondition := func(sql string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(sql, len(args)))
	}
	if filter.State != "" {
		appendCondition("invoices.state = $%d", filter.State)
	}
	if strings.TrimSpace(filter.InvoiceNumber) != "" {
		appendCondition("invoices.invoice_number = $%d", strings.TrimSpace(filter.InvoiceNumber))
	}
	if filter.PeriodStart != nil {
		appendCondition("invoices.period_start >= $%d", filter.PeriodStart.UTC())
	}
	if filter.PeriodEnd != nil {
		appendCondition("invoices.period_end <= $%d", filter.PeriodEnd.UTC())
	}
	where := strings.Join(conditions, " AND ")
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM billing_invoices AS invoices WHERE `+where, args...).Scan(&total); err != nil {
		return InvoicePage{}, err
	}
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.db.Query(ctx, invoiceSelect+` WHERE `+where+fmt.Sprintf(` ORDER BY invoices.created_at DESC, invoices.id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return InvoicePage{}, err
	}
	defer rows.Close()
	out := make([]billing.Invoice, 0)
	for rows.Next() {
		invoice, err := scanInvoice(rows)
		if err != nil {
			return InvoicePage{}, err
		}
		out = append(out, invoice)
	}
	if err := rows.Err(); err != nil {
		return InvoicePage{}, err
	}
	rows.Close()
	for index := range out {
		lines, err := s.listInvoiceLines(ctx, out[index].ID)
		if err != nil {
			return InvoicePage{}, err
		}
		out[index].Lines = lines
		document, err := s.GetInvoiceDocument(ctx, organizationID, out[index].ID)
		if err == nil {
			out[index].Document = &document.Metadata
		} else if !errors.Is(err, ErrNotFound) {
			return InvoicePage{}, err
		}
	}
	return InvoicePage{Invoices: out, Page: Page{Limit: filter.Limit, Offset: filter.Offset, Total: total}}, nil
}

const invoiceSelect = `
	SELECT invoices.id::text, invoices.invoice_number, invoices.organization_id::text, invoices.account_id::text,
	       invoices.period_id::text, invoices.pricing_version_id::text, invoices.currency, invoices.state,
	       invoices.period_start, invoices.period_end, invoices.subtotal_minor, invoices.tax_minor,
	       invoices.total_minor, invoices.amount_settled_minor, invoices.amount_due_minor,
	       invoices.recipient_snapshot, invoices.issued_at, invoices.due_at, invoices.settled_at,
	       invoices.version, invoices.created_at, invoices.updated_at,
	       COALESCE(settlement.ledger_entry_id::text, '')
	FROM billing_invoices AS invoices
	LEFT JOIN invoice_settlement_links AS settlement ON settlement.invoice_id = invoices.id`

func scanInvoice(row rowScanner) (billing.Invoice, error) {
	var out billing.Invoice
	var recipient []byte
	err := row.Scan(&out.ID, &out.InvoiceNumber, &out.OrganizationID, &out.AccountID, &out.PeriodID,
		&out.PricingVersionID, &out.Currency, &out.State, &out.PeriodStart, &out.PeriodEnd,
		&out.SubtotalMinor, &out.TaxMinor, &out.TotalMinor, &out.AmountSettledMinor, &out.AmountDueMinor,
		&recipient, &out.IssuedAt, &out.DueAt, &out.SettledAt, &out.Version, &out.CreatedAt, &out.UpdatedAt,
		&out.SettlementLedgerID)
	if err != nil {
		return billing.Invoice{}, mapNotFound(err)
	}
	if err := json.Unmarshal(recipient, &out.Recipient); err != nil {
		return billing.Invoice{}, err
	}
	return out, nil
}

func (s *Store) listInvoiceLines(ctx context.Context, invoiceID string) ([]billing.InvoiceLine, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id::text, pricing_rate_id::text, service_code, metric_code, description, quantity, quantity_scale,
		       unit, unit_price_minor, unit_price_scale, subtotal_minor, tax_minor, total_minor, rounding_mode, usage_fact_refs
		FROM billing_invoice_lines WHERE invoice_id = $1 ORDER BY service_code, metric_code, id
	`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]billing.InvoiceLine, 0)
	for rows.Next() {
		var line billing.InvoiceLine
		var refs []byte
		if err := rows.Scan(&line.ID, &line.PricingRateID, &line.ServiceCode, &line.MetricCode, &line.Description,
			&line.Quantity, &line.QuantityScale, &line.Unit, &line.UnitPriceMinor, &line.UnitPriceScale,
			&line.SubtotalMinor, &line.TaxMinor, &line.TotalMinor, &line.RoundingMode, &refs); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(refs, &line.UsageFactRefs); err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, rows.Err()
}

type InvoiceDocumentRecord struct {
	Metadata billing.InvoiceDocument
	Bytes    []byte
}

func (s *Store) PutInvoiceDocument(ctx context.Context, organizationID, invoiceID string, document billing.InvoiceDocument, data []byte) error {
	if document.ContentType != "application/pdf" || int64(len(data)) != document.ByteLength || len(document.SHA256) != 64 || document.InvoiceVersion < 1 || len(data) == 0 {
		return ErrConflict
	}
	command, err := s.db.Exec(ctx, `
		INSERT INTO billing_invoice_documents (invoice_id, content_type, document_bytes, byte_length, sha256, renderer_version, invoice_version, generated_at)
		SELECT invoices.id, $3, $4, $5, $6, $7, $8, $9 FROM billing_invoices AS invoices
		WHERE invoices.id = $2 AND invoices.organization_id = $1 AND invoices.state <> 'draft'
		ON CONFLICT (invoice_id) DO NOTHING
	`, organizationID, invoiceID, document.ContentType, data, document.ByteLength, document.SHA256, document.RendererVersion, document.InvoiceVersion, document.GeneratedAt.UTC())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		existing, err := s.GetInvoiceDocument(ctx, organizationID, invoiceID)
		if err != nil {
			return err
		}
		if existing.Metadata.SHA256 != document.SHA256 {
			return ErrInvoiceImmutable
		}
	}
	return nil
}

func (s *Store) GetInvoiceDocument(ctx context.Context, organizationID, invoiceID string) (InvoiceDocumentRecord, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, organizationID, func(view *Store) (InvoiceDocumentRecord, error) {
			return view.GetInvoiceDocument(ctx, organizationID, invoiceID)
		})
	}
	args := []any{organizationID, invoiceID}
	visibility := invoiceVisibility(ctx, &args)
	var out InvoiceDocumentRecord
	err := s.db.QueryRow(ctx, `
		SELECT documents.content_type, documents.byte_length, documents.sha256, documents.renderer_version,
		       documents.invoice_version, documents.generated_at, documents.document_bytes
		FROM billing_invoice_documents AS documents
		JOIN billing_invoices AS invoices ON invoices.id = documents.invoice_id
		WHERE invoices.organization_id = $1 AND invoices.id = $2 AND `+visibility,
		args...).Scan(&out.Metadata.ContentType, &out.Metadata.ByteLength, &out.Metadata.SHA256,
		&out.Metadata.RendererVersion, &out.Metadata.InvoiceVersion, &out.Metadata.GeneratedAt, &out.Bytes)
	return out, mapNotFound(err)
}

func (s *Store) RecordInvoiceSettlement(ctx context.Context, organizationID, invoiceID, ledgerEntryID string, now time.Time) (billing.Invoice, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Invoice{}, err
	}
	defer tx.Rollback(ctx)
	var total int64
	err = tx.QueryRow(ctx, `SELECT total_minor FROM billing_invoices WHERE id = $1 AND organization_id = $2 FOR UPDATE`, invoiceID, organizationID).Scan(&total)
	if err != nil {
		return billing.Invoice{}, mapNotFound(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO invoice_settlement_links (invoice_id, ledger_entry_id, state, attempt_count, created_at, updated_at)
		VALUES ($1, NULLIF($2, '')::uuid, 'posted', 1, $3, $3)
		ON CONFLICT (invoice_id) DO UPDATE SET
		  ledger_entry_id = COALESCE(invoice_settlement_links.ledger_entry_id, EXCLUDED.ledger_entry_id),
		  state = 'posted', updated_at = EXCLUDED.updated_at
	`, invoiceID, ledgerEntryID, now.UTC()); err != nil {
		return billing.Invoice{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE billing_invoices SET state = 'settled', amount_settled_minor = total_minor, amount_due_minor = 0,
		    settled_at = $2, version = version + 1, updated_at = $2
		WHERE id = $1 AND state IN ('issued', 'partially_settled', 'settled')
	`, invoiceID, now.UTC()); err != nil {
		return billing.Invoice{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO billing_activity_events (organization_id, customer_reference, activity_type, state,
		    amount_minor, currency, balance_effect, action, message_key, resource_type, resource_id,
		    occurred_at, updated_at)
		VALUES ($1, $2, 'invoice', 'completed', $3, 'TWD', 'debit', 'none', 'billing.invoice.settled',
		    'invoice', $4, $5, $5)
		ON CONFLICT (organization_id, resource_type, resource_id) WHERE resource_id IS NOT NULL
		DO UPDATE SET state = 'completed', amount_minor = EXCLUDED.amount_minor,
		    balance_effect = 'debit', message_key = EXCLUDED.message_key, updated_at = EXCLUDED.updated_at
	`, organizationID, "invoice-"+invoiceID, total, invoiceID, now.UTC()); err != nil {
		return billing.Invoice{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.Invoice{}, err
	}
	return s.GetInvoice(ctx, organizationID, invoiceID)
}
