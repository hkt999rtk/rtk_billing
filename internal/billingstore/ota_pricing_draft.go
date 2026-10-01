package billingstore

import (
	"context"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/jackc/pgx/v5"
)

type CreateOTAPricingDraftInput struct {
	BaseVersionID           string
	EffectiveFrom           time.Time
	CreatedBy               string
	CandidateRates          []billing.PricingRate
	ReviewedRateSetSHA256   string
	LoggerApprovalReference string
	LoggerRateSetSHA256     string
}

type OTAPricingDraft struct {
	PricingVersion          billing.PricingVersion `json:"pricing_version"`
	BaseVersionID           string                 `json:"base_version_id"`
	RateSetSHA256           string                 `json:"rate_set_sha256"`
	ScopeCode               string                 `json:"scope_code"`
	LoggerApprovalReference string                 `json:"logger_approval_reference,omitempty"`
	LoggerRateSetSHA256     string                 `json:"logger_rate_set_sha256,omitempty"`
}

// CreateOTAPricingDraft copies the current complete TWD card, adds only the
// four approved OTA rates and, with explicit approval, the fixed Logger pair.
// It records the full-card and optional Logger manifests atomically.
// Publication remains a separate two-reviewer operation.
func (s *Store) CreateOTAPricingDraft(ctx context.Context, in CreateOTAPricingDraftInput) (OTAPricingDraft, error) {
	if !required(in.BaseVersionID) || !required(in.CreatedBy) || in.EffectiveFrom.IsZero() {
		return OTAPricingDraft{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return OTAPricingDraft{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('billing-pricing-activation'))`); err != nil {
		return OTAPricingDraft{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return OTAPricingDraft{}, err
	}
	cutover := in.EffectiveFrom.UTC()
	monthStart := time.Date(cutover.Year(), cutover.Month(), 1, 0, 0, 0, 0, time.UTC)
	if !cutover.Equal(monthStart) || !cutover.After(now.UTC()) {
		return OTAPricingDraft{}, ErrConflict
	}
	view := *s
	view.db = database.TransactionConnection{Tx: tx}
	base, err := view.ActivePricingVersion(ctx, now.UTC(), billing.CurrencyTWD)
	if err != nil || base.ID != in.BaseVersionID || base.Version == int64(^uint64(0)>>1) {
		return OTAPricingDraft{}, ErrConflict
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pricing_plan_versions
		WHERE currency='TWD' AND status IN ('active','retired') AND effective_from>$1)`, now.UTC()).Scan(&pending); err != nil {
		return OTAPricingDraft{}, err
	}
	if pending {
		return OTAPricingDraft{}, ErrConflict
	}
	rates := append([]billing.PricingRate(nil), base.Rates...)
	standard := "standard"
	if len(in.CandidateRates) > 0 {
		if strings.TrimSpace(in.ReviewedRateSetSHA256) == "" {
			return OTAPricingDraft{}, ErrConflict
		}
		rates = append([]billing.PricingRate(nil), in.CandidateRates...)
	} else {
		if in.ReviewedRateSetSHA256 != "" || in.LoggerApprovalReference != "" || in.LoggerRateSetSHA256 != "" {
			return OTAPricingDraft{}, ErrConflict
		}
		for _, rate := range billing.ProposedOTARates() {
			rate.TaxCategory = &standard
			rates = append(rates, rate)
		}
	}
	for _, rate := range rates {
		if rate.TaxCategory == nil || *rate.TaxCategory != standard {
			return OTAPricingDraft{}, ErrConflict
		}
	}
	review, err := billing.ReviewOTACandidateRatesWithLogger(base.ID, base.Rates, rates, in.LoggerApprovalReference)
	if err != nil || (in.ReviewedRateSetSHA256 != "" && review.RateSetSHA256 != in.ReviewedRateSetSHA256) ||
		review.LoggerRateSetSHA256 != in.LoggerRateSetSHA256 {
		return OTAPricingDraft{}, ErrConflict
	}
	taxRate := int64(500)
	version, err := view.CreatePricingVersion(ctx, CreatePricingVersionInput{
		PlanKey: base.PlanKey, Version: base.Version + 1, Currency: billing.CurrencyTWD,
		EffectiveFrom: cutover, Rates: rates, TaxMode: billing.TaxModeInvoiceTotal,
		InvoiceTaxRateBasisPoints: &taxRate, InvoiceTaxRoundingMode: billing.RoundingHalfUp,
		InvoiceTaxCategory: standard, CreatedBy: in.CreatedBy, Now: now.UTC(),
	})
	if err != nil {
		return OTAPricingDraft{}, err
	}
	const scope = "commercial_active_product_ota"
	if _, err := tx.Exec(ctx, `INSERT INTO ota_pricing_drafts
		(pricing_version_id, base_version_id, rate_set_sha256, scope_code, effective_from,
		 created_by, created_at, tax_mode, tax_rate_basis_points, tax_rounding_mode, tax_category,
		 logger_approval_reference, logger_rate_set_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'invoice_total',500,'half_up','standard',NULLIF($8,''),NULLIF($9,''))`,
		version.ID, base.ID, review.RateSetSHA256, scope, cutover, in.CreatedBy, now.UTC(),
		review.LoggerApprovalReference, review.LoggerRateSetSHA256); err != nil {
		return OTAPricingDraft{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OTAPricingDraft{}, err
	}
	return OTAPricingDraft{PricingVersion: version, BaseVersionID: base.ID,
		RateSetSHA256: review.RateSetSHA256, ScopeCode: scope,
		LoggerApprovalReference: review.LoggerApprovalReference, LoggerRateSetSHA256: review.LoggerRateSetSHA256}, nil
}
