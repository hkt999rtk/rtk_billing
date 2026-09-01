package paymentstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

var ErrProviderReversalRequired = errors.New("refunds and chargebacks require verified provider reversal attribution")

// Input is supplied by a signature-verified provider worker, not a tenant debit
// endpoint. A digest preserves evidence; its presence alone does not verify a
// provider signature. Provider event IDs are globally deduplicated per provider.
type RecordProviderReversalInput struct {
	AccountID             string
	Provider              string
	EventReference        string
	OriginalIntentID      string
	AmountMinor           int64
	Currency              payment.Currency
	Reason                payment.LedgerReason
	ProviderPayloadSHA256 string
	RequestID             string
}

type ProviderReversalResult struct {
	EventID      string                    `json:"event_id"`
	Disposition  string                    `json:"disposition"`
	PeriodID     string                    `json:"period_id,omitempty"`
	ReviewReason string                    `json:"review_reason,omitempty"`
	Entry        *payment.LedgerEntry      `json:"entry,omitempty"`
	Account      payment.CommercialAccount `json:"account"`
	Duplicate    bool                      `json:"duplicate"`
}

func (s *Store) RecordProviderReversal(ctx context.Context, in RecordProviderReversalInput) (ProviderReversalResult, error) {
	in.Provider = payment.NormalizeProvider(in.Provider)
	in.EventReference = strings.TrimSpace(in.EventReference)
	in.RequestID = strings.TrimSpace(in.RequestID)
	if !canonicalUUID(in.AccountID) || !canonicalUUID(in.OriginalIntentID) || in.Provider == "" ||
		in.EventReference == "" || len(in.EventReference) > 200 || in.RequestID == "" ||
		!validLowerSHA256(in.ProviderPayloadSHA256) || !isSettlementReversal(in.Reason) {
		return ProviderReversalResult{}, ErrConflict
	}
	if err := payment.ValidateChargeAmount(in.Currency, in.AmountMinor); err != nil {
		return ProviderReversalResult{}, err
	}
	digestInput := in
	digestInput.RequestID = "" // transport correlation may change on retry
	digest, err := handoffDigest(digestInput)
	if err != nil {
		return ProviderReversalResult{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ProviderReversalResult{}, err
	}
	defer tx.Rollback(ctx)
	account, err := getAccountForUpdate(ctx, tx, in.AccountID)
	if err != nil {
		return ProviderReversalResult{}, err
	}
	var eventID, previousSHA string
	err = tx.QueryRow(ctx, `SELECT id::text,request_sha256 FROM billing_provider_reversal_events WHERE provider=$1 AND event_reference=$2`, in.Provider, in.EventReference).Scan(&eventID, &previousSHA)
	if err == nil {
		if previousSHA != digest {
			return ProviderReversalResult{}, ErrIdempotencyConflict
		}
		result, err := getProviderReversalResultTx(ctx, tx, account, eventID)
		result.Duplicate = true
		return result, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ProviderReversalResult{}, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO billing_provider_reversal_events
		(account_id,provider,event_reference,original_intent_id,amount_minor,currency,reason,request_sha256,provider_payload_sha256,request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id::text`,
		in.AccountID, in.Provider, in.EventReference, in.OriginalIntentID, in.AmountMinor, in.Currency, in.Reason, digest, in.ProviderPayloadSHA256, in.RequestID).Scan(&eventID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ProviderReversalResult{}, ErrIdempotencyConflict
		}
		return ProviderReversalResult{}, err
	}
	result, err := allocateProviderReversalTx(ctx, tx, account, eventID, in, "")
	if err != nil {
		return ProviderReversalResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProviderReversalResult{}, err
	}
	return result, nil
}

type reversalAllocation struct{ periodID, evidenceSHA, disposition, reviewReason string }

func classifyProviderReversalTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount, eventID string, in RecordProviderReversalInput) (reversalAllocation, error) {
	var intentID, provider, currency, state string
	var originalAmount int64
	err := tx.QueryRow(ctx, `SELECT id::text,provider,currency,state,amount_minor FROM payment_intents WHERE id=$1 AND account_id=$2`, in.OriginalIntentID, account.ID).
		Scan(&intentID, &provider, &currency, &state, &originalAmount)
	if errors.Is(err, pgx.ErrNoRows) {
		return reversalAllocation{reviewReason: "original_payment_unresolved"}, nil
	}
	if err != nil {
		return reversalAllocation{}, err
	}
	if provider != in.Provider || currency != string(in.Currency) {
		return reversalAllocation{reviewReason: "original_payment_mismatch"}, nil
	}
	credit, err := scanLedgerEntry(tx.QueryRow(ctx, `SELECT `+ledgerColumns+` FROM balance_ledger_entries
		WHERE account_id=$1 AND idempotency_scope='payment_intent' AND idempotency_key=$2`, account.ID, intentID))
	if errors.Is(err, ErrNotFound) {
		return reversalAllocation{reviewReason: "original_payment_not_settled"}, nil
	}
	if err != nil {
		return reversalAllocation{}, err
	}
	if state != string(payment.PaymentIntentStateSucceeded) || credit.AmountMinor != originalAmount || credit.Currency != in.Currency ||
		credit.Direction != payment.LedgerDirectionCredit || credit.Reason != payment.LedgerReasonPaymentTopUpCredit ||
		credit.ExternalType != "payment_intent" || credit.ExternalID != intentID {
		return reversalAllocation{reviewReason: "original_payment_not_settled"}, nil
	}
	var total int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(e.amount_minor),0)::bigint FROM billing_provider_reversal_events e
		JOIN billing_provider_reversal_allocations a ON a.event_id=e.id WHERE e.account_id=$1 AND e.original_intent_id=$2 AND e.id<>$3`, account.ID, intentID, eventID).Scan(&total); err != nil {
		return reversalAllocation{}, err
	}
	if total > originalAmount || in.AmountMinor > originalAmount-total {
		return reversalAllocation{reviewReason: "reversal_exceeds_original_credit"}, nil
	}
	var allocation reversalAllocation
	var ended *time.Time
	var committed bool
	err = tx.QueryRow(ctx, `SELECT b.period_id::text,b.evidence_sha256,p.effective_until,
		EXISTS(SELECT 1 FROM billing_ownership_handoffs h JOIN billing_handoff_committed_decisions d ON d.operation_id=h.id
		 WHERE h.account_id=b.account_id AND h.source_period_id=b.period_id)
		FROM billing_payment_responsibility b JOIN billing_responsibility_periods p ON p.id=b.period_id
		WHERE b.intent_id=$1 AND b.account_id=$2`, intentID, account.ID).Scan(&allocation.periodID, &allocation.evidenceSHA, &ended, &committed)
	if errors.Is(err, pgx.ErrNoRows) {
		return reversalAllocation{reviewReason: "payment_responsibility_unproven"}, nil
	}
	if err != nil {
		return reversalAllocation{}, err
	}
	if ended != nil || committed {
		allocation.disposition = "predecessor_adjustment"
		return allocation, nil
	}
	if account.State == payment.AccountStateClosed {
		return reversalAllocation{reviewReason: "commercial_account_closed"}, nil
	}
	var commitPending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_ownership_handoffs h WHERE account_id=$1 AND
		(phase IN ('commit_authorized','finalizing') OR (phase='abort_pending' AND EXISTS(SELECT 1 FROM billing_handoff_commit_authorizations a WHERE a.operation_id=h.id))))`, account.ID).Scan(&commitPending); err != nil {
		return reversalAllocation{}, err
	}
	if commitPending {
		return reversalAllocation{reviewReason: "ownership_commit_in_progress"}, nil
	}
	allocation.disposition = "current_balance"
	return allocation, nil
}

func allocateProviderReversalTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount, eventID string, in RecordProviderReversalInput, resolutionSHA string) (ProviderReversalResult, error) {
	allocation, err := classifyProviderReversalTx(ctx, tx, account, eventID, in)
	if err != nil {
		return ProviderReversalResult{}, err
	}
	result := ProviderReversalResult{EventID: eventID, Account: account, Disposition: allocation.disposition, PeriodID: allocation.periodID, ReviewReason: allocation.reviewReason}
	if allocation.reviewReason != "" {
		if resolutionSHA != "" {
			return ProviderReversalResult{}, ErrConflict
		}
		if _, err := tx.Exec(ctx, `INSERT INTO billing_provider_reversal_reviews(event_id,reason_code) VALUES ($1,$2)`, eventID, allocation.reviewReason); err != nil {
			return ProviderReversalResult{}, err
		}
		result.Disposition = "review"
	} else {
		var ledgerID any
		if allocation.disposition == "current_balance" {
			entry, updated, err := insertLedgerEntryTx(ctx, tx, account, PostLedgerEntryInput{
				AccountID: account.ID, Direction: payment.LedgerDirectionDebit, AmountMinor: in.AmountMinor, Currency: in.Currency, Reason: in.Reason,
				IdempotencyScope: "provider_reversal/" + in.Provider, IdempotencyKey: in.EventReference, ExternalType: "payment_intent", ExternalID: in.OriginalIntentID,
				ActorType: "service", ActorID: "provider_reconciliation", RequestID: in.RequestID, Now: time.Now().UTC(),
			})
			if err != nil {
				return ProviderReversalResult{}, err
			}
			result.Account, err = disarmAutoTopUpAfterSettlementReversalTx(ctx, tx, updated)
			if err != nil {
				return ProviderReversalResult{}, err
			}
			result.Entry = &entry
			ledgerID = entry.ID
		}
		evidence := allocation.evidenceSHA
		if resolutionSHA != "" {
			evidence = resolutionSHA
		}
		if _, err := tx.Exec(ctx, `INSERT INTO billing_provider_reversal_allocations(event_id,account_id,period_id,disposition,ledger_entry_id,evidence_sha256)
			VALUES ($1,$2,$3,$4,$5,$6)`, eventID, account.ID, allocation.periodID, allocation.disposition, ledgerID, evidence); err != nil {
			return ProviderReversalResult{}, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_audit_events(organization_id,event_type,actor_type,actor_id,subject_type,subject_id,request_id,payload)
		VALUES ($1,'billing.provider_reversal.allocated','service','provider_reconciliation','provider_reversal',$2,$3,
		jsonb_build_object('disposition',$4::text,'review_reason',$5::text,'resolution_evidence_sha256',$6::text))`,
		account.OrganizationID, eventID, in.RequestID, result.Disposition, result.ReviewReason, resolutionSHA); err != nil {
		return ProviderReversalResult{}, err
	}
	return result, nil
}

func getProviderReversalResultTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount, eventID string) (ProviderReversalResult, error) {
	result := ProviderReversalResult{EventID: eventID, Account: account}
	var ledgerID *string
	err := tx.QueryRow(ctx, `SELECT COALESCE(a.disposition,'review'),COALESCE(a.period_id::text,''),
		CASE WHEN a.event_id IS NULL THEN COALESCE(r.reason_code,'') ELSE '' END,a.ledger_entry_id::text
		FROM billing_provider_reversal_events e LEFT JOIN billing_provider_reversal_allocations a ON a.event_id=e.id
		LEFT JOIN billing_provider_reversal_reviews r ON r.event_id=e.id WHERE e.id=$1 AND e.account_id=$2`, eventID, account.ID).
		Scan(&result.Disposition, &result.PeriodID, &result.ReviewReason, &ledgerID)
	if err != nil {
		return ProviderReversalResult{}, mapNotFound(err)
	}
	if ledgerID != nil {
		entry, err := scanLedgerEntry(tx.QueryRow(ctx, `SELECT `+ledgerColumns+` FROM balance_ledger_entries WHERE id=$1 AND account_id=$2`, *ledgerID, account.ID))
		if err != nil {
			return ProviderReversalResult{}, err
		}
		result.Entry = &entry
	}
	return result, nil
}

