package billingstore

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

type ReviewedOTACancellation struct {
	BaseVersionID  string    `json:"base_version_id"`
	FirstReviewer  string    `json:"first_reviewer"`
	SecondReviewer string    `json:"second_reviewer"`
	Reason         string    `json:"reason"`
	ApprovedAt     time.Time `json:"approved_at"`
}

// CancelReviewedOTAPricingVersion restores the previous card only before the
// scheduled boundary. Both the publication and cancellation remain auditable.
func (s *Store) CancelReviewedOTAPricingVersion(ctx context.Context, id string, now time.Time, approval ReviewedOTACancellation) (billing.PricingVersion, error) {
	approval.BaseVersionID = strings.TrimSpace(approval.BaseVersionID)
	approval.FirstReviewer = strings.TrimSpace(approval.FirstReviewer)
	approval.SecondReviewer = strings.TrimSpace(approval.SecondReviewer)
	approval.Reason = strings.TrimSpace(approval.Reason)
	now = now.UTC()
	if !required(id) || !required(approval.BaseVersionID) ||
		!required(approval.FirstReviewer) || !required(approval.SecondReviewer) ||
		approval.FirstReviewer == approval.SecondReviewer || !required(approval.Reason) ||
		approval.ApprovedAt.IsZero() || approval.ApprovedAt.After(now) {
		return billing.PricingVersion{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.PricingVersion{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('billing-pricing-activation'))`); err != nil {
		return billing.PricingVersion{}, err
	}
	var baseID, status string
	var effectiveFrom time.Time
	var code billing.Currency
	if err := tx.QueryRow(ctx, `SELECT p.status, p.currency, p.effective_from,
            pub.base_version_id::text
        FROM pricing_plan_versions p
        JOIN ota_pricing_publications pub ON pub.pricing_version_id=p.id
        WHERE p.id=$1 FOR UPDATE OF p, pub`, id).Scan(&status, &code, &effectiveFrom, &baseID); err != nil {
		return billing.PricingVersion{}, mapNotFound(err)
	}
	if status != "active" || code != billing.CurrencyTWD ||
		!effectiveFrom.After(now) || baseID != approval.BaseVersionID {
		return billing.PricingVersion{}, ErrConflict
	}
	var baseStatus string
	var baseEnd *time.Time
	if err := tx.QueryRow(ctx, `SELECT status, effective_until FROM pricing_plan_versions
        WHERE id=$1 AND currency=$2 FOR UPDATE`, baseID, code).Scan(&baseStatus, &baseEnd); err != nil {
		return billing.PricingVersion{}, mapNotFound(err)
	}
	if baseStatus != "retired" || baseEnd == nil || !baseEnd.Equal(effectiveFrom) {
		return billing.PricingVersion{}, ErrConflict
	}
	var conflicting bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
            SELECT 1 FROM pricing_plan_versions
            WHERE currency=$1 AND id<>$2 AND id<>$3 AND status IN ('active','retired')
                AND effective_from>$4
        ) OR EXISTS(
            SELECT 1 FROM billing_periods
            WHERE currency=$1 AND period_end>$4
        ) OR EXISTS(
            SELECT 1 FROM billing_invoices WHERE pricing_version_id=$2
        )`, code, id, baseID, effectiveFrom).Scan(&conflicting); err != nil {
		return billing.PricingVersion{}, err
	}
	if conflicting {
		return billing.PricingVersion{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ota_pricing_cancellations
        (pricing_version_id,base_version_id,first_reviewer,second_reviewer,reason,approved_at,canceled_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, baseID, approval.FirstReviewer, approval.SecondReviewer, approval.Reason,
		approval.ApprovedAt.UTC(), now); err != nil {
		return billing.PricingVersion{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE pricing_plan_versions SET status='canceled'
        WHERE id=$1 AND status='active'`, id); err != nil {
		return billing.PricingVersion{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE pricing_plan_versions SET status='active',
        effective_until=NULL WHERE id=$1 AND status='retired'`, baseID); err != nil {
		return billing.PricingVersion{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.PricingVersion{}, err
	}
	return s.GetPricingVersion(ctx, id)
}
