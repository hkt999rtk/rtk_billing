package paymentstore

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
func tenantRead[T any](ctx context.Context, s *Store, accountID string, read func(*Store) (T, error)) (T, error) {
	var zero T
	scope, ok := billingidentity.FromContext(ctx)
	if !ok || scope.AccountID != accountID {
		return zero, billingidentity.ErrDenied
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	if err := billingidentity.LockAccount(ctx, tx, accountID); err != nil {
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

// All identifiers below are compile-time query fragments, never client input.
func periodVisibility(ctx context.Context, args *[]any, binding, recordID string, currentVersion bool) string {
	scope, ok := billingidentity.FromContext(ctx)
	if !ok {
		return "true"
	}
	*args = append(*args, scope.UserID)
	condition := fmt.Sprintf(`EXISTS(SELECT 1 FROM %s privacy_binding JOIN billing_responsibility_periods privacy_period ON privacy_period.id=privacy_binding.period_id
		WHERE %s AND privacy_period.owner_user_id=$%d`, binding, recordID, len(*args))
	if currentVersion {
		*args = append(*args, scope.OwnershipVersion)
		condition += fmt.Sprintf(` AND privacy_period.ownership_version=$%d`, len(*args))
	}
	return condition + ")"
}
func methodVisibility(ctx context.Context, args *[]any, current bool) string {
	return periodVisibility(ctx, args, "billing_payment_method_responsibility", "privacy_binding.method_id=payment_methods.id AND privacy_binding.account_id=payment_methods.account_id", current)
}
func intentVisibility(ctx context.Context, args *[]any) string {
	return periodVisibility(ctx, args, "billing_payment_responsibility", "privacy_binding.intent_id=payment_intents.id AND privacy_binding.account_id=payment_intents.account_id", false)
}

// A retry is an action in its initiating ownership version, not a history read.
// Account-global legacy keys must never return a predecessor/previous-tenure intent.
func requireCurrentIntentReplay(ctx context.Context, tx pgx.Tx, intentID string) error {
	if _, scoped := billingidentity.FromContext(ctx); !scoped {
		return nil
	}
	args := []any{intentID}
	visibility := periodVisibility(ctx, &args, "billing_payment_responsibility", "privacy_binding.intent_id=payment_intents.id AND privacy_binding.account_id=payment_intents.account_id", true)
	var visible bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_intents WHERE id=$1 AND `+visibility+`)`, args...).Scan(&visible); err != nil {
		return err
	}
	if !visible {
		return ErrIdempotencyConflict
	}
	return nil
}
func ledgerVisibility(ctx context.Context, args *[]any) string {
	return periodVisibility(ctx, args, "billing_ledger_responsibility", `privacy_binding.entry_id=balance_ledger_entries.id AND privacy_binding.account_id=balance_ledger_entries.account_id
		AND (balance_ledger_entries.external_type IS DISTINCT FROM 'invoice' OR EXISTS(SELECT 1 FROM billing_invoices invoice
		WHERE invoice.id::text=balance_ledger_entries.external_id AND invoice.account_id=balance_ledger_entries.account_id
		AND invoice.recipient_snapshot->>'ownership_version'=privacy_period.ownership_version::text
		AND invoice.period_start>=privacy_period.effective_from AND invoice.period_end<=COALESCE(privacy_period.effective_until,'infinity'::timestamptz)))`, false)
}
