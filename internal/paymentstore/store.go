package paymentstore

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

var (
	ErrNotFound            = errors.New("payment resource not found")
	ErrConflict            = errors.New("payment resource conflict")
	ErrIdempotencyConflict = errors.New("payment idempotency conflict")
	ErrAccountClosed       = errors.New("commercial account is closed")
)

type Store struct {
	db         database.Connection
	tenantRead bool
}

func New(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

type rowScanner interface {
	Scan(dest ...any) error
}

func mapNotFound(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55000" && (pgErr.ConstraintName == "billing_handoff_commit_barrier" || pgErr.ConstraintName == "billing_cloud_closure_barrier") {
		return ErrHandoffFenced
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func required(value string) bool {
	return strings.TrimSpace(value) != ""
}

func scanAccount(row rowScanner) (payment.CommercialAccount, error) {
	var out payment.CommercialAccount
	err := row.Scan(
		&out.ID,
		&out.OrganizationID,
		&out.Currency,
		&out.AvailableBalanceMinor,
		&out.State,
		&out.Version,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	return out, mapNotFound(err)
}

func scanConsent(row rowScanner) (payment.PaymentConsent, error) {
	var out payment.PaymentConsent
	var revocationReason *string
	err := row.Scan(
		&out.ID,
		&out.AccountID,
		&out.ConsentType,
		&out.TextVersion,
		&out.TextSHA256,
		&out.AcceptedActorType,
		&out.AcceptedActorID,
		&out.AcceptedAt,
		&out.Locale,
		&out.Source,
		&out.RevokedAt,
		&revocationReason,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if revocationReason != nil {
		out.RevocationReason = *revocationReason
	}
	return out, mapNotFound(err)
}

func scanPaymentMethod(row rowScanner) (payment.PaymentMethod, error) {
	var out payment.PaymentMethod
	var capabilities []byte
	err := row.Scan(
		&out.ID,
		&out.AccountID,
		&out.Provider,
		&out.Status,
		&out.CardBrand,
		&out.LastFour,
		&out.ExpiryMonth,
		&out.ExpiryYear,
		&capabilities,
		&out.ConsentID,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if err != nil {
		return payment.PaymentMethod{}, mapNotFound(err)
	}
	if err := json.Unmarshal(capabilities, &out.Capabilities); err != nil {
		return payment.PaymentMethod{}, err
	}
	return out, nil
}

func scanPolicy(row rowScanner) (payment.AutoTopUpPolicy, error) {
	var out payment.AutoTopUpPolicy
	err := row.Scan(
		&out.ID,
		&out.AccountID,
		&out.Enabled,
		&out.ThresholdMinor,
		&out.TopUpAmountMinor,
		&out.Currency,
		&out.PaymentMethodID,
		&out.DailyAttemptLimit,
		&out.DailyAmountLimitMinor,
		&out.CooldownSeconds,
		&out.Generation,
		&out.Version,
		&out.Armed,
		&out.ConsecutiveFailureCount,
		&out.LastTriggeredAt,
		&out.LastSucceededAt,
		&out.ConsentID,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	return out, mapNotFound(err)
}

func scanLedgerEntry(row rowScanner) (payment.LedgerEntry, error) {
	var out payment.LedgerEntry
	var externalType, externalID, actorType, actorID, requestID *string
	err := row.Scan(
		&out.ID,
		&out.AccountID,
		&out.Direction,
		&out.AmountMinor,
		&out.Currency,
		&out.Reason,
		&out.IdempotencyScope,
		&out.IdempotencyKey,
		&externalType,
		&externalID,
		&out.BalanceAfterMinor,
		&actorType,
		&actorID,
		&requestID,
		&out.CreatedAt,
	)
	if externalType != nil {
		out.ExternalType = *externalType
		out.ExternalID = *externalID
	}
	if actorType != nil {
		out.ActorType = *actorType
	}
	if actorID != nil {
		out.ActorID = *actorID
	}
	if requestID != nil {
		out.RequestID = *requestID
	}
	return out, mapNotFound(err)
}

func scanIntent(row rowScanner) (payment.PaymentIntent, error) {
	var out payment.PaymentIntent
	var triggerLedgerID, merchantOrderRef, providerTransactionRef *string
	err := row.Scan(
		&out.ID,
		&out.AccountID,
		&out.AmountMinor,
		&out.Currency,
		&out.Reason,
		&out.PolicyGeneration,
		&triggerLedgerID,
		&out.Provider,
		&out.PaymentMethodID,
		&out.State,
		&out.IdempotencyKey,
		&merchantOrderRef,
		&providerTransactionRef,
		&out.CorrelationID,
		&out.CreatedAt,
		&out.UpdatedAt,
		&out.CompletedAt,
	)
	if triggerLedgerID != nil {
		out.TriggerLedgerEntryID = *triggerLedgerID
	}
	if merchantOrderRef != nil {
		out.MerchantOrderReference = *merchantOrderRef
	}
	if providerTransactionRef != nil {
		out.ProviderTransactionReference = *providerTransactionRef
	}
	return out, mapNotFound(err)
}

func scanAttempt(row rowScanner) (payment.PaymentAttempt, error) {
	var out payment.PaymentAttempt
	var providerCode, requestSHA256, responseSHA256 *string
	err := row.Scan(
		&out.ID,
		&out.IntentID,
		&out.Operation,
		&out.AttemptNumber,
		&out.StartedAt,
		&out.CompletedAt,
		&out.NormalizedResult,
		&providerCode,
		&requestSHA256,
		&responseSHA256,
		&out.NextReconciliationAt,
		&out.CreatedAt,
	)
	if providerCode != nil {
		out.ProviderCode = *providerCode
	}
	if requestSHA256 != nil {
		out.RequestSHA256 = *requestSHA256
	}
	if responseSHA256 != nil {
		out.ResponseSHA256 = *responseSHA256
	}
	return out, mapNotFound(err)
}

func scanReconciliationJob(row rowScanner) (payment.ReconciliationJob, error) {
	var out payment.ReconciliationJob
	var leaseOwner, lastError *string
	err := row.Scan(
		&out.ID,
		&out.IntentID,
		&out.Reason,
		&out.Status,
		&out.DueAt,
		&out.AttemptCount,
		&out.LeasedAt,
		&leaseOwner,
		&lastError,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if leaseOwner != nil {
		out.LeaseOwner = *leaseOwner
	}
	if lastError != nil {
		out.LastError = *lastError
	}
	return out, mapNotFound(err)
}

func scanWebhookReceipt(row rowScanner) (payment.WebhookReceipt, error) {
	var out payment.WebhookReceipt
	var providerEventReference, intentID, normalizedEventType *string
	var safeSummary []byte
	err := row.Scan(
		&out.ID,
		&out.Provider,
		&providerEventReference,
		&out.PayloadSHA256,
		&out.VerificationResult,
		&intentID,
		&normalizedEventType,
		&out.ProcessingState,
		&safeSummary,
		&out.ReceivedAt,
		&out.ProcessedAt,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if providerEventReference != nil {
		out.ProviderEventReference = *providerEventReference
	}
	if intentID != nil {
		out.IntentID = *intentID
	}
	if normalizedEventType != nil {
		out.NormalizedEventType = *normalizedEventType
	}
	if err == nil {
		err = json.Unmarshal(safeSummary, &out.SafeSummary)
	}
	return out, mapNotFound(err)
}

const accountColumns = `
	id::text, organization_id::text, currency, available_balance_minor,
	state, version, created_at, updated_at`

const consentColumns = `
	id::text, account_id::text, consent_type, text_version, text_sha256,
	accepted_actor_type, accepted_actor_id, accepted_at, locale, source,
	revoked_at, revocation_reason, created_at, updated_at`

const paymentMethodColumns = `
	id::text, account_id::text, provider, status, COALESCE(card_brand, ''),
	COALESCE(last_four, ''), expiry_month, expiry_year, capabilities,
	consent_id::text, created_at, updated_at`

const policyColumns = `
	id::text, account_id::text, enabled, threshold_minor, top_up_amount_minor,
	currency, payment_method_id::text, daily_attempt_limit,
	daily_amount_limit_minor, cooldown_seconds, generation, version, armed,
	consecutive_failure_count, last_triggered_at, last_succeeded_at,
	consent_id::text, created_at, updated_at`

const ledgerColumns = `
	id::text, account_id::text, direction, amount_minor, currency, reason,
	idempotency_scope, idempotency_key, external_type, external_id,
	balance_after_minor, actor_type, actor_id, request_id, created_at`

const intentColumns = `
	id::text, account_id::text, amount_minor, currency, reason,
	policy_generation, trigger_ledger_entry_id::text, provider,
	COALESCE(payment_method_id::text, ''), state, idempotency_key, merchant_order_reference,
	provider_transaction_reference, correlation_id, created_at, updated_at,
	completed_at`

const attemptColumns = `
	id::text, intent_id::text, operation, attempt_number, started_at,
	completed_at, normalized_result, provider_code, request_sha256,
	response_sha256, next_reconciliation_at, created_at`

const reconciliationJobColumns = `
	id::text, intent_id::text, reason, status, due_at, attempt_count,
	leased_at, lease_owner, last_error, created_at, updated_at`

const webhookReceiptColumns = `
	id::text, provider, provider_event_reference, payload_sha256,
	verification_result, intent_id::text, normalized_event_type,
	processing_state, redacted_summary, received_at, processed_at,
	created_at, updated_at`
