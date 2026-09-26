package billingstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/database"
)

type ReviewOTACandidateInput struct {
	BaseVersionID string
	EffectiveFrom time.Time
	Rates         []billing.PricingRate
}

type OTACandidateSnapshot struct {
	ObservedAt    time.Time              `json:"observed_at"`
	EffectiveFrom time.Time              `json:"effective_from"`
	Base          billing.PricingVersion `json:"base"`
	Review        billing.OTACardReview  `json:"review"`
}

// ReviewOTACandidate is read-only and uses one repeatable-read snapshot. Its
// result is a diff/digest for human review, not an approval or a draft write.
func (s *Store) ReviewOTACandidate(ctx context.Context, in ReviewOTACandidateInput) (OTACandidateSnapshot, error) {
	if !required(in.BaseVersionID) || in.EffectiveFrom.IsZero() || len(in.Rates) == 0 {
		return OTACandidateSnapshot{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return OTACandidateSnapshot{}, err
	}
	defer tx.Rollback(ctx)
	view := *s
	view.db = database.TransactionConnection{Tx: tx}
	var observedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&observedAt); err != nil {
		return OTACandidateSnapshot{}, err
	}
	cutover := in.EffectiveFrom.UTC()
	monthStart := time.Date(cutover.Year(), cutover.Month(), 1, 0, 0, 0, 0, time.UTC)
	if !cutover.After(observedAt.UTC()) || !cutover.Equal(monthStart) {
		return OTACandidateSnapshot{}, ErrConflict
	}
	current, err := view.ActivePricingVersion(ctx, observedAt, billing.CurrencyTWD)
	if err != nil {
		return OTACandidateSnapshot{}, err
	}
	if current.ID != in.BaseVersionID {
		return OTACandidateSnapshot{}, ErrConflict
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pricing_plan_versions
		WHERE currency='TWD' AND status IN ('active','retired') AND effective_from>$1)`, observedAt.UTC()).Scan(&pending); err != nil {
		return OTACandidateSnapshot{}, err
	}
	if pending {
		return OTACandidateSnapshot{}, ErrConflict
	}
	review, err := billing.ReviewOTACandidateRates(current.ID, current.Rates, in.Rates)
	if err != nil {
		return OTACandidateSnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OTACandidateSnapshot{}, err
	}
	return OTACandidateSnapshot{ObservedAt: observedAt.UTC(), EffectiveFrom: cutover, Base: current, Review: review}, nil
}
