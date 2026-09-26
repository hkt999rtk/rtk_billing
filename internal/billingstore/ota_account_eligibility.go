package billingstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// VerifyOTAAccountEligibility requires an active Billing account at settlement
// and Account Manager's commercial tier for the entire UTC usage month.
func (s *Store) VerifyOTAAccountEligibility(ctx context.Context, cloudID, accountID string, start, end time.Time) error {
	start, end = start.UTC(), end.UTC()
	if !required(cloudID) || !otaUTCMonth(start, end) || s.otaTierVerifier == nil {
		return ErrIncomplete
	}
	var state string
	var err error
	if accountID == "" {
		err = s.db.QueryRow(ctx, `SELECT state FROM commercial_accounts
			WHERE organization_id=$1 AND currency='TWD'`, cloudID).Scan(&state)
	} else {
		err = s.db.QueryRow(ctx, `SELECT state FROM commercial_accounts
			WHERE id=$1 AND organization_id=$2 AND currency='TWD'`, accountID, cloudID).Scan(&state)
	}
	if errors.Is(err, pgx.ErrNoRows) || state != "active" && err == nil {
		return ErrIncomplete
	}
	if err != nil {
		return err
	}
	if err := s.otaTierVerifier.VerifyCommercialMonth(ctx, cloudID, start, end); err != nil {
		return fmt.Errorf("%w: commercial tier history", ErrIncomplete)
	}
	return nil
}
