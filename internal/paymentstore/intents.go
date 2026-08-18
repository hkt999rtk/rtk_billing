package paymentstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

type TransitionIntentInput struct {
	IntentID                     string
	ToState                      payment.PaymentIntentState
	ProviderTransactionReference string
	Now                          time.Time
}

type TransitionIntentResult struct {
	Intent      payment.PaymentIntent     `json:"intent"`
	Account     payment.CommercialAccount `json:"account"`
	CreditEntry *payment.LedgerEntry      `json:"credit_entry,omitempty"`
	Duplicate   bool                      `json:"duplicate"`
}

func (s *Store) TransitionIntent(ctx context.Context, in TransitionIntentInput) (TransitionIntentResult, error) {
	if !required(in.IntentID) || !payment.ValidIntentState(in.ToState) {
		return TransitionIntentResult{}, ErrConflict
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	} else {
		in.Now = in.Now.UTC()
	}

	var accountID string
	if err := s.db.QueryRow(ctx, `SELECT account_id::text FROM payment_intents WHERE id = $1`, in.IntentID).Scan(&accountID); err != nil {
		return TransitionIntentResult{}, mapNotFound(err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return TransitionIntentResult{}, err
	}
	defer tx.Rollback(ctx)

	account, err := getAccountForUpdate(ctx, tx, accountID)
	if err != nil {
		return TransitionIntentResult{}, err
	}
	intent, err := getIntentForUpdate(ctx, tx, in.IntentID)
	if err != nil {
		return TransitionIntentResult{}, err
	}
	result, err := transitionIntentTx(ctx, tx, account, intent, in)
	if err != nil {
		return TransitionIntentResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TransitionIntentResult{}, err
	}
	return result, nil
}

func transitionIntentTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount, intent payment.PaymentIntent, in TransitionIntentInput) (TransitionIntentResult, error) {
	var err error
	providerReference := strings.TrimSpace(in.ProviderTransactionReference)
	if intent.ProviderTransactionReference != "" && providerReference != "" && intent.ProviderTransactionReference != providerReference {
		return TransitionIntentResult{}, ErrConflict
	}

	if intent.State == in.ToState {
		credit, creditErr := getPaymentIntentCredit(ctx, tx, intent.ID)
		if creditErr != nil && !errors.Is(creditErr, ErrNotFound) {
			return TransitionIntentResult{}, creditErr
		}
		var creditPtr *payment.LedgerEntry
		if creditErr == nil {
			creditPtr = &credit
		}
		return TransitionIntentResult{Intent: intent, Account: account, CreditEntry: creditPtr, Duplicate: true}, nil
	}
	if err := payment.ValidateIntentTransition(intent.State, in.ToState); err != nil {
		return TransitionIntentResult{}, err
	}
	if intent.ProviderTransactionReference == "" {
		intent.ProviderTransactionReference = providerReference
	}

	completedAt := (*time.Time)(nil)
	if payment.IntentStateTerminal(in.ToState) {
		completedAt = &in.Now
	}
	intent, err = scanIntent(tx.QueryRow(ctx, `
		UPDATE payment_intents
		SET state = $2,
			provider_transaction_reference = NULLIF($3, ''),
			completed_at = $4,
			updated_at = $5
		WHERE id = $1
		RETURNING `+intentColumns,
		intent.ID, in.ToState, intent.ProviderTransactionReference, completedAt, in.Now,
	))
	if err != nil {
		return TransitionIntentResult{}, err
	}

	var creditPtr *payment.LedgerEntry
	if in.ToState == payment.PaymentIntentStateSucceeded {
		creditInput := PostLedgerEntryInput{
			AccountID:        account.ID,
			Direction:        payment.LedgerDirectionCredit,
			AmountMinor:      intent.AmountMinor,
			Currency:         intent.Currency,
			Reason:           payment.LedgerReasonPaymentTopUpCredit,
			IdempotencyScope: "payment_intent",
			IdempotencyKey:   intent.ID,
			ExternalType:     "payment_intent",
			ExternalID:       intent.ID,
			ActorType:        "service",
			ActorID:          "payment_worker",
			RequestID:        intent.CorrelationID,
			Now:              in.Now,
		}
		credit, updatedAccount, creditErr := insertLedgerEntryTx(ctx, tx, account, creditInput)
		if creditErr != nil {
			return TransitionIntentResult{}, creditErr
		}
		account = updatedAccount
		creditPtr = &credit

		policy, policyErr := getPolicyForUpdate(ctx, tx, account.ID)
		if policyErr != nil && !errors.Is(policyErr, ErrNotFound) {
			return TransitionIntentResult{}, policyErr
		}
		if policyErr == nil {
			armed := account.AvailableBalanceMinor >= policy.ThresholdMinor
			resetFailures := intent.Reason == payment.PaymentIntentReasonAutoTopUp &&
				intent.PolicyGeneration != nil && *intent.PolicyGeneration == policy.Generation
			if _, err := tx.Exec(ctx, `
				UPDATE auto_topup_policies
				SET last_succeeded_at = $2,
					armed = $3,
					consecutive_failure_count = CASE WHEN $4 THEN 0 ELSE consecutive_failure_count END,
					version = version + 1
				WHERE id = $1
			`, policy.ID, in.Now, armed, resetFailures); err != nil {
				return TransitionIntentResult{}, err
			}
			if account.State != payment.AccountStateSuspended && account.State != payment.AccountStateClosed {
				state := payment.AccountStateAttentionRequired
				if armed {
					state = payment.AccountStateActive
				}
				account, err = scanAccount(tx.QueryRow(ctx, `
					UPDATE commercial_accounts
					SET state = $2
					WHERE id = $1
					RETURNING `+accountColumns,
					account.ID, state,
				))
				if err != nil {
					return TransitionIntentResult{}, err
				}
			}
		}
	} else if intent.Reason == payment.PaymentIntentReasonAutoTopUp && intent.PolicyGeneration != nil {
		policy, policyErr := getPolicyForUpdate(ctx, tx, account.ID)
		if policyErr != nil && !errors.Is(policyErr, ErrNotFound) {
			return TransitionIntentResult{}, policyErr
		}
		if policyErr == nil && policy.Generation == *intent.PolicyGeneration {
			attentionRequired := in.ToState == payment.PaymentIntentStateRequiresAction
			if in.ToState == payment.PaymentIntentStateFailed || in.ToState == payment.PaymentIntentStateCanceled {
				policy.ConsecutiveFailureCount++
				disable := policy.ConsecutiveFailureCount >= 3
				if _, err := tx.Exec(ctx, `
					UPDATE auto_topup_policies
					SET consecutive_failure_count = $2,
						enabled = CASE WHEN $3 THEN false ELSE enabled END,
						armed = false,
						version = version + 1
					WHERE id = $1
				`, policy.ID, policy.ConsecutiveFailureCount, disable); err != nil {
					return TransitionIntentResult{}, err
				}
				attentionRequired = disable
			}
			if attentionRequired && account.State == payment.AccountStateActive {
				account, err = scanAccount(tx.QueryRow(ctx, `
					UPDATE commercial_accounts
					SET state = 'attention_required'
					WHERE id = $1
					RETURNING `+accountColumns,
					account.ID,
				))
				if err != nil {
					return TransitionIntentResult{}, err
				}
			}
		}
	}

	return TransitionIntentResult{Intent: intent, Account: account, CreditEntry: creditPtr}, nil
}

