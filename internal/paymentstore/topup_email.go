package paymentstore

import (
	"context"
	"time"
)

type TopUpEmail struct {
	IntentID, OrganizationID, RecipientEmail, RecipientName string
	Currency, Provider                                      string
	AmountMinor                                             int64
	PaymentCreatedAt, PaymentCompletedAt                    time.Time
	AttemptCount                                            int
}

// ClaimTopUpEmails leases due messages. A worker crash leaves them claimable again.
func (s *Store) ClaimTopUpEmails(ctx context.Context, now time.Time, limit int) ([]TopUpEmail, error) {
	if limit < 1 || limit > 100 {
		limit = 25
	}
	rows, err := s.db.Query(ctx, `
		WITH ready AS (
			SELECT intent_id FROM billing_topup_email_outbox
			WHERE (status = 'pending' AND available_at <= $1)
			   OR (status = 'sending' AND lease_until <= $1)
			ORDER BY available_at, created_at
			FOR UPDATE SKIP LOCKED LIMIT $2
		), claimed AS (
			UPDATE billing_topup_email_outbox AS outbox
			SET status = 'sending', lease_until = $3, attempt_count = attempt_count + 1, updated_at = $1
			FROM ready WHERE outbox.intent_id = ready.intent_id
			RETURNING outbox.intent_id::text, outbox.organization_id::text, outbox.recipient_email,
				outbox.recipient_name, outbox.amount_minor, outbox.currency, outbox.provider,
				outbox.payment_created_at, outbox.payment_completed_at, outbox.attempt_count
		)
		SELECT * FROM claimed
	`, now.UTC(), limit, now.UTC().Add(5*time.Minute))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TopUpEmail, 0)
	for rows.Next() {
		var item TopUpEmail
		if err := rows.Scan(&item.IntentID, &item.OrganizationID, &item.RecipientEmail, &item.RecipientName,
			&item.AmountMinor, &item.Currency, &item.Provider, &item.PaymentCreatedAt,
			&item.PaymentCompletedAt, &item.AttemptCount); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) MarkTopUpEmailSent(ctx context.Context, intentID string, attempt int, now time.Time) (bool, error) {
	result, err := s.db.Exec(ctx, `
		UPDATE billing_topup_email_outbox SET status='sent', sent_at=$3, lease_until=NULL, updated_at=$3
		WHERE intent_id=$1 AND status='sending' AND attempt_count=$2
	`, intentID, attempt, now.UTC())
	return result.RowsAffected() == 1, err
}

func (s *Store) RetryTopUpEmail(ctx context.Context, intentID string, attempt int, now time.Time) (bool, error) {
	delay := 30 * time.Second
	for i := 1; i < attempt && delay < time.Hour; i++ {
		delay *= 2
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	result, err := s.db.Exec(ctx, `
		UPDATE billing_topup_email_outbox SET status='pending', available_at=$3, lease_until=NULL, updated_at=$4
		WHERE intent_id=$1 AND status='sending' AND attempt_count=$2
	`, intentID, attempt, now.UTC().Add(delay), now.UTC())
	return result.RowsAffected() == 1, err
}
