package paymentstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

var (
	ErrSettlementEvidenceStale = errors.New("handoff settlement evidence is stale")
	ErrHandoffParticipant      = errors.New("user is not a handoff participant")
	ErrHandoffNotConfirmable   = errors.New("handoff has no current confirmable snapshot")
)

type HandoffScope struct {
	OrganizationID   string
	OperationID      string
	OwnershipVersion int64
}

// SettlementState is internal collector input, not a participant preview. The
// collector must independently reconcile the frozen cutoff/producer manifest;
// local counts and a digest cannot prove completeness by themselves.
type SettlementState struct {
	SHA256    string
	Financial billing.FinancialEvidence
}

// RecordSettlementInput is trusted collector output, never browser data. A true
// completeness flag must reference a durably verified domain checkpoint. The
// digest must be captured BEFORE collecting those checkpoints, so intervening
// local mutations cannot be certified accidentally with an old provider report.
type RecordSettlementInput struct {
	Scope                    HandoffScope
	ReceiptID                string
	StateSHA256              string
	UsageCheckpointSHA256    string
	InvoiceCheckpointSHA256  string
	ProviderCheckpointSHA256 string
	Financial                billing.FinancialEvidence
}

type HandoffBalanceSnapshot struct {
	Version         int64            `json:"version"`
	BalanceMinor    int64            `json:"balance_minor"`
	Currency        billing.Currency `json:"currency"`
	Cutoff          time.Time        `json:"cutoff"`
	SourceConfirmed bool             `json:"source_confirmed"`
	TargetConfirmed bool             `json:"target_confirmed"`
}

type HandoffSettlementStatus struct {
	OperationID string                  `json:"operation_id"`
	Phase       string                  `json:"phase"`
	Snapshot    *HandoffBalanceSnapshot `json:"snapshot,omitempty"`
	Blockers    []string                `json:"blockers"`
}

func validHandoffScope(scope HandoffScope) bool {
	return canonicalUUID(scope.OrganizationID) && canonicalUUID(scope.OperationID) && scope.OwnershipVersion > 0
}

func lockHandoffOperationTx(ctx context.Context, tx pgx.Tx, scope HandoffScope) (payment.CommercialAccount, OwnershipHandoff, error) {
	account, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM commercial_accounts
		WHERE organization_id=$1 AND currency='TWD' FOR UPDATE`, scope.OrganizationID))
	if err != nil {
		return account, OwnershipHandoff{}, err
	}
	op, err := scanHandoff(tx.QueryRow(ctx, `SELECT `+handoffColumns+` FROM billing_ownership_handoffs
		WHERE id=$1 AND account_id=$2 FOR UPDATE`, scope.OperationID, account.ID))
	if err != nil {
		return account, op, err
	}
	if op.OwnershipVersion != scope.OwnershipVersion {
		return account, op, ErrOwnershipVersionConflict
	}
	return account, op, nil
}

func loadHandoffScopeTx(ctx context.Context, tx pgx.Tx, scope HandoffScope) (payment.CommercialAccount, OwnershipHandoff, error) {
	account, op, err := lockHandoffOperationTx(ctx, tx, scope)
	if err != nil {
		return account, op, err
	}
	return account, op, requireSourceResponsibilityTx(ctx, tx, account, op)
}

func requireSourceResponsibilityTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount, op OwnershipHandoff) error {
	var current bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM billing_responsibility_periods
		WHERE account_id=$1 AND owner_user_id=$2 AND ownership_version=$3 AND effective_until IS NULL)`,
		account.ID, op.SourceUserID, op.OwnershipVersion).Scan(&current)
	if err == nil && !current {
		err = ErrOwnershipVersionConflict
	}
	return err
}

