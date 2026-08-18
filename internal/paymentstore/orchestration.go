package paymentstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

var (
	ErrLeaseConflict    = errors.New("payment job lease conflict")
	ErrJobNotActionable = errors.New("payment job is no longer actionable")
)

type ProviderAttemptWork struct {
	Job                           payment.ReconciliationJob `json:"job"`
	Attempt                       payment.PaymentAttempt    `json:"attempt"`
	Intent                        payment.PaymentIntent     `json:"intent"`
	ProviderMethodRefCiphertext   []byte                    `json:"-"`
	ProviderCustomerRefCiphertext []byte                    `json:"-"`
	RecoverIncompleteAttempt      bool                      `json:"recover_incomplete_attempt"`
}

func (s *Store) ClaimPaymentJobs(ctx context.Context, now, staleBefore time.Time, leaseOwner string, limit int) ([]payment.ReconciliationJob, error) {
	if !required(leaseOwner) {
		return nil, ErrConflict
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(ctx, `
		WITH candidates AS (
			SELECT id
			FROM payment_reconciliation_jobs
			WHERE (status = 'pending' AND due_at <= $1)
			   OR (status = 'leased' AND leased_at <= $2)
			ORDER BY due_at, created_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $4
		), claimed AS (
			UPDATE payment_reconciliation_jobs AS jobs
			SET status = 'leased',
				leased_at = $1,
				lease_owner = $3,
				attempt_count = attempt_count + 1,
				last_error = NULL
			FROM candidates
			WHERE jobs.id = candidates.id
			RETURNING jobs.*
		)
		SELECT `+reconciliationJobColumns+`
		FROM claimed
		ORDER BY due_at, created_at, id
	`, now.UTC(), staleBefore.UTC(), strings.TrimSpace(leaseOwner), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []payment.ReconciliationJob
	for rows.Next() {
		job, err := scanReconciliationJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

type BeginProviderAttemptInput struct {
	JobID      string
	LeaseOwner string
	Operation  payment.ProviderOperation
	Now        time.Time
}

func (s *Store) BeginProviderAttempt(ctx context.Context, in BeginProviderAttemptInput) (ProviderAttemptWork, error) {
	if !required(in.JobID) || !required(in.LeaseOwner) ||
		(in.Operation != payment.ProviderOperationCharge && in.Operation != payment.ProviderOperationQuery) {
		return ProviderAttemptWork{}, ErrConflict
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	} else {
		in.Now = in.Now.UTC()
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ProviderAttemptWork{}, err
	}
	defer tx.Rollback(ctx)

	job, err := scanReconciliationJob(tx.QueryRow(ctx, `
		SELECT `+reconciliationJobColumns+`
		FROM payment_reconciliation_jobs
		WHERE id = $1
		FOR UPDATE
	`, in.JobID))
	if err != nil {
		return ProviderAttemptWork{}, err
	}
	if job.Status != payment.ReconciliationStatusLeased || job.LeaseOwner != strings.TrimSpace(in.LeaseOwner) {
		return ProviderAttemptWork{}, ErrLeaseConflict
	}

	var accountID string
	if err := tx.QueryRow(ctx, `SELECT account_id::text FROM payment_intents WHERE id = $1`, job.IntentID).Scan(&accountID); err != nil {
		return ProviderAttemptWork{}, mapNotFound(err)
	}
	if _, err := getAccountForUpdate(ctx, tx, accountID); err != nil {
		return ProviderAttemptWork{}, err
	}
	intent, err := getIntentForUpdate(ctx, tx, job.IntentID)
	if err != nil {
		return ProviderAttemptWork{}, err
	}
	if payment.IntentStateTerminal(intent.State) {
		return ProviderAttemptWork{}, ErrJobNotActionable
	}

	existing, existingErr := scanAttempt(tx.QueryRow(ctx, `
		SELECT `+attemptColumns+`
		FROM payment_attempts
		WHERE intent_id = $1 AND operation = $2 AND completed_at IS NULL
		ORDER BY attempt_number DESC
		LIMIT 1
		FOR UPDATE
	`, intent.ID, in.Operation))
	if existingErr == nil {
		if err := tx.Commit(ctx); err != nil {
			return ProviderAttemptWork{}, err
		}
		return ProviderAttemptWork{
			Job: job, Attempt: existing, Intent: intent, RecoverIncompleteAttempt: true,
		}, nil
	}
	if !errors.Is(existingErr, ErrNotFound) {
		return ProviderAttemptWork{}, existingErr
	}

	switch in.Operation {
	case payment.ProviderOperationCharge:
		if job.Reason != payment.ReconciliationReasonCharge || intent.State != payment.PaymentIntentStateCreated {
			return ProviderAttemptWork{}, ErrJobNotActionable
		}
		if intent.MerchantOrderReference == "" {
			compactID := strings.ReplaceAll(intent.ID, "-", "")
			intent.MerchantOrderReference = "rtk_" + compactID[:26]
		}
		intent, err = scanIntent(tx.QueryRow(ctx, `
			UPDATE payment_intents
			SET state = 'processing',
				merchant_order_reference = $2,
				updated_at = $3
			WHERE id = $1
			RETURNING `+intentColumns,
			intent.ID, intent.MerchantOrderReference, in.Now,
		))
		if err != nil {
			return ProviderAttemptWork{}, err
		}
	case payment.ProviderOperationQuery:
		if job.Reason == payment.ReconciliationReasonCharge ||
			(intent.State != payment.PaymentIntentStateProcessing &&
				intent.State != payment.PaymentIntentStateAuthorized &&
				intent.State != payment.PaymentIntentStateRequiresAction &&
				intent.State != payment.PaymentIntentStateUnknown) {
			return ProviderAttemptWork{}, ErrJobNotActionable
		}
	}

	var attemptNumber int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(attempt_number), 0) + 1 FROM payment_attempts WHERE intent_id = $1`, intent.ID).Scan(&attemptNumber); err != nil {
		return ProviderAttemptWork{}, err
	}
	attempt, err := scanAttempt(tx.QueryRow(ctx, `
		INSERT INTO payment_attempts (
			intent_id, operation, attempt_number, started_at, normalized_result
		)
		VALUES ($1, $2, $3, $4, 'started')
		RETURNING `+attemptColumns,
		intent.ID, in.Operation, attemptNumber, in.Now,
	))
	if err != nil {
		return ProviderAttemptWork{}, err
	}

	var methodRefCiphertext, customerRefCiphertext []byte
	if in.Operation == payment.ProviderOperationCharge {
		if err := tx.QueryRow(ctx, `
			SELECT provider_method_ref_ciphertext, provider_customer_ref_ciphertext
			FROM payment_methods
			WHERE id = $1 AND account_id = $2 AND provider = $3 AND status = 'active'
		`, intent.PaymentMethodID, intent.AccountID, intent.Provider).Scan(&methodRefCiphertext, &customerRefCiphertext); err != nil {
			return ProviderAttemptWork{}, mapNotFound(err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ProviderAttemptWork{}, err
	}
	return ProviderAttemptWork{
		Job:                           job,
		Attempt:                       attempt,
		Intent:                        intent,
		ProviderMethodRefCiphertext:   methodRefCiphertext,
		ProviderCustomerRefCiphertext: customerRefCiphertext,
	}, nil
}

func (s *Store) SetAttemptRequestDigest(ctx context.Context, attemptID, sha256 string) error {
	if !required(attemptID) || !validSHA256(sha256) {
		return ErrConflict
	}
	command, err := s.db.Exec(ctx, `
		UPDATE payment_attempts
		SET request_sha256 = $2
		WHERE id = $1 AND completed_at IS NULL AND request_sha256 IS NULL
	`, attemptID, strings.ToLower(sha256))
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

type FinalizeProviderAttemptInput struct {
	JobID                string
	LeaseOwner           string
	AttemptID            string
	Result               payment.ProviderResult
	ResponseSHA256       string
	Now                  time.Time
	NextReconciliationAt time.Time
}

type FinalizeProviderAttemptResult struct {
	Transition TransitionIntentResult    `json:"transition"`
	Job        payment.ReconciliationJob `json:"job"`
	Duplicate  bool                      `json:"duplicate"`
}

func (s *Store) FinalizeProviderAttempt(ctx context.Context, in FinalizeProviderAttemptInput) (FinalizeProviderAttemptResult, error) {
	if !required(in.JobID) || !required(in.LeaseOwner) || !required(in.AttemptID) ||
		(in.ResponseSHA256 != "" && !validSHA256(in.ResponseSHA256)) {
		return FinalizeProviderAttemptResult{}, ErrConflict
	}
	if err := payment.ValidateProviderResult(in.Result); err != nil {
		return FinalizeProviderAttemptResult{}, err
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	} else {
		in.Now = in.Now.UTC()
	}
	if in.NextReconciliationAt.IsZero() {
		in.NextReconciliationAt = in.Now.Add(time.Minute)
	} else {
		in.NextReconciliationAt = in.NextReconciliationAt.UTC()
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return FinalizeProviderAttemptResult{}, err
	}
	defer tx.Rollback(ctx)

	job, err := scanReconciliationJob(tx.QueryRow(ctx, `
		SELECT `+reconciliationJobColumns+`
		FROM payment_reconciliation_jobs
		WHERE id = $1
		FOR UPDATE
	`, in.JobID))
	if err != nil {
		return FinalizeProviderAttemptResult{}, err
	}
	if job.Status != payment.ReconciliationStatusLeased || job.LeaseOwner != strings.TrimSpace(in.LeaseOwner) {
		return FinalizeProviderAttemptResult{}, ErrLeaseConflict
	}
	attempt, err := scanAttempt(tx.QueryRow(ctx, `
		SELECT `+attemptColumns+`
		FROM payment_attempts
		WHERE id = $1
		FOR UPDATE
	`, in.AttemptID))
	if err != nil {
		return FinalizeProviderAttemptResult{}, err
	}
	if attempt.IntentID != job.IntentID {
		return FinalizeProviderAttemptResult{}, ErrConflict
	}
	if attempt.CompletedAt != nil {
		return FinalizeProviderAttemptResult{Job: job, Duplicate: true}, nil
	}

	var accountID string
	if err := tx.QueryRow(ctx, `SELECT account_id::text FROM payment_intents WHERE id = $1`, job.IntentID).Scan(&accountID); err != nil {
		return FinalizeProviderAttemptResult{}, mapNotFound(err)
	}
	account, err := getAccountForUpdate(ctx, tx, accountID)
	if err != nil {
		return FinalizeProviderAttemptResult{}, err
	}
	intent, err := getIntentForUpdate(ctx, tx, job.IntentID)
	if err != nil {
		return FinalizeProviderAttemptResult{}, err
	}

	providerCode := payment.NormalizeProviderCode(in.Result.ProviderCode)
	if _, err := tx.Exec(ctx, `
		UPDATE payment_attempts
		SET completed_at = $2,
			normalized_result = $3,
			provider_code = NULLIF($4, ''),
			response_sha256 = NULLIF($5, ''),
			next_reconciliation_at = $6
		WHERE id = $1
	`, attempt.ID, in.Now, in.Result.State, providerCode, strings.ToLower(in.ResponseSHA256),
		nullTimeUnlessUnknown(in.Result.State, in.NextReconciliationAt)); err != nil {
		return FinalizeProviderAttemptResult{}, err
	}

	transition, err := transitionIntentTx(ctx, tx, account, intent, TransitionIntentInput{
		IntentID:                     intent.ID,
		ToState:                      in.Result.State,
		ProviderTransactionReference: in.Result.ProviderTransactionReference,
		Now:                          in.Now,
	})
	if err != nil {
		return FinalizeProviderAttemptResult{}, err
	}

	if in.Result.State == payment.PaymentIntentStateUnknown && job.Reason == payment.ReconciliationReasonUnknown {
		job, err = scanReconciliationJob(tx.QueryRow(ctx, `
			UPDATE payment_reconciliation_jobs
			SET status = 'pending', due_at = $2, leased_at = NULL,
				lease_owner = NULL, last_error = NULL
			WHERE id = $1
			RETURNING `+reconciliationJobColumns,
			job.ID, in.NextReconciliationAt,
		))
	} else {
		job, err = scanReconciliationJob(tx.QueryRow(ctx, `
			UPDATE payment_reconciliation_jobs
			SET status = 'completed', leased_at = NULL,
				lease_owner = NULL, last_error = NULL
			WHERE id = $1
			RETURNING `+reconciliationJobColumns,
			job.ID,
		))
		if err == nil && in.Result.State == payment.PaymentIntentStateUnknown {
			_, err = tx.Exec(ctx, `
				INSERT INTO payment_reconciliation_jobs (intent_id, reason, status, due_at)
				VALUES ($1, 'unknown', 'pending', $2)
				ON CONFLICT DO NOTHING
			`, intent.ID, in.NextReconciliationAt)
		}
	}
	if err != nil {
		return FinalizeProviderAttemptResult{}, err
	}
	if in.Result.State != payment.PaymentIntentStateUnknown {
		if _, err := tx.Exec(ctx, `
			UPDATE payment_webhook_inbox
			SET processing_state = 'processed', processed_at = $2
			WHERE intent_id = $1 AND processing_state = 'scheduled'
		`, intent.ID, in.Now); err != nil {
			return FinalizeProviderAttemptResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return FinalizeProviderAttemptResult{}, err
	}
	return FinalizeProviderAttemptResult{Transition: transition, Job: job}, nil
}

func (s *Store) CompletePaymentJob(ctx context.Context, jobID, leaseOwner string) error {
	command, err := s.db.Exec(ctx, `
		UPDATE payment_reconciliation_jobs
		SET status = 'completed', leased_at = NULL, lease_owner = NULL, last_error = NULL
		WHERE id = $1 AND status = 'leased' AND lease_owner = $2
	`, jobID, strings.TrimSpace(leaseOwner))
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseConflict
	}
	return nil
}

func (s *Store) RetryPaymentJob(ctx context.Context, jobID, leaseOwner string, dueAt time.Time, safeError string) error {
	safeError = sanitizeSafeError(safeError)
	command, err := s.db.Exec(ctx, `
		UPDATE payment_reconciliation_jobs
		SET status = 'pending', due_at = $3, leased_at = NULL,
			lease_owner = NULL, last_error = NULLIF($4, '')
		WHERE id = $1 AND status = 'leased' AND lease_owner = $2
	`, jobID, strings.TrimSpace(leaseOwner), dueAt.UTC(), safeError)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrLeaseConflict
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range strings.ToLower(value) {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func nullTimeUnlessUnknown(state payment.PaymentIntentState, value time.Time) any {
	if state == payment.PaymentIntentStateUnknown {
		return value.UTC()
	}
	return nil
}

func sanitizeSafeError(value string) string {
	return payment.NormalizeProviderCode(value)
}
