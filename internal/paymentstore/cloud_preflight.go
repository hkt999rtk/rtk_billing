package paymentstore

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/jackc/pgx/v5"
)

type CloudPreflightScope struct {
	OrganizationID   string `json:"cloud_id"`
	OwnerUserID      string `json:"owner_user_id"`
	OwnershipVersion int64  `json:"ownership_version"`
}

func (s CloudPreflightScope) valid() bool {
	return canonicalUUID(s.OrganizationID) && canonicalUUID(s.OwnerUserID) && s.OwnershipVersion > 0
}

type CloudPreflightState struct {
	Scope CloudPreflightScope
	SettlementState
	ObservedAt time.Time
}

// Only a trusted collector may record reconciled checkpoints. This interface is
// not exposed by the coordinator HTTP credential or any tenant-facing route.
type RecordCloudPreflightInput struct {
	State                                                                    CloudPreflightState
	ReceiptID                                                                string
	UsageCheckpointSHA256, InvoiceCheckpointSHA256, ProviderCheckpointSHA256 string
	ExpiresAt                                                                time.Time
}
type CloudDeletionPreflight struct {
	CloudPreflightScope
	Eligible     bool      `json:"eligible"`
	Blockers     []string  `json:"blockers"`
	BalanceMinor int64     `json:"balance_minor"`
	Currency     string    `json:"currency"`
	ObservedAt   time.Time `json:"observed_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func loadCloudPreflightAccountTx(ctx context.Context, tx pgx.Tx, in CloudPreflightScope, lock bool) (payment.CommercialAccount, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	account, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM commercial_accounts WHERE organization_id=$1 AND currency='TWD'`+suffix, in.OrganizationID))
	if err != nil {
		return account, err
	}
	var owner string
	var version int64
	err = tx.QueryRow(ctx, `SELECT owner_user_id::text,ownership_version FROM billing_responsibility_periods WHERE account_id=$1 AND effective_until IS NULL`, account.ID).Scan(&owner, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return account, ErrOwnershipEvidenceMissing
	}
	if err != nil {
		return account, err
	}
	if owner != in.OwnerUserID || version != in.OwnershipVersion {
		return account, ErrOwnershipVersionConflict
	}
	return account, nil
}

func (s *Store) CaptureCloudPreflightState(ctx context.Context, in CloudPreflightScope) (CloudPreflightState, error) {
	if !in.valid() {
		return CloudPreflightState{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return CloudPreflightState{}, err
	}
	defer tx.Rollback(ctx)
	account, err := loadCloudPreflightAccountTx(ctx, tx, in, false)
	if err != nil {
		return CloudPreflightState{}, err
	}
	var observed time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&observed); err != nil {
		return CloudPreflightState{}, err
	}
	state, err := captureSettlementStateTx(ctx, tx, account)
	if err != nil {
		return CloudPreflightState{}, err
	}
	return CloudPreflightState{Scope: in, SettlementState: state, ObservedAt: observed}, tx.Commit(ctx)
}

func (s *Store) RecordCloudPreflightEvidence(ctx context.Context, in RecordCloudPreflightInput) error {
	f := in.State.Financial
	if !in.State.Scope.valid() || !canonicalUUID(in.ReceiptID) || !validLowerSHA256(in.State.SHA256) ||
		!validCheckpoint(f.UsageSettled, in.UsageCheckpointSHA256) || !validCheckpoint(f.InvoicesReconciled, in.InvoiceCheckpointSHA256) || !validCheckpoint(f.ProviderWorkReconciled, in.ProviderCheckpointSHA256) ||
		in.State.ObservedAt.IsZero() || !in.ExpiresAt.After(in.State.ObservedAt) || in.ExpiresAt.After(in.State.ObservedAt.Add(5*time.Minute)) {
		return ErrConflict
	}
	for _, v := range []int64{f.UnpaidInvoiceCount, f.DebtMinor, f.PendingPaymentCount, f.PendingRefundCount, f.OpenDisputeCount, f.PendingSetupCount, f.UnresolvedProviderEventCount} {
		if v < 0 {
			return ErrConflict
		}
	}
	in.State.ObservedAt = databaseTime(in.State.ObservedAt)
	in.ExpiresAt = databaseTime(in.ExpiresAt)
	digest, err := handoffDigest(in)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	account, err := loadCloudPreflightAccountTx(ctx, tx, in.State.Scope, true)
	if err != nil {
		return err
	}
	var previous string
	err = tx.QueryRow(ctx, `SELECT request_sha256 FROM billing_cloud_preflight_receipts WHERE id=$1`, in.ReceiptID).Scan(&previous)
	if err == nil {
		if previous != digest {
			return ErrIdempotencyConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if in.State.ObservedAt.After(now) || !in.ExpiresAt.After(now) {
		return ErrSettlementEvidenceStale
	}
	local, err := captureSettlementStateTx(ctx, tx, account)
	if err != nil {
		return err
	}
	if local.SHA256 != in.State.SHA256 || !f.BalanceKnown || f.BalanceMinor != local.Financial.BalanceMinor || f.Currency != local.Financial.Currency {
		return ErrSettlementEvidenceStale
	}
	encoded, err := json.Marshal(mergeSettlementEvidence(f, local.Financial))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_cloud_preflight_receipts(id,account_id,owner_user_id,ownership_version,request_sha256,state_sha256,
        usage_checkpoint_sha256,invoice_checkpoint_sha256,provider_checkpoint_sha256,financial_evidence,observed_at,expires_at)
        VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),$10,$11,$12)`, in.ReceiptID, account.ID, in.State.Scope.OwnerUserID, in.State.Scope.OwnershipVersion, digest, in.State.SHA256,
		in.UsageCheckpointSHA256, in.InvoiceCheckpointSHA256, in.ProviderCheckpointSHA256, encoded, in.State.ObservedAt, in.ExpiresAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_audit_events(organization_id,event_type,actor_type,actor_id,subject_type,subject_id,request_id,payload)
        VALUES($1,'billing.cloud_preflight.evidence','service','billing_settlement_collector','commercial_account',$2,$3,jsonb_build_object('request_sha256',$4::text))`, account.OrganizationID, account.ID, in.ReceiptID, digest); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Advisory only. Uses a read-only repeatable snapshot and never creates an