func (s *Store) GetPaymentIntent(ctx context.Context, intentID string) (payment.PaymentIntent, error) {
	return scanIntent(s.db.QueryRow(ctx, `
		SELECT `+intentColumns+`
		FROM payment_intents
		WHERE id = $1
	`, intentID))
}

func getIntentForUpdate(ctx context.Context, tx pgx.Tx, intentID string) (payment.PaymentIntent, error) {
	return scanIntent(tx.QueryRow(ctx, `
		SELECT `+intentColumns+`
		FROM payment_intents
		WHERE id = $1
		FOR UPDATE
	`, intentID))
}

func getIntentByTriggerLedgerEntry(ctx context.Context, tx pgx.Tx, ledgerEntryID string) (payment.PaymentIntent, error) {
	return scanIntent(tx.QueryRow(ctx, `
		SELECT `+intentColumns+`
		FROM payment_intents
		WHERE trigger_ledger_entry_id = $1
	`, ledgerEntryID))
}

func getPaymentIntentCredit(ctx context.Context, tx pgx.Tx, intentID string) (payment.LedgerEntry, error) {
	return scanLedgerEntry(tx.QueryRow(ctx, `
		SELECT `+ledgerColumns+`
		FROM balance_ledger_entries
		WHERE idempotency_scope = 'payment_intent' AND idempotency_key = $1
	`, intentID))
}
