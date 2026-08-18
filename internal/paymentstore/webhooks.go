package paymentstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

type RecordWebhookInput struct {
	Provider           string
	PayloadSHA256      string
	VerificationResult string
	Event              payment.WebhookEvent
	ReceivedAt         time.Time
}

type RecordWebhookResult struct {
	Receipt   payment.WebhookReceipt `json:"receipt"`
	Duplicate bool                   `json:"duplicate"`
}

func (s *Store) RecordWebhook(ctx context.Context, in RecordWebhookInput) (RecordWebhookResult, error) {
	provider := payment.NormalizeProvider(in.Provider)
	if provider == "" || !validSHA256(in.PayloadSHA256) ||
		(in.VerificationResult != "verified" && in.VerificationResult != "rejected") {
		return RecordWebhookResult{}, ErrConflict
	}
	if in.ReceivedAt.IsZero() {
		in.ReceivedAt = time.Now().UTC()
	} else {
		in.ReceivedAt = in.ReceivedAt.UTC()
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return RecordWebhookResult{}, err
	}
	defer tx.Rollback(ctx)

	processingState := "quarantined"
	var processedAt any = in.ReceivedAt
	var intent payment.PaymentIntent
	intentID := ""
	if in.VerificationResult == "verified" {
		if !required(in.Event.MerchantOrderReference) || in.Event.AmountMinor <= 0 ||
			payment.ValidateProviderResult(payment.ProviderResult{State: in.Event.State}) != nil {
			return RecordWebhookResult{}, ErrConflict
		}

		var accountID string
		if err := tx.QueryRow(ctx, `
			SELECT account_id::text
			FROM payment_intents
			WHERE provider = $1 AND merchant_order_reference = $2
		`, provider, strings.TrimSpace(in.Event.MerchantOrderReference)).Scan(&accountID); err != nil {
			return RecordWebhookResult{}, mapNotFound(err)
		}
		if _, err := getAccountForUpdate(ctx, tx, accountID); err != nil {
			return RecordWebhookResult{}, err
		}
		intent, err = scanIntent(tx.QueryRow(ctx, `
			SELECT `+intentColumns+`
			FROM payment_intents
			WHERE provider = $1 AND merchant_order_reference = $2
			FOR UPDATE
		`, provider, strings.TrimSpace(in.Event.MerchantOrderReference)))
		if err != nil {
			return RecordWebhookResult{}, err
		}
		if intent.AmountMinor != in.Event.AmountMinor || intent.Currency != in.Event.Currency {
			return RecordWebhookResult{}, ErrConflict
		}
		providerReference := strings.TrimSpace(in.Event.ProviderTransactionReference)
		if intent.ProviderTransactionReference != "" && providerReference != "" && intent.ProviderTransactionReference != providerReference {
			return RecordWebhookResult{}, ErrConflict
		}
		if intent.ProviderTransactionReference == "" && providerReference != "" {
			intent, err = scanIntent(tx.QueryRow(ctx, `
				UPDATE payment_intents
				SET provider_transaction_reference = $2
				WHERE id = $1
				RETURNING `+intentColumns,
				intent.ID, providerReference,
			))
			if err != nil {
				return RecordWebhookResult{}, err
			}
		}
		intentID = intent.ID
		processingState = "scheduled"
		processedAt = nil
	}

	safeSummary, err := json.Marshal(sanitizeWebhookSummary(in.Event))
	if err != nil {
		return RecordWebhookResult{}, err
	}
	receipt, insertErr := scanWebhookReceipt(tx.QueryRow(ctx, `
		INSERT INTO payment_webhook_inbox (
			provider, provider_event_reference, payload_sha256,
			verification_result, intent_id, normalized_event_type,
			processing_state, redacted_summary, received_at, processed_at
		)
		VALUES (
			$1, NULLIF($2, ''), $3, $4, NULLIF($5, '')::uuid,
			NULLIF($6, ''), $7, $8::jsonb, $9, $10
		)
		ON CONFLICT DO NOTHING
		RETURNING `+webhookReceiptColumns,
		provider, strings.TrimSpace(in.Event.ProviderEventReference), strings.ToLower(in.PayloadSHA256),
		in.VerificationResult, intentID, strings.TrimSpace(in.Event.EventType),
		processingState, safeSummary, in.ReceivedAt, processedAt,
	))
	if insertErr != nil && !errors.Is(insertErr, ErrNotFound) {
		return RecordWebhookResult{}, insertErr
	}
	if errors.Is(insertErr, ErrNotFound) {
		receipt, err = findWebhookDuplicate(ctx, tx, provider, strings.TrimSpace(in.Event.ProviderEventReference), strings.ToLower(in.PayloadSHA256))
		if err != nil {
			return RecordWebhookResult{}, err
		}
		if receipt.PayloadSHA256 != strings.ToLower(in.PayloadSHA256) || receipt.VerificationResult != in.VerificationResult {
			return RecordWebhookResult{}, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return RecordWebhookResult{}, err
		}
		return RecordWebhookResult{Receipt: receipt, Duplicate: true}, nil
	}

	if in.VerificationResult == "verified" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment_reconciliation_jobs (intent_id, reason, status, due_at)
			VALUES ($1, 'webhook', 'pending', $2)
			ON CONFLICT DO NOTHING
		`, intent.ID, in.ReceivedAt); err != nil {
			return RecordWebhookResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RecordWebhookResult{}, err
	}
	return RecordWebhookResult{Receipt: receipt}, nil
}

func findWebhookDuplicate(ctx context.Context, tx pgx.Tx, provider, eventReference, digest string) (payment.WebhookReceipt, error) {
	return scanWebhookReceipt(tx.QueryRow(ctx, `
		SELECT `+webhookReceiptColumns+`
		FROM payment_webhook_inbox
		WHERE provider = $1
		  AND (payload_sha256 = $3 OR ($2 <> '' AND provider_event_reference = $2))
		ORDER BY created_at
		LIMIT 1
	`, provider, eventReference, digest))
}

func sanitizeWebhookSummary(event payment.WebhookEvent) map[string]string {
	return map[string]string{
		"event_type":    truncateSafe(strings.TrimSpace(event.EventType), 64),
		"provider_code": payment.NormalizeProviderCode(event.ProviderCode),
		"state":         string(event.State),
	}
}

func truncateSafe(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
