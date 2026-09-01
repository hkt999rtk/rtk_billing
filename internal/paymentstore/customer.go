package paymentstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

type PaymentMethodPage struct {
	Methods []payment.PaymentMethod `json:"payment_methods"`
	Total   int                     `json:"total"`
}

func (s *Store) ListPaymentMethods(ctx context.Context, accountID string, limit, offset int) (PaymentMethodPage, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, accountID, func(view *Store) (PaymentMethodPage, error) {
			return view.ListPaymentMethods(ctx, accountID, limit, offset)
		})
	}
	if !required(accountID) {
		return PaymentMethodPage{}, ErrConflict
	}
	limit, offset = boundedPage(limit, offset)
	args := []any{accountID}
	visibility := methodVisibility(ctx, &args, false)
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM payment_methods WHERE account_id = $1 AND `+visibility, args...).Scan(&total); err != nil {
		return PaymentMethodPage{}, err
	}
	args = append(args, limit, offset)
	rows, err := s.db.Query(ctx, `
		SELECT `+paymentMethodColumns+`
		FROM payment_methods
		WHERE account_id = $1 AND `+visibility+`
		ORDER BY created_at DESC, id DESC
		`+fmt.Sprintf("LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return PaymentMethodPage{}, err
	}
	defer rows.Close()
	methods := make([]payment.PaymentMethod, 0)
	for rows.Next() {
		method, scanErr := scanPaymentMethod(rows)
		if scanErr != nil {
			return PaymentMethodPage{}, scanErr
		}
		methods = append(methods, method)
	}
	return PaymentMethodPage{Methods: methods, Total: total}, rows.Err()
}

func (s *Store) GetPaymentMethod(ctx context.Context, accountID, methodID string) (payment.PaymentMethod, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, accountID, func(view *Store) (payment.PaymentMethod, error) {
			return view.GetPaymentMethod(ctx, accountID, methodID)
		})
	}
	if !required(accountID) || !required(methodID) {
		return payment.PaymentMethod{}, ErrConflict
	}
	args := []any{accountID, methodID}
	visibility := methodVisibility(ctx, &args, false)
	return scanPaymentMethod(s.db.QueryRow(ctx, `
		SELECT `+paymentMethodColumns+`
		FROM payment_methods
		WHERE account_id = $1 AND id = $2 AND `+visibility,
		args...))
}

type RevokePaymentMethodInput struct {
	AccountID string
	MethodID  string
	ActorID   string
	Reason    string
	Now       time.Time
}

type RevokePaymentMethodResult struct {
	Method         payment.PaymentMethod `json:"payment_method"`
	PolicyDisabled bool                  `json:"policy_disabled"`
	Duplicate      bool                  `json:"duplicate"`
}

func (s *Store) RevokePaymentMethod(ctx context.Context, in RevokePaymentMethodInput) (RevokePaymentMethodResult, error) {
	if !required(in.AccountID) || !required(in.MethodID) || !required(in.ActorID) || !required(in.Reason) {
		return RevokePaymentMethodResult{}, ErrConflict
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	} else {
		in.Now = in.Now.UTC()
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return RevokePaymentMethodResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := getAccountForUpdate(ctx, tx, in.AccountID); err != nil {
		return RevokePaymentMethodResult{}, err
	}
	method, err := getPaymentMethodForUpdate(ctx, tx, in.MethodID)
	if err != nil || method.AccountID != in.AccountID {
		if err == nil {
			err = ErrNotFound
		}
		return RevokePaymentMethodResult{}, err
	}
	if method.Status == payment.PaymentMethodStatusRevoked {
		if err := tx.Commit(ctx); err != nil {
			return RevokePaymentMethodResult{}, err
		}
		return RevokePaymentMethodResult{Method: method, Duplicate: true}, nil
	}
	method, err = scanPaymentMethod(tx.QueryRow(ctx, `
		UPDATE payment_methods
		SET status = 'revoked'
		WHERE id = $1
		RETURNING `+paymentMethodColumns,
		in.MethodID,
	))
	if err != nil {
		return RevokePaymentMethodResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE payment_consents
		SET revoked_at = COALESCE(revoked_at, $2),
			revocation_reason = COALESCE(revocation_reason, $3)
		WHERE id = $1
	`, method.ConsentID, in.Now, strings.TrimSpace(in.Reason)); err != nil {
		return RevokePaymentMethodResult{}, err
	}
	command, err := tx.Exec(ctx, `
		UPDATE auto_topup_policies
		SET enabled = false,
			armed = false,
			generation = generation + 1,
			version = version + 1,
			updated_by = $3
		WHERE account_id = $1 AND payment_method_id = $2 AND enabled = true
	`, in.AccountID, in.MethodID, strings.TrimSpace(in.ActorID))
	if err != nil {
		return RevokePaymentMethodResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RevokePaymentMethodResult{}, err
	}
	return RevokePaymentMethodResult{Method: method, PolicyDisabled: command.RowsAffected() > 0}, nil
}

