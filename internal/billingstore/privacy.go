package billingstore

import (
	"context"
	"fmt"

	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/jackc/pgx/v5"
)

func needsTenantRead(ctx context.Context, s *Store) bool {
	_, ok := billingidentity.FromContext(ctx)
	return ok && !s.tenantRead
}

func tenantRead[T any](ctx context.Context, s *Store, organizationID string, read func(*Store) (T, error)) (T, error) {
	var zero T
	scope, ok := billingidentity.FromContext(ctx)
	if !ok || scope.OrganizationID != organizationID {
		return zero, billingidentity.ErrDenied
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	if err := billingidentity.LockAccount(ctx, tx, scope.AccountID); err != nil {
		return zero, err
	}
	view := &Store{db: database.TransactionConnection{Tx: tx}, tenantRead: true}
	result, err := read(view)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, err
	}
	return result, nil
}

// Whole invoices must belong to one proven responsibility period. A recipient
// version alone is insufficient for mixed-period invoices, including return owners.
func invoiceVisibility(ctx context.Context, args *[]any) string {
	scope, ok := billingidentity.FromContext(ctx)
	if !ok {
		return "true"
	}
	*args = append(*args, scope.UserID, scope.AccountID)
	return fmt.Sprintf(`EXISTS(SELECT 1 FROM billing_responsibility_periods privacy_period
		WHERE privacy_period.account_id=invoices.account_id AND privacy_period.owner_user_id=$%d AND privacy_period.account_id=$%d
		AND invoices.recipient_snapshot->>'ownership_version'=privacy_period.ownership_version::text
		AND invoices.period_start>=privacy_period.effective_from
		AND invoices.period_end<=COALESCE(privacy_period.effective_until,'infinity'::timestamptz))`, len(*args)-1, len(*args))
}

func paymentVisibility(ctx context.Context, args *[]any) string {
	scope, ok := billingidentity.FromContext(ctx)
	if !ok {
		return "true"
	}
	*args = append(*args, scope.UserID)
	return fmt.Sprintf(`EXISTS(SELECT 1 FROM billing_payment_responsibility privacy_binding
		JOIN billing_responsibility_periods privacy_period ON privacy_period.id=privacy_binding.period_id
		WHERE privacy_binding.intent_id=intents.id AND privacy_binding.account_id=intents.account_id
		AND privacy_period.owner_user_id=$%d)`, len(*args))
}

func usageVisibility(ctx context.Context, args *[]any) string {
	scope, ok := billingidentity.FromContext(ctx)
	if !ok {
		return "true"
	}
	*args = append(*args, scope.AccountID, scope.UserID)
	return fmt.Sprintf(`EXISTS(SELECT 1 FROM billing_responsibility_periods privacy_period
		WHERE privacy_period.account_id=$%d AND privacy_period.owner_user_id=$%d
		AND billing_usage_facts.window_start>=privacy_period.effective_from
		AND billing_usage_facts.window_end<=COALESCE(privacy_period.effective_until,'infinity'::timestamptz))`, len(*args)-1, len(*args))
}
