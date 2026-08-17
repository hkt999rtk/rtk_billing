package billingstore

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

type ActivityFilter struct {
	State     billing.ActivityState
	Type      string
	Reference string
	Limit     int
	Offset    int
}

type ActivitySummary struct {
	ActionRequired int `json:"action_required"`
	Processing     int `json:"processing"`
	Completed      int `json:"completed"`
}

type ActivityPage struct {
	Activities []billing.Activity `json:"activities"`
	Summary    ActivitySummary    `json:"summary"`
	Page       Page               `json:"pagination"`
}

func (s *Store) ListActivities(ctx context.Context, organizationID string, filter ActivityFilter) (ActivityPage, error) {
	if filter.Limit <= 0 || filter.Limit > 100 {
		filter.Limit = 25
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	activities, err := s.listStoredActivities(ctx, organizationID)
	if err != nil {
		return ActivityPage{}, err
	}
	paymentActivities, err := s.listPaymentActivities(ctx, organizationID)
	if err != nil {
		return ActivityPage{}, err
	}
	activities = append(activities, paymentActivities...)
	filtered := activities[:0]
	for _, activity := range activities {
		if filter.State != "" && activity.State != filter.State {
			continue
		}
		if filter.Type != "" && activity.Type != filter.Type {
			continue
		}
		if strings.TrimSpace(filter.Reference) != "" && activity.CustomerReference != strings.TrimSpace(filter.Reference) {
			continue
		}
		filtered = append(filtered, activity)
	}
	activities = filtered
	sort.SliceStable(activities, func(i, j int) bool {
		if activities[i].OccurredAt.Equal(activities[j].OccurredAt) {
			return activities[i].ID > activities[j].ID
		}
		return activities[i].OccurredAt.After(activities[j].OccurredAt)
	})
	summary := ActivitySummary{}
	for _, activity := range activities {
		switch activity.State {
		case billing.ActivityActionRequired:
			summary.ActionRequired++
		case billing.ActivityProcessing, billing.ActivityPendingReconciliation:
			summary.Processing++
		case billing.ActivityCompleted:
			summary.Completed++
		}
	}
	total := len(activities)
	start := filter.Offset
	if start > total {
		start = total
	}
	end := start + filter.Limit
	if end > total {
		end = total
	}
	return ActivityPage{Activities: activities[start:end], Summary: summary, Page: Page{Limit: filter.Limit, Offset: filter.Offset, Total: total}}, nil
}

func (s *Store) GetActivity(ctx context.Context, organizationID, activityID string) (billing.Activity, error) {
	activity, err := s.getPaymentActivity(ctx, organizationID, activityID)
	if err == nil {
		return activity, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return billing.Activity{}, err
	}
	activity, err = s.getStoredActivity(ctx, organizationID, activityID)
	if err != nil {
		return billing.Activity{}, err
	}
	if activity.Type == "invoice" {
		var invoiceID, invoiceNumber, ledgerID string
		var issuedAt time.Time
		err := s.db.QueryRow(ctx, `
			SELECT invoices.id::text, invoices.invoice_number, invoices.issued_at,
			       COALESCE(links.ledger_entry_id::text, '')
			FROM billing_invoices AS invoices
			LEFT JOIN invoice_settlement_links AS links ON links.invoice_id = invoices.id
			WHERE invoices.organization_id = $1 AND invoices.id = (
			  SELECT resource_id FROM billing_activity_events WHERE id = $2 AND organization_id = $1
			)
		`, organizationID, activityID).Scan(&invoiceID, &invoiceNumber, &issuedAt, &ledgerID)
		if err != nil {
			return billing.Activity{}, mapNotFound(err)
		}
		activity.Steps = append(activity.Steps, billing.ActivityStep{
			Kind: "invoice", State: "issued", OccurredAt: issuedAt,
			CustomerReference: invoiceNumber, MessageKey: "billing.invoice.issued",
		})
		if ledgerID != "" {
			activity.Steps = append(activity.Steps, billing.ActivityStep{
				Kind: "balance_ledger", State: "debit", OccurredAt: activity.UpdatedAt,
				CustomerReference: "ledger-" + ledgerID, MessageKey: "billing.balance.debited",
			})
		}
	}
	return activity, nil
}

func (s *Store) listStoredActivities(ctx context.Context, organizationID string) ([]billing.Activity, error) {
	rows, err := s.db.Query(ctx, storedActivitySelect+` WHERE organization_id = $1 ORDER BY occurred_at DESC LIMIT 500`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]billing.Activity, 0)
	for rows.Next() {
		activity, err := scanStoredActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, activity)
	}
	return out, rows.Err()
}

