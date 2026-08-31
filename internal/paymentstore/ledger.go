package paymentstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

type PostLedgerEntryInput struct {
	AccountID        string
	Direction        payment.LedgerDirection
	AmountMinor      int64
	Currency         payment.Currency
	Reason           payment.LedgerReason
	IdempotencyScope string
	IdempotencyKey   string
	ExternalType     string
	ExternalID       string
	ActorType        string
	ActorID          string
	RequestID        string
	Now              time.Time
}

type PostLedgerEntryResult struct {
	Account   payment.CommercialAccount `json:"account"`
	Entry     payment.LedgerEntry       `json:"entry"`
	Intent    *payment.PaymentIntent    `json:"intent,omitempty"`
	Duplicate bool                      `json:"duplicate"`
}

func (s *Store) PostLedgerEntry(ctx context.Context, in PostLedgerEntryInput) (PostLedgerEntryResult, error) {
	if err := validateLedgerInput(in); err != nil {
		return PostLedgerEntryResult{}, err
	}
	if isSettlementReversal(in.Reason) {
		return PostLedgerEntryResult{}, ErrProviderReversalRequired
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	} else {
		in.Now = in.Now.UTC()
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return PostLedgerEntryResult{}, err
	}
	defer tx.Rollback(ctx)

	account, err := getAccountForUpdate(ctx, tx, in.AccountID)
	if err != nil {
		return PostLedgerEntryResult{}, err
	}
	if account.State == payment.AccountStateClosed {
		return PostLedgerEntryResult{}, ErrAccountClosed
	}
	if account.Currency != in.Currency {
		return PostLedgerEntryResult{}, ErrConflict
	}

	existing, err := getLedgerEntryByIdempotency(ctx, tx, in.AccountID, in.IdempotencyScope, in.IdempotencyKey)
	if err == nil {
		if !sameLedgerRequest(existing, in) {
			return PostLedgerEntryResult{}, ErrIdempotencyConflict
		}
		intent, intentErr := getIntentByTriggerLedgerEntry(ctx, tx, existing.ID)
		if intentErr != nil && !errors.Is(intentErr, ErrNotFound) {
			return PostLedgerEntryResult{}, intentErr
		}
		var intentPtr *payment.PaymentIntent
		if intentErr == nil {
			intentPtr = &intent
		}
		return PostLedgerEntryResult{Account: account, Entry: existing, Intent: intentPtr, Duplicate: true}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return PostLedgerEntryResult{}, err
	}
	// Provider reconciliation credits use transitionIntentTx. Manual credit is
	// not an escape hatch for topping up during handoff preparation.
	if in.Direction == payment.LedgerDirectionCredit {
		if err := requireNoHandoffTx(ctx, tx, account.ID); err != nil {
			return PostLedgerEntryResult{}, err
		}
	}

	entry, account, err := insertLedgerEntryTx(ctx, tx, account, in)
	if err != nil {
		return PostLedgerEntryResult{}, err
	}

	var intent *payment.PaymentIntent
	if in.Direction == payment.LedgerDirectionDebit && account.State == payment.AccountStateActive {
		created, evalErr := evaluateAutoTopUpTx(ctx, tx, account, entry, in.Now, in.RequestID)
		if evalErr != nil {
			return PostLedgerEntryResult{}, evalErr
		}
		intent = created
	} else if in.Direction == payment.LedgerDirectionCredit {
		account, err = rearmPolicyAfterCreditTx(ctx, tx, account)
		if err != nil {
			return PostLedgerEntryResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return PostLedgerEntryResult{}, err
	}
	return PostLedgerEntryResult{Account: account, Entry: entry, Intent: intent}, nil
}

func isSettlementReversal(reason payment.LedgerReason) bool {
	return reason == payment.LedgerReasonRefundDebit || reason == payment.LedgerReasonChargebackDebit
}

func disarmAutoTopUpAfterSettlementReversalTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount) (payment.CommercialAccount, error) {
	command, err := tx.Exec(ctx, `
		UPDATE auto_topup_policies
		SET armed = false,
			version = version + 1
		WHERE account_id = $1 AND armed = true
	`, account.ID)
	if err != nil {
		return payment.CommercialAccount{}, err
	}
	if command.RowsAffected() == 0 || account.State != payment.AccountStateActive {
		return account, nil
	}
	return scanAccount(tx.QueryRow(ctx, `
		UPDATE commercial_accounts
		SET state = 'attention_required'
		WHERE id = $1
		RETURNING `+accountColumns,
		account.ID,
	))
}

func validateLedgerInput(in PostLedgerEntryInput) error {
	if !required(in.AccountID) || !required(in.IdempotencyScope) || !required(in.IdempotencyKey) {
		return ErrConflict
	}
	if (required(in.ExternalType) && !required(in.ExternalID)) || (!required(in.ExternalType) && required(in.ExternalID)) {
		return ErrConflict
	}
	if (required(in.ActorType) && !required(in.ActorID)) || (!required(in.ActorType) && required(in.ActorID)) {
		return ErrConflict
	}
	if err := payment.ValidateChargeAmount(in.Currency, in.AmountMinor); err != nil {
		return err
	}
	return payment.ValidateLedgerReason(in.Direction, in.Reason)
}

func insertLedgerEntryTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount, in PostLedgerEntryInput) (payment.LedgerEntry, payment.CommercialAccount, error) {
	nextBalance, err := payment.ApplyBalance(account.AvailableBalanceMinor, in.Direction, in.AmountMinor)
	if err != nil {
		return payment.LedgerEntry{}, payment.CommercialAccount{}, err
	}

	entry, err := scanLedgerEntry(tx.QueryRow(ctx, `
		INSERT INTO balance_ledger_entries (
			account_id, direction, amount_minor, currency, reason,
			idempotency_scope, idempotency_key, external_type, external_id,
			balance_after_minor, actor_type, actor_id, request_id, created_at
		)
		VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			NULLIF($8, ''), NULLIF($9, ''), $10,
			NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, ''), $14
		)
		RETURNING `+ledgerColumns,
		account.ID, in.Direction, in.AmountMinor, in.Currency, in.Reason,
		strings.TrimSpace(in.IdempotencyScope), strings.TrimSpace(in.IdempotencyKey),
		strings.TrimSpace(in.ExternalType), strings.TrimSpace(in.ExternalID), nextBalance,
		strings.TrimSpace(in.ActorType), strings.TrimSpace(in.ActorID), strings.TrimSpace(in.RequestID), in.Now,
	))
	if err != nil {
		return payment.LedgerEntry{}, payment.CommercialAccount{}, err
	}

	account, err = scanAccount(tx.QueryRow(ctx, `
		UPDATE commercial_accounts
		SET available_balance_minor = $2,
			version = version + 1
		WHERE id = $1
		RETURNING `+accountColumns,
		account.ID, nextBalance,
	))
	if err != nil {
		return payment.LedgerEntry{}, payment.CommercialAccount{}, err
	}
	return entry, account, nil
}

func getLedgerEntryByIdempotency(ctx context.Context, tx pgx.Tx, accountID, scope, key string) (payment.LedgerEntry, error) {
	return scanLedgerEntry(tx.QueryRow(ctx, `
		SELECT `+ledgerColumns+`
		FROM balance_ledger_entries
		WHERE account_id = $1 AND idempotency_scope = $2 AND idempotency_key = $3
	`, accountID, strings.TrimSpace(scope), strings.TrimSpace(key)))
}

func sameLedgerRequest(existing payment.LedgerEntry, in PostLedgerEntryInput) bool {
	return existing.AccountID == in.AccountID && existing.Direction == in.Direction &&
		existing.AmountMinor == in.AmountMinor && existing.Currency == in.Currency &&
		existing.Reason == in.Reason && existing.IdempotencyScope == strings.TrimSpace(in.IdempotencyScope) &&
		existing.IdempotencyKey == strings.TrimSpace(in.IdempotencyKey) &&
		existing.ExternalType == strings.TrimSpace(in.ExternalType) && existing.ExternalID == strings.TrimSpace(in.ExternalID) &&
		existing.ActorType == strings.TrimSpace(in.ActorType) && existing.ActorID == strings.TrimSpace(in.ActorID)
}

func evaluateAutoTopUpTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount, trigger payment.LedgerEntry, now time.Time, correlationID string) (*payment.PaymentIntent, error) {
	if err := requireNoHandoffTx(ctx, tx, account.ID); err != nil {
		if errors.Is(err, ErrHandoffFenced) {
			return nil, nil
		}
		return nil, err
	}
	policy, err := getPolicyForUpdate(ctx, tx, account.ID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := payment.ValidatePolicy(policy); err != nil {
		return nil, err
	}

	method, err := getPaymentMethodForUpdate(ctx, tx, policy.PaymentMethodID)
	if err != nil {
		return nil, err
	}

	var hasOpen bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM payment_intents
			WHERE account_id = $1
			  AND state IN ('created', 'processing', 'authorized', 'requires_action', 'unknown')
		)
	`, account.ID).Scan(&hasOpen); err != nil {
		return nil, err
	}

	var attemptsToday int
	var amountToday int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*)::int,
		       COALESCE(sum(amount_minor) FILTER (WHERE state NOT IN ('failed', 'canceled')), 0)::bigint
		FROM payment_intents
		WHERE account_id = $1
		  AND reason = 'auto_top_up'
		  AND created_at >= $2
	`, account.ID, policyDayStart(now)).Scan(&attemptsToday, &amountToday); err != nil {
		return nil, err
	}

	decision := payment.EvaluateAutoTopUp(policy, payment.PolicyEvaluation{
		BalanceMinor:              account.AvailableBalanceMinor,
		Now:                       now,
		AttemptsToday:             attemptsToday,
		AutomaticAmountTodayMinor: amountToday,
		HasOpenIntent:             hasOpen,
		PaymentMethodStatus:       method.Status,
		MerchantInitiatedCharge:   method.Capabilities.MerchantInitiatedCharge,
	})
	if !decision.Trigger {
		return nil, nil
	}

	if !required(correlationID) {
		correlationID = trigger.ID
	}
	idempotencyKey := fmt.Sprintf("auto/%s/%d/%s", policy.ID, policy.Generation, trigger.ID)
	intent, err := scanIntent(tx.QueryRow(ctx, `
		INSERT INTO payment_intents (
			account_id, amount_minor, currency, reason, policy_generation,
			trigger_ledger_entry_id, provider, payment_method_id, state,
			idempotency_key, correlation_id, created_at, updated_at
		)
		VALUES ($1, $2, $3, 'auto_top_up', $4, $5, $6, $7, 'created', $8, $9, $10, $10)
		RETURNING `+intentColumns,
		account.ID, policy.TopUpAmountMinor, policy.Currency, policy.Generation,
		trigger.ID, method.Provider, method.ID, idempotencyKey, correlationID, now,
	))
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE auto_topup_policies
		SET armed = false,
			last_triggered_at = $2,
			version = version + 1
		WHERE id = $1
	`, policy.ID, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO payment_reconciliation_jobs (intent_id, reason, status, due_at)
		VALUES ($1, 'charge', 'pending', $2)
	`, intent.ID, now); err != nil {
		return nil, err
	}
	return &intent, nil
}

func rearmPolicyAfterCreditTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount) (payment.CommercialAccount, error) {
	if err := requireNoHandoffTx(ctx, tx, account.ID); err != nil {
		if errors.Is(err, ErrHandoffFenced) {
			return account, nil
		}
		return payment.CommercialAccount{}, err
	}
	policy, err := getPolicyForUpdate(ctx, tx, account.ID)
	if errors.Is(err, ErrNotFound) {
		return account, nil
	}
	if err != nil {
		return payment.CommercialAccount{}, err
	}
	if policy.Enabled && account.AvailableBalanceMinor >= policy.ThresholdMinor {
		if _, err := tx.Exec(ctx, `
			UPDATE auto_topup_policies
			SET armed = true,
				version = CASE WHEN armed THEN version ELSE version + 1 END
			WHERE id = $1
		`, policy.ID); err != nil {
			return payment.CommercialAccount{}, err
		}
		if account.State == payment.AccountStateAttentionRequired {
			account, err = scanAccount(tx.QueryRow(ctx, `
				UPDATE commercial_accounts
				SET state = 'active'
				WHERE id = $1
				RETURNING `+accountColumns,
				account.ID,
			))
			if err != nil {
				return payment.CommercialAccount{}, err
			}
		}
	}
	return account, nil
}

func (s *Store) ListLedgerEntries(ctx context.Context, accountID string, limit int) ([]payment.LedgerEntry, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, accountID, func(view *Store) ([]payment.LedgerEntry, error) { return view.ListLedgerEntries(ctx, accountID, limit) })
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	args := []any{accountID, limit}
	visibility := ledgerVisibility(ctx, &args)
	rows, err := s.db.Query(ctx, `
		SELECT `+ledgerColumns+`
		FROM balance_ledger_entries
		WHERE account_id = $1 AND `+visibility+`
		ORDER BY created_at, id
		LIMIT $2
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []payment.LedgerEntry
	for rows.Next() {
		entry, err := scanLedgerEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

type LedgerEntryPage struct {
	Entries []payment.LedgerEntry `json:"ledger_entries"`
	Total   int                   `json:"total"`
}

func (s *Store) ListLedgerEntriesPage(ctx context.Context, accountID string, limit, offset int) (LedgerEntryPage, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, accountID, func(view *Store) (LedgerEntryPage, error) {
			return view.ListLedgerEntriesPage(ctx, accountID, limit, offset)
		})
	}
	if !required(accountID) {
		return LedgerEntryPage{}, ErrConflict
	}
	limit, offset = boundedPage(limit, offset)
	args := []any{accountID}
	visibility := ledgerVisibility(ctx, &args)
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM balance_ledger_entries WHERE account_id = $1 AND `+visibility, args...).Scan(&total); err != nil {
		return LedgerEntryPage{}, err
	}
	args = append(args, limit, offset)
	rows, err := s.db.Query(ctx, `
		SELECT `+ledgerColumns+`
		FROM balance_ledger_entries
		WHERE account_id = $1 AND `+visibility+`
		ORDER BY created_at DESC, id DESC
		`+fmt.Sprintf("LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return LedgerEntryPage{}, err
	}
	defer rows.Close()
	entries := make([]payment.LedgerEntry, 0)
	for rows.Next() {
		entry, scanErr := scanLedgerEntry(rows)
		if scanErr != nil {
			return LedgerEntryPage{}, scanErr
		}
		entries = append(entries, entry)
	}
	return LedgerEntryPage{Entries: entries, Total: total}, rows.Err()
}