// account, freezes payments, changes a receipt, or authorizes closure.
func (s *Store) GetCloudDeletionPreflight(ctx context.Context, in CloudPreflightScope) (CloudDeletionPreflight, error) {
	if !in.valid() {
		return CloudDeletionPreflight{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return CloudDeletionPreflight{}, err
	}
	defer tx.Rollback(ctx)
	account, err := loadCloudPreflightAccountTx(ctx, tx, in, false)
	if err != nil {
		return CloudDeletionPreflight{}, err
	}
	state, err := captureSettlementStateTx(ctx, tx, account)
	if err != nil {
		return CloudDeletionPreflight{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return CloudDeletionPreflight{}, err
	}
	out := CloudDeletionPreflight{CloudPreflightScope: in, Blockers: []string{}, BalanceMinor: state.Financial.BalanceMinor, Currency: string(state.Financial.Currency), ObservedAt: now, ExpiresAt: now.Add(30 * time.Second)}
	add := func(code string) {
		if !slices.Contains(out.Blockers, code) {
			out.Blockers = append(out.Blockers, code)
		}
	}
	var sha string
	var raw []byte
	var observed, expires time.Time
	err = tx.QueryRow(ctx, `SELECT state_sha256,financial_evidence,observed_at,expires_at FROM billing_cloud_preflight_receipts
        WHERE account_id=$1 AND owner_user_id=$2 AND ownership_version=$3 ORDER BY observed_at DESC,created_at DESC,id DESC LIMIT 1`, account.ID, in.OwnerUserID, in.OwnershipVersion).Scan(&sha, &raw, &observed, &expires)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CloudDeletionPreflight{}, err
	}
	financial := state.Financial
	if err == nil && sha == state.SHA256 && !observed.After(now) && expires.After(now) && observed.After(now.Add(-5*time.Minute)) {
		var evidence billing.FinancialEvidence
		if err := json.Unmarshal(raw, &evidence); err != nil {
			return CloudDeletionPreflight{}, err
		}
		financial = mergeSettlementEvidence(evidence, state.Financial)
		if expires.Before(out.ExpiresAt) {
			out.ExpiresAt = expires
		}
	} else {
		add("evidence_unavailable")
	}
	for _, code := range billing.CloudClosureBlockers(financial) {
		switch code {
		case "balance_negative", "balance_positive":
			add("balance_nonzero")
		case "usage_unsettled":
			add(code)
		case "unpaid_invoices", "outstanding_debt":
			add("debt_outstanding")
		case "payments_pending", "payment_setups_pending":
			add("payment_pending")
		case "refunds_pending":
			add("refund_pending")
		case "disputes_open":
			add("dispute_pending")
		default:
			add("evidence_unavailable")
		}
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_ownership_handoffs WHERE account_id=$1 AND phase NOT IN ('aborted','finalized'))`, account.ID).Scan(&active); err != nil {
		return CloudDeletionPreflight{}, err
	}
	if active || account.State != payment.AccountStateActive {
		add("lifecycle_conflict")
	}
	out.Eligible = len(out.Blockers) == 0
	return out, tx.Commit(ctx)
}
