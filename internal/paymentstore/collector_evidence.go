package paymentstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/jackc/pgx/v5"
)

// ReconcileSettlementInput binds a producer-authenticated usage horizon to the
// Billing state captured before the producer was queried. Record* rechecks the
// complete local state digest again before accepting this evidence.
type ReconcileSettlementInput struct {
	OrganizationID           string
	State                    SettlementState
	CoveredThrough           time.Time
	ProducerCheckpointSHA256 string
}

type ReconciledSettlementEvidence struct {
	Financial                billing.FinancialEvidence
	UsageCheckpointSHA256    string
	InvoiceCheckpointSHA256  string
	ProviderCheckpointSHA256 string
}

type HandoffCollectorTarget struct {
	Scope        HandoffScope
	SourceUserID string
	Cutoff       time.Time
}

func collectorCheckpoint(domain string, fields ...string) string {
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	_ = encoder.Encode(domain)
	for _, field := range fields {
		_ = encoder.Encode(field)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// ReconcileSettlementEvidence independently checks Billing's authoritative
// invoice and provider state after a trusted producer has proved its delivery
// horizon. Empty Billing tables are sufficient only when paired with that
// external checkpoint; they never manufacture usage completeness by themselves.
func (s *Store) ReconcileSettlementEvidence(ctx context.Context, in ReconcileSettlementInput) (ReconciledSettlementEvidence, error) {
	if !canonicalUUID(in.OrganizationID) || !validLowerSHA256(in.State.SHA256) || !validLowerSHA256(in.ProducerCheckpointSHA256) ||
		in.CoveredThrough.IsZero() || !in.CoveredThrough.Equal(databaseTime(in.CoveredThrough)) {
		return ReconciledSettlementEvidence{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ReconciledSettlementEvidence{}, err
	}
	defer tx.Rollback(ctx)
	account, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM commercial_accounts WHERE organization_id=$1 AND currency='TWD'`, in.OrganizationID))
	if err != nil {
		return ReconciledSettlementEvidence{}, err
	}
	state, err := captureSettlementStateTx(ctx, tx, account)
	if err != nil {
		return ReconciledSettlementEvidence{}, err
	}
	if state.SHA256 != in.State.SHA256 {
		return ReconciledSettlementEvidence{}, ErrSettlementEvidenceStale
	}

	var usageState, invoiceState, providerState string
	var usageSettled, invoicesReconciled, providerWorkReconciled bool
	err = tx.QueryRow(ctx, `WITH
	usage AS (SELECT usage_id,service_code,metric_code,quantity,quantity_scale,unit,window_start,window_end,source,source_sha256
		FROM billing_usage_facts WHERE organization_id=$1 AND window_start<$3),
	periods AS (SELECT id,state,version,period_start,period_end,usage_locked_at,closed_at,pricing_version_id
		FROM billing_periods WHERE organization_id=$1 AND period_start<$3),
	invoices AS (SELECT id,state,version,total_minor,amount_due_minor,amount_settled_minor,period_id,period_start,period_end
		FROM billing_invoices WHERE account_id=$2 AND period_start<$3),
	settlements AS (SELECT l.invoice_id,l.ledger_entry_id,l.state,l.attempt_count FROM invoice_settlement_links l JOIN invoices i ON i.id=l.invoice_id),
	usage_links AS (SELECT i.id AS invoice_id,i.state,l.usage_fact_refs FROM billing_invoice_lines l JOIN invoices i ON i.id=l.invoice_id),
	intents AS (SELECT id,state,amount_minor,currency,provider,updated_at,completed_at FROM payment_intents WHERE account_id=$2),
	jobs AS (SELECT j.id,j.intent_id,j.reason,j.status,j.updated_at FROM payment_reconciliation_jobs j JOIN intents i ON i.id=j.intent_id),
	attempts AS (SELECT a.id,a.intent_id,a.operation,a.normalized_result,a.completed_at FROM payment_attempts a JOIN intents i ON i.id=a.intent_id),
	setups AS (SELECT id,state,invalidated_by_handoff,invalidated_by_closure,updated_at FROM payment_method_setup_sessions WHERE account_id=$2),
	webhooks AS (SELECT w.id,w.intent_id,w.processing_state,w.payload_sha256 FROM payment_webhook_inbox w JOIN intents i ON i.id=w.intent_id),
	reversals AS (SELECT e.id,e.request_sha256,EXISTS(SELECT 1 FROM billing_provider_reversal_allocations a WHERE a.event_id=e.id) AS allocated
		FROM billing_provider_reversal_events e WHERE e.account_id=$2)
	SELECT
		jsonb_build_object('covered_through',$3::timestamptz,'usage',(SELECT jsonb_agg(to_jsonb(u) ORDER BY u.usage_id) FROM usage u),
			'links',(SELECT jsonb_agg(to_jsonb(l) ORDER BY l.invoice_id) FROM usage_links l))::text,
		jsonb_build_object('covered_through',$3::timestamptz,'periods',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM periods p),
			'invoices',(SELECT jsonb_agg(to_jsonb(i) ORDER BY i.id) FROM invoices i),'settlements',(SELECT jsonb_agg(to_jsonb(s) ORDER BY s.invoice_id) FROM settlements s))::text,
		jsonb_build_object('intents',(SELECT jsonb_agg(to_jsonb(i) ORDER BY i.id) FROM intents i),'jobs',(SELECT jsonb_agg(to_jsonb(j) ORDER BY j.id) FROM jobs j),
			'attempts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM attempts a),'setups',(SELECT jsonb_agg(to_jsonb(s) ORDER BY s.id) FROM setups s),
			'webhooks',(SELECT jsonb_agg(to_jsonb(w) ORDER BY w.id) FROM webhooks w),'reversals',(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id) FROM reversals r))::text,
		NOT EXISTS(SELECT 1 FROM usage u WHERE u.window_end>$3 OR NOT EXISTS(
			SELECT 1 FROM usage_links l WHERE l.state='settled' AND l.usage_fact_refs ? u.usage_id)),
		NOT EXISTS(SELECT 1 FROM periods p WHERE p.state<>'closed') AND
		NOT EXISTS(SELECT 1 FROM invoices i WHERE i.state NOT IN ('settled','void') OR i.amount_due_minor<>0 OR
			(i.total_minor>0 AND NOT EXISTS(SELECT 1 FROM settlements s WHERE s.invoice_id=i.id AND s.state='posted' AND s.ledger_entry_id IS NOT NULL))),
		NOT EXISTS(SELECT 1 FROM intents i WHERE i.state IN ('created','processing','authorized','requires_action','unknown')) AND
		NOT EXISTS(SELECT 1 FROM jobs j WHERE j.status<>'completed') AND
		NOT EXISTS(SELECT 1 FROM attempts a WHERE a.completed_at IS NULL) AND
		NOT EXISTS(SELECT 1 FROM setups s WHERE s.state IN ('created','requires_action','unknown')) AND
		NOT EXISTS(SELECT 1 FROM webhooks w WHERE w.processing_state<>'processed') AND
		NOT EXISTS(SELECT 1 FROM reversals r WHERE NOT r.allocated)`, in.OrganizationID, account.ID, in.CoveredThrough).
		Scan(&usageState, &invoiceState, &providerState, &usageSettled, &invoicesReconciled, &providerWorkReconciled)
	if err != nil {
		return ReconciledSettlementEvidence{}, err
	}

	out := ReconciledSettlementEvidence{Financial: state.Financial}
	out.Financial.UsageSettled = usageSettled && invoicesReconciled
	out.Financial.InvoicesReconciled = invoicesReconciled
	out.Financial.ProviderWorkReconciled = providerWorkReconciled
	if out.Financial.UsageSettled {
		out.UsageCheckpointSHA256 = collectorCheckpoint("billing-settled-usage-v1", in.ProducerCheckpointSHA256, usageState, in.State.SHA256)
	}
	if invoicesReconciled {
		out.InvoiceCheckpointSHA256 = collectorCheckpoint("billing-reconciled-invoices-v1", invoiceState, in.State.SHA256)
	}
	if providerWorkReconciled {
		out.ProviderCheckpointSHA256 = collectorCheckpoint("billing-reconciled-provider-work-v1", providerState, in.State.SHA256)
	}
	if err := tx.Commit(ctx); err != nil {
		return ReconciledSettlementEvidence{}, err
	}
	return out, nil
}

func (s *Store) ListCloudPreflightScopes(ctx context.Context, afterOrganizationID string, limit int) ([]CloudPreflightScope, error) {
	if limit < 1 || limit > 500 || afterOrganizationID != "" && !canonicalUUID(afterOrganizationID) {
		return nil, ErrConflict
	}
	rows, err := s.db.Query(ctx, `SELECT a.organization_id::text,r.owner_user_id::text,r.ownership_version
		FROM commercial_accounts a JOIN billing_responsibility_periods r ON r.account_id=a.id AND r.effective_until IS NULL
		WHERE a.state<>'closed' AND ($1='' OR a.organization_id::text>$1) ORDER BY a.organization_id LIMIT $2`, afterOrganizationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CloudPreflightScope, 0, limit)
	for rows.Next() {
		var scope CloudPreflightScope
		if err := rows.Scan(&scope.OrganizationID, &scope.OwnerUserID, &scope.OwnershipVersion); err != nil {
			return nil, err
		}
		out = append(out, scope)
	}
	return out, rows.Err()
}

func (s *Store) ListHandoffCollectorTargets(ctx context.Context, limit int) ([]HandoffCollectorTarget, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrConflict
	}
	rows, err := s.db.Query(ctx, `SELECT a.organization_id::text,h.id::text,h.ownership_version,h.source_user_id::text,h.cutoff
		FROM billing_ownership_handoffs h JOIN commercial_accounts a ON a.id=h.account_id
		WHERE h.phase IN ('preparing','prepared') ORDER BY h.updated_at,h.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]HandoffCollectorTarget, 0, limit)
	for rows.Next() {
		var target HandoffCollectorTarget
		if err := rows.Scan(&target.Scope.OrganizationID, &target.Scope.OperationID, &target.Scope.OwnershipVersion, &target.SourceUserID, &target.Cutoff); err != nil {
			return nil, err
		}
		out = append(out, target)
	}
	return out, rows.Err()
}

func IsCollectorRetryable(err error) bool {
	return err != nil && (errors.Is(err, ErrSettlementEvidenceStale) || errors.Is(err, ErrOwnershipVersionConflict) || errors.Is(err, ErrConflict))
}