type ResolveProviderReversalInput struct{ AccountID, EventID, EvidenceSHA256, RequestID string }

// Only an audited operator/reconciler may invoke resolution after obtaining
// evidence. Resolution reruns attribution/amount checks; it cannot select a
// different owner or force unknown liability into the current spendable balance.
func (s *Store) ResolveProviderReversal(ctx context.Context, in ResolveProviderReversalInput) (ProviderReversalResult, error) {
	if !canonicalUUID(in.AccountID) || !canonicalUUID(in.EventID) || !validLowerSHA256(in.EvidenceSHA256) || !required(in.RequestID) {
		return ProviderReversalResult{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ProviderReversalResult{}, err
	}
	defer tx.Rollback(ctx)
	account, err := getAccountForUpdate(ctx, tx, in.AccountID)
	if err != nil {
		return ProviderReversalResult{}, err
	}
	var original RecordProviderReversalInput
	original.AccountID = in.AccountID
	original.RequestID = in.RequestID
	err = tx.QueryRow(ctx, `SELECT e.provider,e.event_reference,e.original_intent_id::text,e.amount_minor,e.currency,e.reason,e.provider_payload_sha256
		FROM billing_provider_reversal_events e JOIN billing_provider_reversal_reviews r ON r.event_id=e.id WHERE e.id=$1 AND e.account_id=$2`, in.EventID, in.AccountID).
		Scan(&original.Provider, &original.EventReference, &original.OriginalIntentID, &original.AmountMinor, &original.Currency, &original.Reason, &original.ProviderPayloadSHA256)
	if err != nil {
		return ProviderReversalResult{}, mapNotFound(err)
	}
	var previousEvidence string
	err = tx.QueryRow(ctx, `SELECT evidence_sha256 FROM billing_provider_reversal_allocations WHERE event_id=$1`, in.EventID).Scan(&previousEvidence)
	if err == nil {
		if previousEvidence != in.EvidenceSHA256 {
			return ProviderReversalResult{}, ErrIdempotencyConflict
		}
		result, err := getProviderReversalResultTx(ctx, tx, account, in.EventID)
		result.Duplicate = true
		return result, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ProviderReversalResult{}, err
	}
	result, err := allocateProviderReversalTx(ctx, tx, account, in.EventID, original, in.EvidenceSHA256)
	if err != nil {
		return ProviderReversalResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProviderReversalResult{}, err
	}
	return result, nil
}

type BindHistoricalPaymentResponsibilityInput struct {
	AccountID, IntentID, PeriodID, EvidenceSHA256, ReviewerUserID, RequestID string
}

// BindHistoricalPaymentResponsibility is a migration/operator boundary, not a
// tenant API. Historical liability requires reviewed evidence and never defaults
// to today's owner. An existing binding cannot be reassigned, including on retry.
func (s *Store) BindHistoricalPaymentResponsibility(ctx context.Context, in BindHistoricalPaymentResponsibilityInput) error {
	if !canonicalUUID(in.AccountID) || !canonicalUUID(in.IntentID) || !canonicalUUID(in.PeriodID) ||
		!canonicalUUID(in.ReviewerUserID) || !validLowerSHA256(in.EvidenceSHA256) || !required(in.RequestID) {
		return ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	account, err := getAccountForUpdate(ctx, tx, in.AccountID)
	if err != nil {
		return err
	}
	var periodID, evidence string
	err = tx.QueryRow(ctx, `SELECT period_id::text,evidence_sha256 FROM billing_payment_responsibility WHERE intent_id=$1 AND account_id=$2`, in.IntentID, in.AccountID).Scan(&periodID, &evidence)
	if err == nil {
		if periodID != in.PeriodID || evidence != in.EvidenceSHA256 {
			return ErrIdempotencyConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	command, err := tx.Exec(ctx, `INSERT INTO billing_payment_responsibility(intent_id,account_id,period_id,evidence_sha256,provenance)
		SELECT i.id,i.account_id,p.id,$4,'reviewed_migration' FROM payment_intents i
		JOIN billing_responsibility_periods p ON p.account_id=i.account_id
		WHERE i.id=$1 AND i.account_id=$2 AND p.id=$3`, in.IntentID, in.AccountID, in.PeriodID, in.EvidenceSHA256)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrNotFound
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_audit_events(organization_id,event_type,actor_type,actor_id,subject_type,subject_id,request_id,payload)
		VALUES ($1,'billing.payment_responsibility.reviewed','user',$2,'payment_intent',$3,$4,
		jsonb_build_object('period_id',$5::text,'evidence_sha256',$6::text))`,
		account.OrganizationID, in.ReviewerUserID, in.IntentID, in.RequestID, in.PeriodID, in.EvidenceSHA256)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
