package billingstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/jackc/pgx/v5"
)

// OTABridgePeriod preserves the start of the final profile-local month and
// extends or truncates its end to the first UTC month charged at OTA rates.
type OTABridgePeriod struct {
	PeriodStart    time.Time `json:"period_start"`
	LocalPeriodEnd time.Time `json:"local_period_end"`
	PeriodEnd      time.Time `json:"period_end"`
}

func otaBridgePeriod(cutover time.Time, zone string) (OTABridgePeriod, error) {
	utc := cutover.UTC()
	if cutover.IsZero() || !utc.Equal(time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)) {
		return OTABridgePeriod{}, ErrConflict
	}
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" {
		return OTABridgePeriod{}, ErrConflict
	}
	localEnd := time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, loc)
	return OTABridgePeriod{PeriodStart: localEnd.AddDate(0, -1, 0).UTC(), LocalPeriodEnd: localEnd.UTC(), PeriodEnd: utc}, nil
}

type OTACutoverBridgeReview struct {
	OrganizationID   string `json:"organization_id"`
	AccountID        string `json:"account_id"`
	PricingVersionID string `json:"pricing_version_id"`
	BaseVersionID    string `json:"base_version_id"`
	Timezone         string `json:"timezone"`
	OwnershipVersion int64  `json:"ownership_version"`
	ProfileSHA256    string `json:"profile_sha256"`
	RatesSHA256      string `json:"rates_sha256"`
	FactsSHA256      string `json:"facts_sha256"`
	FactCount        int    `json:"billable_fact_count"`
	SubtotalMinor    int64  `json:"subtotal_minor"`
	TaxMinor         int64  `json:"tax_minor"`
	TotalMinor       int64  `json:"total_minor"`
	OTABridgePeriod
	ReviewSHA256 string `json:"review_sha256"`
}

type OTACutoverBridgeInput struct {
	OrganizationID   string
	PricingVersionID string
	ReviewSHA256     string
	CreatedBy        string
	Now              time.Time
}

func bridgeDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

// ReviewOTACutoverBridge takes a consistent read-only snapshot. Preview is
// allowed before the cutover; apply never issues an invoice before its end.
func (s *Store) ReviewOTACutoverBridge(ctx context.Context, org, version string) (OTACutoverBridgeReview, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return OTACutoverBridgeReview{}, err
	}
	defer tx.Rollback(ctx)
	view := *s
	view.db = database.TransactionConnection{Tx: tx}
	review, err := view.reviewOTACutoverBridge(ctx, org, version)
	if err != nil {
		return OTACutoverBridgeReview{}, err
	}
	return review, tx.Commit(ctx)
}

func bridgeHold(reason string) error { return fmt.Errorf("%w: %s", ErrIncomplete, reason) }