func (s *Store) getStoredActivity(ctx context.Context, organizationID, activityID string) (billing.Activity, error) {
	return scanStoredActivity(s.db.QueryRow(ctx, storedActivitySelect+` WHERE organization_id = $1 AND id = $2`, organizationID, activityID))
}

const storedActivitySelect = `
	SELECT id::text, customer_reference, activity_type, state, currency, amount_minor, balance_effect,
	       action, COALESCE(message_key, ''), retry_scheduled, next_retry_at, occurred_at, updated_at
	FROM billing_activity_events`

func scanStoredActivity(row rowScanner) (billing.Activity, error) {
	var out billing.Activity
	err := row.Scan(&out.ID, &out.CustomerReference, &out.Type, &out.State, &out.Currency, &out.AmountMinor,
		&out.BalanceEffect, &out.Action, &out.MessageKey, &out.RetryScheduled, &out.NextRetryAt,
		&out.OccurredAt, &out.UpdatedAt)
	if err != nil {
		return billing.Activity{}, mapNotFound(err)
	}
	out.Steps = []billing.ActivityStep{}
	return out, nil
}

func (s *Store) listPaymentActivities(ctx context.Context, organizationID string) ([]billing.Activity, error) {
	rows, err := s.db.Query(ctx, paymentActivitySelect+` WHERE accounts.organization_id = $1 ORDER BY intents.created_at DESC LIMIT 500`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]billing.Activity, 0)
	for rows.Next() {
		activity, err := scanPaymentActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, activity)
	}
	return out, rows.Err()
}

func (s *Store) getPaymentActivity(ctx context.Context, organizationID, intentID string) (billing.Activity, error) {
	activity, err := scanPaymentActivity(s.db.QueryRow(ctx, paymentActivitySelect+` WHERE accounts.organization_id = $1 AND intents.id = $2`, organizationID, intentID))
	if err != nil {
		return billing.Activity{}, err
	}
	activity.Steps = append(activity.Steps, billing.ActivityStep{
		Kind: "payment_intent", State: "created", OccurredAt: activity.OccurredAt,
		CustomerReference: activity.CustomerReference, MessageKey: "billing.payment_intent.created",
	})
	rows, err := s.db.Query(ctx, `
		SELECT id::text, operation, attempt_number, started_at, completed_at, normalized_result,
		       next_reconciliation_at
		FROM payment_attempts WHERE intent_id = $1 ORDER BY attempt_number, created_at
	`, intentID)
	if err != nil {
		return billing.Activity{}, err
	}
	for rows.Next() {
		var id, operation, result string
		var number int
		var started time.Time
		var completed, next *time.Time
		if err := rows.Scan(&id, &operation, &number, &started, &completed, &result, &next); err != nil {
			rows.Close()
			return billing.Activity{}, err
		}
		occurred := started
		if completed != nil {
			occurred = *completed
		}
		activity.Steps = append(activity.Steps, billing.ActivityStep{
			Kind: "payment_attempt", State: result, OccurredAt: occurred,
			CustomerReference: "attempt-" + id, MessageKey: "billing.payment_attempt." + result,
		})
		if next != nil && (activity.NextRetryAt == nil || next.Before(*activity.NextRetryAt)) {
			copy := next.UTC()
			activity.NextRetryAt = &copy
		}
	}
	rows.Close()
	var jobID, jobStatus string
	var jobTime time.Time
	err = s.db.QueryRow(ctx, `
		SELECT id::text, status, updated_at FROM payment_reconciliation_jobs
		WHERE intent_id = $1 ORDER BY updated_at DESC LIMIT 1
	`, intentID).Scan(&jobID, &jobStatus, &jobTime)
	if err == nil {
		activity.Steps = append(activity.Steps, billing.ActivityStep{
			Kind: "reconciliation", State: jobStatus, OccurredAt: jobTime,
			CustomerReference: "reconciliation-" + jobID, MessageKey: "billing.reconciliation." + jobStatus,
		})
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return billing.Activity{}, err
	}
	var ledgerID, direction string
	var ledgerAt time.Time
	err = s.db.QueryRow(ctx, `
		SELECT id::text, direction, created_at FROM balance_ledger_entries
		WHERE account_id = (SELECT account_id FROM payment_intents WHERE id = $1)
		  AND idempotency_scope = 'payment_intent' AND idempotency_key = $1
		ORDER BY created_at DESC LIMIT 1
	`, intentID).Scan(&ledgerID, &direction, &ledgerAt)
	if err == nil {
		activity.Steps = append(activity.Steps, billing.ActivityStep{
			Kind: "balance_ledger", State: direction, OccurredAt: ledgerAt,
			CustomerReference: "ledger-" + ledgerID, MessageKey: "billing.balance." + direction,
		})
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return billing.Activity{}, err
	} else if activity.State == billing.ActivityFailed || activity.State == billing.ActivityActionRequired {
		activity.Steps = append(activity.Steps, billing.ActivityStep{
			Kind: "balance_ledger", State: "none", OccurredAt: activity.UpdatedAt,
			CustomerReference: activity.CustomerReference, MessageKey: "billing.balance.unchanged",
		})
	}
	return activity, nil
}