func (s *Store) CaptureHandoffSettlementState(ctx context.Context, scope HandoffScope) (SettlementState, error) {
	if !validHandoffScope(scope) {
		return SettlementState{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return SettlementState{}, err
	}
	defer tx.Rollback(ctx)
	account, op, err := loadHandoffScopeTx(ctx, tx, scope)
	if err != nil {
		return SettlementState{}, err
	}
	if op.Phase != "preparing" && op.Phase != "prepared" {
		return SettlementState{}, ErrConflict
	}
	return captureSettlementStateTx(ctx, tx, account)
}

func (s *Store) RecordHandoffSettlement(ctx context.Context, in RecordSettlementInput) (HandoffSettlementStatus, error) {
	if !validHandoffScope(in.Scope) || !canonicalUUID(in.ReceiptID) || !validLowerSHA256(in.StateSHA256) ||
		!validCheckpoint(in.Financial.UsageSettled, in.UsageCheckpointSHA256) ||
		!validCheckpoint(in.Financial.InvoicesReconciled, in.InvoiceCheckpointSHA256) ||
		!validCheckpoint(in.Financial.ProviderWorkReconciled, in.ProviderCheckpointSHA256) {
		return HandoffSettlementStatus{}, ErrConflict
	}
	// Invalid counts are not persisted as an alternate representation of zero.
	for _, count := range []int64{in.Financial.UnpaidInvoiceCount, in.Financial.DebtMinor, in.Financial.PendingPaymentCount,
		in.Financial.PendingRefundCount, in.Financial.OpenDisputeCount, in.Financial.PendingSetupCount, in.Financial.UnresolvedProviderEventCount} {
		if count < 0 {
			return HandoffSettlementStatus{}, ErrConflict
		}
	}
	digest, err := handoffDigest(in)
	if err != nil {
		return HandoffSettlementStatus{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return HandoffSettlementStatus{}, err
	}
	defer tx.Rollback(ctx)
	account, op, err := loadHandoffScopeTx(ctx, tx, in.Scope)
	if err != nil {
		return HandoffSettlementStatus{}, err
	}
	var existingDigest string
	err = tx.QueryRow(ctx, `SELECT request_sha256 FROM billing_handoff_settlement_receipts WHERE id=$1`, in.ReceiptID).Scan(&existingDigest)
	if err == nil {
		if existingDigest != digest {
			return HandoffSettlementStatus{}, ErrIdempotencyConflict
		}
		// Replays report live status, never revive an old snapshot/confirmation.
		return handoffSettlementStatusTx(ctx, tx, account, op)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return HandoffSettlementStatus{}, err
	}
	if op.Phase != "preparing" && op.Phase != "prepared" {
		return HandoffSettlementStatus{}, ErrConflict
	}
	state, err := captureSettlementStateTx(ctx, tx, account)
	if err != nil {
		return HandoffSettlementStatus{}, err
	}
	if state.SHA256 != in.StateSHA256 {
		return HandoffSettlementStatus{}, ErrSettlementEvidenceStale
	}
	// The collector must name this exact balance as well as the digest. Local
	// blockers can only add restrictions, never be overwritten by supplied zeros.
	if !in.Financial.BalanceKnown || in.Financial.BalanceMinor != state.Financial.BalanceMinor || in.Financial.Currency != state.Financial.Currency {
		return HandoffSettlementStatus{}, ErrSettlementEvidenceStale
	}
	evidence := mergeSettlementEvidence(in.Financial, state.Financial)
	blockers := billing.OwnershipTransferBlockers(evidence)
	if account.State == payment.AccountStateClosed {
		blockers = append(blockers, "account_closed")
	}
	op.Version++
	op.Phase = "preparing"
	if len(blockers) == 0 {
		op.Phase = "prepared"
	}
	financialJSON, err := json.Marshal(evidence)
	if err != nil {
		return HandoffSettlementStatus{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_handoff_settlement_receipts
		(id,operation_id,operation_version,request_sha256,state_sha256,usage_checkpoint_sha256,
		invoice_checkpoint_sha256,provider_checkpoint_sha256,financial_evidence)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9::jsonb)`,
		in.ReceiptID, op.ID, op.Version, digest, state.SHA256, in.UsageCheckpointSHA256,
		in.InvoiceCheckpointSHA256, in.ProviderCheckpointSHA256, financialJSON); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return HandoffSettlementStatus{}, ErrIdempotencyConflict
		}
		return HandoffSettlementStatus{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE billing_ownership_handoffs SET phase=$2,version=$3,updated_at=now() WHERE id=$1`,
		op.ID, op.Phase, op.Version); err != nil {
		return HandoffSettlementStatus{}, err
	}
	if len(blockers) == 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO billing_handoff_balance_snapshots
			(operation_id,version,receipt_id,balance_minor,currency,cutoff) VALUES ($1,$2,$3,$4,$5,$6)`,
			op.ID, op.Version, in.ReceiptID, evidence.BalanceMinor, evidence.Currency, op.Cutoff); err != nil {
			return HandoffSettlementStatus{}, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_audit_events
		(organization_id,event_type,actor_type,actor_id,subject_type,subject_id,request_id,payload)
		VALUES ($1,'billing.ownership_handoff.settlement','service','billing_settlement_collector',
		'ownership_handoff',$2,$3,jsonb_build_object('version',$4::bigint,'phase',$5::text,'request_sha256',$6::text))`,
		in.Scope.OrganizationID, op.ID, in.ReceiptID, op.Version, op.Phase, digest); err != nil {
		return HandoffSettlementStatus{}, err
	}
	status, err := handoffSettlementStatusTx(ctx, tx, account, op)
	if err != nil {
		return HandoffSettlementStatus{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HandoffSettlementStatus{}, err
	}
	return status, nil
}

func validCheckpoint(complete bool, digest string) bool {
	if digest == "" {
		return !complete
	}
	return validLowerSHA256(digest)
}

func mergeSettlementEvidence(external, local billing.FinancialEvidence) billing.FinancialEvidence {
	external.BalanceKnown, external.Currency, external.BalanceMinor = local.BalanceKnown, local.Currency, local.BalanceMinor
	external.UnpaidInvoiceCount = max(external.UnpaidInvoiceCount, local.UnpaidInvoiceCount)
	external.DebtMinor = max(external.DebtMinor, local.DebtMinor)
	external.PendingPaymentCount = max(external.PendingPaymentCount, local.PendingPaymentCount)
	external.PendingRefundCount = max(external.PendingRefundCount, local.PendingRefundCount)
	external.PendingSetupCount = max(external.PendingSetupCount, local.PendingSetupCount)
	external.UnresolvedProviderEventCount = max(external.UnresolvedProviderEventCount, local.UnresolvedProviderEventCount)
	return external
}

func (s *Store) GetHandoffSettlementStatus(ctx context.Context, scope HandoffScope) (HandoffSettlementStatus, error) {
	if !validHandoffScope(scope) {
		return HandoffSettlementStatus{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return HandoffSettlementStatus{}, err
	}
	defer tx.Rollback(ctx)
	account, op, err := loadHandoffScopeTx(ctx, tx, scope)
	if err != nil {
		return HandoffSettlementStatus{}, err
	}
	return handoffSettlementStatusTx(ctx, tx, account, op)
}

// Every preview and confirmation derives validity from current state, not merely
// a persisted prepared flag. Immutable old confirmations remain audit evidence but
// do not apply to a replacement snapshot, even if its balance happens to be equal.
func handoffSettlementStatusTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount, op OwnershipHandoff) (HandoffSettlementStatus, error) {
	status := HandoffSettlementStatus{OperationID: op.ID, Phase: op.Phase, Blockers: []string{}}
	if op.Phase != "preparing" && op.Phase != "prepared" {
		status.Blockers = append(status.Blockers, "handoff_not_confirmable")
		return status, nil
	}
	local, err := captureSettlementStateTx(ctx, tx, account)
	if err != nil {
		return status, err
	}
	var stateSHA string
	var encoded []byte
	err = tx.QueryRow(ctx, `SELECT state_sha256,financial_evidence FROM billing_handoff_settlement_receipts
		WHERE operation_id=$1 AND operation_version=$2`, op.ID, op.Version).Scan(&stateSHA, &encoded)
	var evidence billing.FinancialEvidence
	if errors.Is(err, pgx.ErrNoRows) {
		status.Blockers = append(status.Blockers, "settlement_evidence_missing")
	} else if err != nil {
		return status, err
	} else {
		if err := json.Unmarshal(encoded, &evidence); err != nil {
			return status, err
		}
		if stateSHA != local.SHA256 {
			status.Blockers = append(status.Blockers, "settlement_evidence_stale")
		}
	}
	status.Blockers = append(status.Blockers, billing.OwnershipTransferBlockers(mergeSettlementEvidence(evidence, local.Financial))...)
	if account.State == payment.AccountStateClosed {
		status.Blockers = append(status.Blockers, "account_closed")
	}
	if len(status.Blockers) > 0 {
		status.Phase = "preparing"
		return status, nil
	}
	var snapshot HandoffBalanceSnapshot
	err = tx.QueryRow(ctx, `SELECT version,balance_minor,currency,cutoff,
		EXISTS (SELECT 1 FROM billing_handoff_confirmations WHERE operation_id=$1 AND snapshot_version=$2 AND user_id=$3),
		EXISTS (SELECT 1 FROM billing_handoff_confirmations WHERE operation_id=$1 AND snapshot_version=$2 AND user_id=$4)
		FROM billing_handoff_balance_snapshots WHERE operation_id=$1 AND version=$2`,
		op.ID, op.Version, op.SourceUserID, op.TargetUserID).Scan(&snapshot.Version, &snapshot.BalanceMinor,
		&snapshot.Currency, &snapshot.Cutoff, &snapshot.SourceConfirmed, &snapshot.TargetConfirmed)
	if err != nil {
		return status, mapNotFound(err)
	}
	status.Snapshot = &snapshot
	return status, nil
}

type ConfirmHandoffSnapshotInput struct {
	Scope           HandoffScope
	UserID          string
	SnapshotVersion int64
	BalanceMinor    int64
	Currency        billing.Currency
}

// The Account Manager caller authenticates the participant session and checks
// invitation expiry/eligibility. This store independently checks identity, scope,
// phase, ownership version, exact amount and live settlement evidence. It grants
// no general Billing access and does not switch ownership or release any fence.
func (s *Store) ConfirmHandoffSnapshot(ctx context.Context, in ConfirmHandoffSnapshotInput) (HandoffSettlementStatus, error) {
	if !validHandoffScope(in.Scope) || !canonicalUUID(in.UserID) || in.SnapshotVersion < 2 || in.BalanceMinor < 0 || in.Currency != billing.CurrencyTWD {
		return HandoffSettlementStatus{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return HandoffSettlementStatus{}, err
	}
	defer tx.Rollback(ctx)
	account, op, err := loadHandoffScopeTx(ctx, tx, in.Scope)
	if err != nil {
		return HandoffSettlementStatus{}, err
	}
	if in.UserID != op.SourceUserID && in.UserID != op.TargetUserID {
		return HandoffSettlementStatus{}, ErrHandoffParticipant
	}
	status, err := handoffSettlementStatusTx(ctx, tx, account, op)
	if err != nil {
		return status, err
	}
	if status.Snapshot == nil || status.Snapshot.Version != in.SnapshotVersion || status.Snapshot.BalanceMinor != in.BalanceMinor || status.Snapshot.Currency != in.Currency {
		return status, ErrHandoffNotConfirmable
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO billing_handoff_confirmations(operation_id,snapshot_version,user_id)
		VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, op.ID, in.SnapshotVersion, in.UserID)
	if err != nil {
		return status, err
	}
	if inserted.RowsAffected() != 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO billing_audit_events
			(organization_id,event_type,actor_type,actor_id,subject_type,subject_id,request_id,payload)
			VALUES ($1,'billing.ownership_handoff.confirm','user',$2,'ownership_handoff',$3,$3,
			jsonb_build_object('snapshot_version',$4::bigint,'balance_minor',$5::bigint,'currency',$6::text))`,
			in.Scope.OrganizationID, in.UserID, op.ID, in.SnapshotVersion, in.BalanceMinor, in.Currency); err != nil {
			return status, err
		}
	}
	status.Snapshot.SourceConfirmed = status.Snapshot.SourceConfirmed || in.UserID == op.SourceUserID
	status.Snapshot.TargetConfirmed = status.Snapshot.TargetConfirmed || in.UserID == op.TargetUserID
	if err := tx.Commit(ctx); err != nil {
		return HandoffSettlementStatus{}, err
	}
	return status, nil
}

// One SQL statement gives a consistent local observation across independently
// written invoice/usage/provider tables. The digest contains no returned PII.
// Account locking serializes payment mutations; producer cutoffs and coordinated
// commit fences are still required before owner commit (not implemented here).
func captureSettlementStateTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount) (SettlementState, error) {
	var encoded string
	state := SettlementState{Financial: billing.FinancialEvidence{
		BalanceKnown: true, Currency: billing.Currency(account.Currency), BalanceMinor: account.AvailableBalanceMinor,
	}}
	err := tx.QueryRow(ctx, `WITH
	usage AS (SELECT * FROM billing_usage_facts WHERE organization_id=$2),
	periods AS (SELECT id,state,version,period_start,period_end,usage_locked_at,closed_at,pricing_version_id
		FROM billing_periods WHERE organization_id=$2),
	invoices AS (SELECT id,state,version,total_minor,amount_due_minor,amount_settled_minor,period_id
		FROM billing_invoices WHERE account_id=$1),
	settlements AS (SELECT l.invoice_id,l.ledger_entry_id,l.state,l.attempt_count FROM invoice_settlement_links l JOIN invoices i ON i.id=l.invoice_id),
	intents AS (SELECT id,state,amount_minor,currency,updated_at FROM payment_intents WHERE account_id=$1),
	jobs AS (SELECT j.id,j.intent_id,j.reason,j.status FROM payment_reconciliation_jobs j JOIN intents i ON i.id=j.intent_id),
	attempts AS (SELECT a.id,a.intent_id,a.operation,a.normalized_result,a.completed_at FROM payment_attempts a JOIN intents i ON i.id=a.intent_id),
	setups AS (SELECT id,state,invalidated_by_handoff,updated_at FROM payment_method_setup_sessions WHERE account_id=$1),
	observations AS (SELECT o.session_id,o.result_sha256 FROM billing_handoff_setup_observations o JOIN setups s ON s.id=o.session_id),
	webhooks AS (SELECT w.id,w.intent_id,w.processing_state,w.payload_sha256 FROM payment_webhook_inbox w JOIN intents i ON i.id=w.intent_id),
	unresolved_reversals AS (
		SELECT e.id,e.request_sha256,r.reason_code FROM billing_provider_reversal_events e
		JOIN billing_provider_reversal_reviews r ON r.event_id=e.id WHERE e.account_id=$1
		AND NOT EXISTS (SELECT 1 FROM billing_provider_reversal_allocations a WHERE a.event_id=e.id)
	),
	open_payment_intents AS (
		SELECT id FROM intents WHERE state IN ('created','processing','authorized','requires_action','unknown')
		UNION SELECT intent_id FROM jobs WHERE status <> 'completed'
		UNION SELECT intent_id FROM attempts WHERE completed_at IS NULL
		UNION SELECT intent_id FROM webhooks WHERE processing_state <> 'processed'
	)
	SELECT jsonb_build_object(
		'account',jsonb_build_array($1::text,$3::bigint,$4::bigint,$5::text,$6::text),
		'usage',(SELECT jsonb_agg(to_jsonb(u) ORDER BY u.id) FROM usage u),
		'periods',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM periods p),
		'invoices',(SELECT jsonb_agg(to_jsonb(i) ORDER BY i.id) FROM invoices i),
		'settlements',(SELECT jsonb_agg(to_jsonb(s) ORDER BY s.invoice_id) FROM settlements s),
		'intents',(SELECT jsonb_agg(to_jsonb(i) ORDER BY i.id) FROM intents i),
		'jobs',(SELECT jsonb_agg(to_jsonb(j) ORDER BY j.id) FROM jobs j),
		'attempts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM attempts a),
		'setups',(SELECT jsonb_agg(to_jsonb(s) ORDER BY s.id) FROM setups s),
		'observations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY o.session_id,o.result_sha256) FROM observations o),
		'webhooks',(SELECT jsonb_agg(to_jsonb(w) ORDER BY w.id) FROM webhooks w),
		'unresolved_reversals',(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id) FROM unresolved_reversals r)
	)::text,
	(SELECT count(*) FROM invoices i WHERE i.state <> 'void' AND (i.state <> 'settled' OR
		(i.total_minor > 0 AND NOT EXISTS (SELECT 1 FROM settlements s WHERE s.invoice_id=i.id AND s.state='posted' AND s.ledger_entry_id IS NOT NULL)))),
	(SELECT COALESCE(sum(amount_due_minor),0)::bigint FROM invoices WHERE state <> 'void'),
	(SELECT count(*) FROM open_payment_intents),
	(SELECT count(*) FROM jobs WHERE reason='refund' AND status <> 'completed'),
	(SELECT count(*) FROM setups WHERE state IN ('created','requires_action','unknown')),
	(SELECT count(*) FROM unresolved_reversals)`,
		account.ID, account.OrganizationID, account.AvailableBalanceMinor, account.Version, account.Currency, account.State).
		Scan(&encoded, &state.Financial.UnpaidInvoiceCount, &state.Financial.DebtMinor, &state.Financial.PendingPaymentCount,
			&state.Financial.PendingRefundCount, &state.Financial.PendingSetupCount, &state.Financial.UnresolvedProviderEventCount)
	if err != nil {
		return SettlementState{}, err
	}
	digest := sha256.Sum256([]byte(encoded))
	state.SHA256 = hex.EncodeToString(digest[:])
	return state, nil
}