func (s *Store) reviewOTACutoverBridge(ctx context.Context, org, version string) (OTACutoverBridgeReview, error) {
	out := OTACutoverBridgeReview{OrganizationID: org, PricingVersionID: version}
	if !required(org) || !required(version) {
		return out, ErrConflict
	}
	var cutover time.Time
	err := s.db.QueryRow(ctx, `SELECT p.base_version_id::text,p.effective_from
 FROM ota_pricing_publications p JOIN pricing_plan_versions v ON v.id=p.pricing_version_id
 WHERE p.pricing_version_id=$1 AND v.status IN ('active','retired')
 AND NOT EXISTS (SELECT 1 FROM ota_pricing_cancellations c WHERE c.pricing_version_id=p.pricing_version_id)`, version).Scan(&out.BaseVersionID, &cutover)
	if err != nil {
		return out, mapNotFound(err)
	}
	var state string
	if err = s.db.QueryRow(ctx, `SELECT id::text,state FROM commercial_accounts WHERE organization_id=$1 AND currency='TWD'`, org).Scan(&out.AccountID, &state); err != nil {
		return out, mapNotFound(err)
	}
	if state != "active" {
		return out, bridgeHold("account_not_active")
	}
	profile, err := s.GetBillingProfile(ctx, org)
	if errors.Is(err, ErrNotFound) {
		return out, bridgeHold("profile_missing")
	}
	if err != nil {
		return out, err
	}
	if profile.RequiresConfiguration || profile.OwnershipVersion == nil {
		return out, bridgeHold("profile_configuration_required")
	}
	out.Timezone = profile.Timezone
	out.OTABridgePeriod, err = otaBridgePeriod(cutover, profile.Timezone)
	if err != nil {
		return out, err
	}
	if err = s.db.QueryRow(ctx, `SELECT ownership_version FROM billing_responsibility_periods
 WHERE account_id=$1 AND effective_from<=$2 AND effective_until IS NULL`, out.AccountID, out.PeriodStart).Scan(&out.OwnershipVersion); errors.Is(err, pgx.ErrNoRows) {
		return out, bridgeHold("owner_period_incomplete")
	} else if err != nil {
		return out, err
	}
	if *profile.OwnershipVersion != out.OwnershipVersion {
		return out, bridgeHold("profile_owner_mismatch")
	}
	base, err := s.ActivePricingVersion(ctx, out.PeriodStart, billing.CurrencyTWD)
	if err != nil {
		return out, err
	}
	enabled, complete := billing.OTAPricingState(base.Rates)
	if base.ID != out.BaseVersionID || enabled || !complete || base.EffectiveUntil == nil || !base.EffectiveUntil.Equal(cutover) {
		return out, bridgeHold("old_rate_interval_mismatch")
	}
	target, err := s.ActivePricingVersion(ctx, cutover, billing.CurrencyTWD)
	if err != nil {
		return out, err
	}
	enabled, complete = billing.OTAPricingState(target.Rates)
	if target.ID != version || !enabled || !complete {
		return out, bridgeHold("ota_rate_interval_mismatch")
	}
	var conflicting bool
	if err = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pricing_plan_versions WHERE currency='TWD'
 AND status IN ('active','retired') AND id<>$1 AND effective_from<$3
 AND (effective_until IS NULL OR effective_until>$2))`, base.ID, out.PeriodStart, cutover).Scan(&conflicting); err != nil {
		return out, err
	}
	if conflicting {
		return out, bridgeHold("old_rate_interval_ambiguous")
	}
	if err = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM billing_periods WHERE organization_id=$1
 AND currency='TWD' AND period_start<$3 AND period_end>$2)`, org, out.PeriodStart, cutover).Scan(&conflicting); err != nil {
		return out, err
	}
	if conflicting {
		return out, bridgeHold("existing_period_requires_review")
	}
	if err = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM billing_usage_facts WHERE organization_id=$1
 AND service_code<>'ota' AND window_start<$3 AND window_end>$2
 AND (window_start<$2 OR window_end>$3))`, org, out.PeriodStart, cutover).Scan(&conflicting); err != nil {
		return out, err
	}
	if conflicting {
		return out, bridgeHold("usage_crosses_boundary")
	}
	facts, err := s.ListUsageFacts(ctx, org, out.PeriodStart, cutover)
	if err != nil {
		return out, err
	}
	facts, err = billing.BillableUsageFacts(facts, base.Rates)
	if err != nil {
		return out, err
	}
	if len(facts) == 0 {
		return out, bridgeHold("usage_missing")
	}
	draft, err := billing.BuildDraftInvoice(billing.Invoice{OrganizationID: org, AccountID: out.AccountID,
		PricingVersionID: base.ID, Currency: billing.CurrencyTWD, PeriodStart: out.PeriodStart, PeriodEnd: cutover,
		Recipient: profile, TaxMode: base.TaxMode, InvoiceTaxRateBasisPoints: base.InvoiceTaxRateBasisPoints,
		InvoiceTaxRoundingMode: base.InvoiceTaxRoundingMode, InvoiceTaxCategory: base.InvoiceTaxCategory}, facts, base.Rates)
	if err != nil {
		return out, err
	}
	out.FactCount = len(facts)
	out.SubtotalMinor = draft.SubtotalMinor
	out.TaxMinor = draft.TaxMinor
	out.TotalMinor = draft.TotalMinor
	if out.ProfileSHA256, err = bridgeDigest(profile); err != nil {
		return out, err
	}
	if out.RatesSHA256, err = bridgeDigest(base.Rates); err != nil {
		return out, err
	}
	if out.FactsSHA256, err = bridgeDigest(facts); err != nil {
		return out, err
	}
	out.ReviewSHA256, err = bridgeDigest(out)
	return out, err
}

// ApplyOTACutoverBridge rechecks the reviewed amounts while holding the same
// pricing/account barriers as normal close and usage ingestion. An immutable
// receipt makes retries return the original invoice even after profile edits.
func (s *Store) ApplyOTACutoverBridge(ctx context.Context, in OTACutoverBridgeInput) (billing.Invoice, bool, error) {
	if !required(in.OrganizationID) || !required(in.PricingVersionID) || !required(in.CreatedBy) || len(in.ReviewSHA256) != 64 {
		return billing.Invoice{}, false, ErrConflict
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Invoice{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext('billing-pricing-activation'))`); err != nil {
		return billing.Invoice{}, false, err
	}
	var account string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM commercial_accounts WHERE organization_id=$1 AND currency='TWD' FOR UPDATE`, in.OrganizationID).Scan(&account); err != nil {
		return billing.Invoice{}, false, mapNotFound(err)
	}
	view := *s
	view.db = database.TransactionConnection{Tx: tx}
	var invoiceID, digest string
	err = tx.QueryRow(ctx, `SELECT invoice_id::text,review_sha256 FROM ota_cutover_bridges WHERE organization_id=$1 AND pricing_version_id=$2`, in.OrganizationID, in.PricingVersionID).Scan(&invoiceID, &digest)
	if err == nil {
		if digest != in.ReviewSHA256 {
			return billing.Invoice{}, false, ErrConflict
		}
		invoice, e := view.GetInvoice(ctx, in.OrganizationID, invoiceID)
		if e != nil {
			return billing.Invoice{}, false, e
		}
		return invoice, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return billing.Invoice{}, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT organization_id FROM billing_profiles WHERE organization_id=$1 FOR UPDATE`, in.OrganizationID); err != nil {
		return billing.Invoice{}, false, err
	}
	review, err := view.reviewOTACutoverBridge(ctx, in.OrganizationID, in.PricingVersionID)
	if err != nil {
		return billing.Invoice{}, false, err
	}
	if review.ReviewSHA256 != in.ReviewSHA256 {
		return billing.Invoice{}, false, ErrConflict
	}
	if in.Now.Before(review.PeriodEnd) {
		return billing.Invoice{}, false, bridgeHold("cutover_not_reached")
	}
	invoice, created, err := view.prepareInvoice(ctx, PrepareInvoiceInput{OrganizationID: in.OrganizationID, AccountID: account, Currency: billing.CurrencyTWD, PeriodStart: review.PeriodStart, PeriodEnd: review.PeriodEnd, Now: in.Now})
	if err != nil {
		return billing.Invoice{}, false, err
	}
	if invoice.PricingVersionID != review.BaseVersionID || invoice.Currency != billing.CurrencyTWD ||
		invoice.AccountID != review.AccountID || !invoice.PeriodStart.Equal(review.PeriodStart) ||
		!invoice.PeriodEnd.Equal(review.PeriodEnd) || invoice.SubtotalMinor != review.SubtotalMinor ||
		invoice.TaxMinor != review.TaxMinor || invoice.TotalMinor != review.TotalMinor {
		return billing.Invoice{}, false, ErrConflict
	}
	raw, err := json.Marshal(review)
	if err != nil {
		return billing.Invoice{}, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO ota_cutover_bridges (organization_id,pricing_version_id,invoice_id,review_sha256,review,created_by,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, in.OrganizationID, in.PricingVersionID, invoice.ID, in.ReviewSHA256, raw, in.CreatedBy, in.Now.UTC()); err != nil {
		return billing.Invoice{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return billing.Invoice{}, false, err
	}
	return invoice, created, nil
}

// firstOTACutovers ignores later OTA price changes and canceled publications.
func (s *Store) firstOTACutovers(ctx context.Context) ([]time.Time, error) {
	rows, err := s.db.Query(ctx, `SELECT p.effective_from FROM ota_pricing_publications p
 JOIN pricing_plan_versions v ON v.id=p.pricing_version_id
 WHERE v.status IN ('active','retired')
 AND NOT EXISTS (SELECT 1 FROM ota_pricing_cancellations c WHERE c.pricing_version_id=p.pricing_version_id)
 AND NOT EXISTS (SELECT 1 FROM pricing_rates r WHERE r.pricing_version_id=p.base_version_id AND r.service_code='ota')
 ORDER BY p.effective_from`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at time.Time
		if err = rows.Scan(&at); err != nil {
			return nil, err
		}
		out = append(out, at.UTC())
	}
	return out, rows.Err()
}

// CurrentOTABridgePeriod lets the customer preview show the extended old
// period during the local/UTC gap, before the UTC-priced month begins.
func (s *Store) CurrentOTABridgePeriod(ctx context.Context, zone string, at time.Time) (OTABridgePeriod, bool, error) {
	cuts, err := s.firstOTACutovers(ctx)
	if err != nil {
		return OTABridgePeriod{}, false, err
	}
	for _, cut := range cuts {
		period, err := otaBridgePeriod(cut, zone)
		if err != nil {
			return OTABridgePeriod{}, false, err
		}
		if !at.Before(period.PeriodStart) && at.Before(period.PeriodEnd) {
			return period, true, nil
		}
	}
	return OTABridgePeriod{}, false, nil
}

func (s *Store) requireOTABridgeCommand(ctx context.Context, in PrepareInvoiceInput) error {
	cuts, err := s.firstOTACutovers(ctx)
	if err != nil {
		return err
	}
	if len(cuts) == 0 {
		return nil
	}
	// Preserve idempotent access to an already-issued historical invoice.
	var issued bool
	if err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_invoices WHERE organization_id=$1 AND currency=$2 AND period_start=$3 AND period_end=$4)`, in.OrganizationID, in.Currency, in.PeriodStart, in.PeriodEnd).Scan(&issued); err != nil {
		return err
	}
	if issued {
		return nil
	}
	profile, err := s.GetBillingProfile(ctx, in.OrganizationID)
	if err != nil {
		return err
	}
	for _, cut := range cuts {
		period, err := otaBridgePeriod(cut, profile.Timezone)
		if err != nil {
			return err
		}
		if in.PeriodStart.Before(period.PeriodEnd) && in.PeriodEnd.After(period.PeriodStart) {
			return bridgeHold("ota_cutover_bridge_required")
		}
	}
	return nil
}