type DisableAutoTopUpPolicyInput struct {
	AccountID       string
	ActorID         string
	ExpectedVersion int64
}

func (s *Store) DisableAutoTopUpPolicy(ctx context.Context, in DisableAutoTopUpPolicyInput) (payment.AutoTopUpPolicy, error) {
	if !required(in.AccountID) || !required(in.ActorID) || in.ExpectedVersion < 0 {
		return payment.AutoTopUpPolicy{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := getAccountForUpdate(ctx, tx, in.AccountID); err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	policy, err := getPolicyForUpdate(ctx, tx, in.AccountID)
	if err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	// A retained predecessor policy is not the current owner's configuration,
	// even when already disabled. Never disclose it through DELETE's response.
	if _, scoped := billingidentity.FromContext(ctx); scoped {
		if _, err := getPaymentMethodForUpdate(ctx, tx, policy.PaymentMethodID); err != nil {
			return payment.AutoTopUpPolicy{}, err
		}
	}
	if in.ExpectedVersion > 0 && policy.Version != in.ExpectedVersion {
		return payment.AutoTopUpPolicy{}, ErrConflict
	}
	if !policy.Enabled {
		if err := tx.Commit(ctx); err != nil {
			return payment.AutoTopUpPolicy{}, err
		}
		return policy, nil
	}
	policy, err = scanPolicy(tx.QueryRow(ctx, `
		UPDATE auto_topup_policies
		SET enabled = false,
			armed = false,
			generation = generation + 1,
			version = version + 1,
			updated_by = $2
		WHERE account_id = $1
		RETURNING `+policyColumns,
		in.AccountID, strings.TrimSpace(in.ActorID),
	))
	if err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	return policy, nil
}

type CreateManualTopUpInput struct {
	AccountID       string
	AmountMinor     int64
	Currency        payment.Currency
	PaymentMethodID string
	IdempotencyKey  string
	CorrelationID   string
	Now             time.Time
}

type CreateManualTopUpResult struct {
	Intent    payment.PaymentIntent `json:"payment_intent"`
	Duplicate bool                  `json:"duplicate"`
}

type CreateHostedTopUpInput struct {
	AccountID, Provider, IdempotencyKey, CorrelationID string
	AmountMinor                                        int64
	Currency                                           payment.Currency
	Now                                                time.Time
}

func (s *Store) CreateHostedTopUp(ctx context.Context, in CreateHostedTopUpInput) (CreateManualTopUpResult, error) {
	in.Provider = payment.NormalizeProvider(in.Provider)
	if !required(in.AccountID) || !required(in.Provider) || !required(in.IdempotencyKey) || !required(in.CorrelationID) {
		return CreateManualTopUpResult{}, ErrConflict
	}
	if err := payment.ValidateChargeAmount(in.Currency, in.AmountMinor); err != nil {
		return CreateManualTopUpResult{}, err
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	} else {
		in.Now = in.Now.UTC()
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CreateManualTopUpResult{}, err
	}
	defer tx.Rollback(ctx)
	account, err := getAccountForUpdate(ctx, tx, in.AccountID)
	if err != nil {
		return CreateManualTopUpResult{}, err
	}
	if account.State == payment.AccountStateClosed || account.State == payment.AccountStateSuspended {
		return CreateManualTopUpResult{}, ErrAccountClosed
	}
	if err := requireNoHandoffTx(ctx, tx, account.ID); err != nil {
		return CreateManualTopUpResult{}, err
	}
	if account.Currency != in.Currency {
		return CreateManualTopUpResult{}, ErrConflict
	}
	existing, existingErr := scanIntent(tx.QueryRow(ctx, `SELECT `+intentColumns+` FROM payment_intents WHERE account_id=$1 AND idempotency_key=$2 FOR UPDATE`, in.AccountID, strings.TrimSpace(in.IdempotencyKey)))
	if existingErr == nil {
		if err := requireCurrentIntentReplay(ctx, tx, existing.ID); err != nil {
			return CreateManualTopUpResult{}, err
		}
		if existing.AmountMinor != in.AmountMinor || existing.Currency != in.Currency || existing.Provider != in.Provider || existing.PaymentMethodID != "" || existing.Reason != payment.PaymentIntentReasonManualTopUp {
			return CreateManualTopUpResult{}, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return CreateManualTopUpResult{}, err
		}
		return CreateManualTopUpResult{Intent: existing, Duplicate: true}, nil
	}
	if !errors.Is(existingErr, ErrNotFound) {
		return CreateManualTopUpResult{}, existingErr
	}
	random := make([]byte, 13)
	if _, err := rand.Read(random); err != nil {
		return CreateManualTopUpResult{}, err
	}
	merchantOrderReference := "rtk_" + hex.EncodeToString(random)
	intent, err := scanIntent(tx.QueryRow(ctx, `
		INSERT INTO payment_intents (account_id,amount_minor,currency,reason,provider,payment_method_id,state,idempotency_key,merchant_order_reference,correlation_id,created_at,updated_at)
		VALUES ($1,$2,$3,'manual_top_up',$4,NULL,'processing',$5,$6,$7,$8,$8)
		RETURNING `+intentColumns,
		in.AccountID, in.AmountMinor, in.Currency, in.Provider, strings.TrimSpace(in.IdempotencyKey), merchantOrderReference, strings.TrimSpace(in.CorrelationID), in.Now))
	if err != nil {
		return CreateManualTopUpResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payment_reconciliation_jobs (intent_id,reason,status,due_at) VALUES ($1,'unknown','pending',$2)`, intent.ID, in.Now.Add(5*time.Minute)); err != nil {
		return CreateManualTopUpResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CreateManualTopUpResult{}, err
	}
	return CreateManualTopUpResult{Intent: intent}, nil
}

func (s *Store) CreateManualTopUp(ctx context.Context, in CreateManualTopUpInput) (CreateManualTopUpResult, error) {
	if !required(in.AccountID) || !required(in.PaymentMethodID) || !required(in.IdempotencyKey) || !required(in.CorrelationID) {
		return CreateManualTopUpResult{}, ErrConflict
	}
	if err := payment.ValidateChargeAmount(in.Currency, in.AmountMinor); err != nil {
		return CreateManualTopUpResult{}, err
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	} else {
		in.Now = in.Now.UTC()
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CreateManualTopUpResult{}, err
	}
	defer tx.Rollback(ctx)
	account, err := getAccountForUpdate(ctx, tx, in.AccountID)
	if err != nil {
		return CreateManualTopUpResult{}, err
	}
	if account.State == payment.AccountStateClosed || account.State == payment.AccountStateSuspended {
		return CreateManualTopUpResult{}, ErrAccountClosed
	}
	if err := requireNoHandoffTx(ctx, tx, account.ID); err != nil {
		return CreateManualTopUpResult{}, err
	}
	if account.Currency != in.Currency {
		return CreateManualTopUpResult{}, ErrConflict
	}
	existing, existingErr := scanIntent(tx.QueryRow(ctx, `
		SELECT `+intentColumns+`
		FROM payment_intents
		WHERE account_id = $1 AND idempotency_key = $2
		FOR UPDATE
	`, in.AccountID, strings.TrimSpace(in.IdempotencyKey)))
	if existingErr == nil {
		if err := requireCurrentIntentReplay(ctx, tx, existing.ID); err != nil {
			return CreateManualTopUpResult{}, err
		}
		if existing.AmountMinor != in.AmountMinor || existing.Currency != in.Currency ||
			existing.PaymentMethodID != in.PaymentMethodID || existing.Reason != payment.PaymentIntentReasonManualTopUp {
			return CreateManualTopUpResult{}, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return CreateManualTopUpResult{}, err
		}
		return CreateManualTopUpResult{Intent: existing, Duplicate: true}, nil
	}
	if !errors.Is(existingErr, ErrNotFound) {
		return CreateManualTopUpResult{}, existingErr
	}
	method, err := getPaymentMethodForUpdate(ctx, tx, in.PaymentMethodID)
	if err != nil {
		return CreateManualTopUpResult{}, err
	}
	if method.AccountID != in.AccountID || method.Status != payment.PaymentMethodStatusActive {
		return CreateManualTopUpResult{}, payment.ErrPaymentMethodInactive
	}
	if !method.Capabilities.MerchantInitiatedCharge {
		return CreateManualTopUpResult{}, payment.ErrCapabilityUnsupported
	}
	intent, err := scanIntent(tx.QueryRow(ctx, `
		INSERT INTO payment_intents (
			account_id, amount_minor, currency, reason, provider,
			payment_method_id, state, idempotency_key, correlation_id,
			created_at, updated_at
		)
		VALUES ($1, $2, $3, 'manual_top_up', $4, $5, 'created', $6, $7, $8, $8)
		RETURNING `+intentColumns,
		in.AccountID, in.AmountMinor, in.Currency, method.Provider,
		in.PaymentMethodID, strings.TrimSpace(in.IdempotencyKey), strings.TrimSpace(in.CorrelationID), in.Now,
	))
	if err != nil {
		return CreateManualTopUpResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO payment_reconciliation_jobs (intent_id, reason, status, due_at)
		VALUES ($1, 'charge', 'pending', $2)
	`, intent.ID, in.Now); err != nil {
		return CreateManualTopUpResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CreateManualTopUpResult{}, err
	}
	return CreateManualTopUpResult{Intent: intent}, nil
}

type PaymentIntentPage struct {
	Intents []payment.PaymentIntent `json:"payment_intents"`
	Total   int                     `json:"total"`
}

func (s *Store) ListPaymentIntents(ctx context.Context, accountID string, limit, offset int) (PaymentIntentPage, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, accountID, func(view *Store) (PaymentIntentPage, error) {
			return view.ListPaymentIntents(ctx, accountID, limit, offset)
		})
	}
	if !required(accountID) {
		return PaymentIntentPage{}, ErrConflict
	}
	limit, offset = boundedPage(limit, offset)
	args := []any{accountID}
	visibility := intentVisibility(ctx, &args)
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM payment_intents WHERE account_id = $1 AND `+visibility, args...).Scan(&total); err != nil {
		return PaymentIntentPage{}, err
	}
	args = append(args, limit, offset)
	rows, err := s.db.Query(ctx, `
		SELECT `+intentColumns+`
		FROM payment_intents
		WHERE account_id = $1 AND `+visibility+`
		ORDER BY created_at DESC, id DESC
		`+fmt.Sprintf("LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return PaymentIntentPage{}, err
	}
	defer rows.Close()
	intents := make([]payment.PaymentIntent, 0)
	for rows.Next() {
		intent, scanErr := scanIntent(rows)
		if scanErr != nil {
			return PaymentIntentPage{}, scanErr
		}
		intents = append(intents, intent)
	}
	return PaymentIntentPage{Intents: intents, Total: total}, rows.Err()
}

func (s *Store) GetPaymentIntentForAccount(ctx context.Context, accountID, intentID string) (payment.PaymentIntent, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, accountID, func(view *Store) (payment.PaymentIntent, error) {
			return view.GetPaymentIntentForAccount(ctx, accountID, intentID)
		})
	}
	if !required(accountID) || !required(intentID) {
		return payment.PaymentIntent{}, ErrConflict
	}
	args := []any{accountID, intentID}
	visibility := intentVisibility(ctx, &args)
	return scanIntent(s.db.QueryRow(ctx, `
		SELECT `+intentColumns+`
		FROM payment_intents
		WHERE account_id = $1 AND id = $2 AND `+visibility,
		args...))
}

func (s *Store) ListPaymentAttempts(ctx context.Context, intentID string) ([]payment.PaymentAttempt, error) {
	if scope, ok := billingidentity.FromContext(ctx); ok {
		if !s.tenantRead {
			return tenantRead(ctx, s, scope.AccountID, func(view *Store) ([]payment.PaymentAttempt, error) { return view.ListPaymentAttempts(ctx, intentID) })
		}
		if _, err := s.GetPaymentIntentForAccount(ctx, scope.AccountID, intentID); err != nil {
			return nil, err
		}
	}
	if !required(intentID) {
		return nil, ErrConflict
	}
	rows, err := s.db.Query(ctx, `
		SELECT `+attemptColumns+`
		FROM payment_attempts
		WHERE intent_id = $1
		ORDER BY attempt_number, created_at
	`, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := make([]payment.PaymentAttempt, 0)
	for rows.Next() {
		attempt, scanErr := scanAttempt(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func boundedPage(limit, offset int) (int, int) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