const paymentActivitySelect = `
	SELECT intents.id::text, intents.correlation_id, intents.reason, intents.state, intents.currency,
	       intents.amount_minor, intents.created_at, intents.updated_at,
	       COALESCE(methods.card_brand, methods.provider, ''), COALESCE(methods.last_four, ''),
	       COALESCE(policy.enabled, false), COALESCE(policy.consecutive_failure_count, 0)
	FROM payment_intents AS intents
	JOIN commercial_accounts AS accounts ON accounts.id = intents.account_id
	LEFT JOIN payment_methods AS methods ON methods.id = intents.payment_method_id
	LEFT JOIN auto_topup_policies AS policy ON policy.account_id = intents.account_id`

func scanPaymentActivity(row rowScanner) (billing.Activity, error) {
	var out billing.Activity
	var reason, providerState, brand, lastFour string
	var policyEnabled bool
	var failures int
	err := row.Scan(&out.ID, &out.CustomerReference, &reason, &providerState, &out.Currency, &out.AmountMinor,
		&out.OccurredAt, &out.UpdatedAt, &brand, &lastFour, &policyEnabled, &failures)
	if err != nil {
		return billing.Activity{}, mapNotFound(err)
	}
	if reason == "auto_top_up" {
		out.Type = "automatic_top_up"
	} else {
		out.Type = "manual_top_up"
	}
	out.Action = "none"
	out.BalanceEffect = "unknown"
	out.MessageKey = "billing.payment." + providerState
	switch providerState {
	case "succeeded":
		out.State = billing.ActivityCompleted
		out.BalanceEffect = "credit"
	case "failed", "canceled":
		out.State = billing.ActivityFailed
		out.BalanceEffect = "none"
		if reason == "auto_top_up" && (!policyEnabled || failures >= 3) {
			out.State = billing.ActivityActionRequired
			out.Action = "update_payment_method"
		}
	case "requires_action":
		out.State = billing.ActivityActionRequired
		out.BalanceEffect = "none"
		out.Action = "update_payment_method"
	case "unknown":
		out.State = billing.ActivityPendingReconciliation
		out.RetryScheduled = true
	case "created", "processing", "authorized":
		out.State = billing.ActivityProcessing
	default:
		out.State = billing.ActivityUnavailable
	}
	if brand != "" {
		out.PaymentMethodLabel = strings.ToUpper(brand)
		if len(lastFour) == 4 {
			out.PaymentMethodLabel += " •••• " + lastFour
		}
	}
	out.Steps = []billing.ActivityStep{}
	return out, nil
}
