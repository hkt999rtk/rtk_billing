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

var ErrCloudClosureNotReady = errors.New("cloud closure requires current zero-balance settlement and all provider revocations")

type CloudClosureScope struct {
	CloudPreflightScope
	OperationID string `json:"operation_id"`
}

func (in CloudClosureScope) valid() bool {
	return in.CloudPreflightScope.valid() && canonicalUUID(in.OperationID)
}

type PrepareCloudClosureInput struct {
	Scope           CloudClosureScope
	Cutoff          time.Time
	AMRequestSHA256 string
}
type CloudClosure struct {
	ID                            string    `json:"id"`
	AccountID                     string    `json:"account_id"`
	OwnerUserID                   string    `json:"owner_user_id"`
	OwnershipVersion              int64     `json:"ownership_version"`
	Cutoff                        time.Time `json:"cutoff"`
	Phase                         string    `json:"phase"`
	Version                       int64     `json:"version"`
	CreatedAt                     time.Time `json:"created_at"`
	SourcePeriodID, RequestSHA256 string    `json:"-"`
}

const cloudClosureColumns = `id::text,account_id::text,owner_user_id::text,ownership_version,cutoff,phase,version,created_at,source_period_id::text,request_sha256`

func scanCloudClosure(row rowScanner) (CloudClosure, error) {
	var c CloudClosure
	err := row.Scan(&c.ID, &c.AccountID, &c.OwnerUserID, &c.OwnershipVersion, &c.Cutoff, &c.Phase, &c.Version, &c.CreatedAt, &c.SourcePeriodID, &c.RequestSHA256)
	return c, mapNotFound(err)
}

// The caller is the trusted AM coordinator with a durable deletion intent and
// resource fence. This installs a financial fence, not closure/settlement proof.
func (s *Store) PrepareCloudClosure(ctx context.Context, in PrepareCloudClosureInput) (CloudClosure, error) {
	if !in.Scope.valid() || in.Cutoff.IsZero() || !validLowerSHA256(in.AMRequestSHA256) {
		return CloudClosure{}, ErrConflict
	}
	in.Cutoff = databaseTime(in.Cutoff)
	digest, err := handoffDigest(in)
	if err != nil {
		return CloudClosure{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CloudClosure{}, err
	}
	defer tx.Rollback(ctx)
	account, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM commercial_accounts WHERE organization_id=$1 AND currency='TWD' FOR UPDATE`, in.Scope.OrganizationID))
	if err != nil {
		return CloudClosure{}, err
	}
	prior, err := scanCloudClosure(tx.QueryRow(ctx, `SELECT `+cloudClosureColumns+` FROM billing_cloud_closures WHERE id=$1`, in.Scope.OperationID))
	if err == nil {
		if prior.AccountID != account.ID || prior.RequestSHA256 != digest {
			return CloudClosure{}, ErrIdempotencyConflict
		}
		return prior, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return CloudClosure{}, err
	}
	if err := requireNoHandoffTx(ctx, tx, account.ID); err != nil {
		return CloudClosure{}, err
	}
	if account.State != payment.AccountStateActive {
		return CloudClosure{}, ErrConflict
	}
	var period string
	var valid bool
	err = tx.QueryRow(ctx, `SELECT id::text,effective_from<=$4 AND $4<=clock_timestamp() FROM billing_responsibility_periods WHERE account_id=$1 AND owner_user_id=$2 AND ownership_version=$3 AND effective_until IS NULL`, account.ID, in.Scope.OwnerUserID, in.Scope.OwnershipVersion, in.Cutoff).Scan(&period, &valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudClosure{}, ErrOwnershipVersionConflict
	}
	if err != nil {
		return CloudClosure{}, err
	}
	if !valid {
		return CloudClosure{}, ErrOwnershipVersionConflict
	}
	op, err := scanCloudClosure(tx.QueryRow(ctx, `INSERT INTO billing_cloud_closures(id,account_id,source_period_id,owner_user_id,ownership_version,cutoff,am_request_sha256,request_sha256)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+cloudClosureColumns, in.Scope.OperationID, account.ID, period, in.Scope.OwnerUserID, in.Scope.OwnershipVersion, in.Cutoff, in.AMRequestSHA256, digest))
	if err != nil {
		return CloudClosure{}, err
	}
	// Persist exact provider work before revoking local authority. No network
	// call is made under this transaction, and local flags are not provider acks.
	if _, err := tx.Exec(ctx, `INSERT INTO billing_cloud_closure_revocations(operation_id,subject_type,subject_id)
        SELECT $1::uuid,'method',id FROM payment_methods WHERE account_id=$2
        UNION ALL SELECT $1::uuid,'setup',id FROM payment_method_setup_sessions WHERE account_id=$2 AND state IN ('created','requires_action','unknown')`, op.ID, account.ID); err != nil {
		return CloudClosure{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_method_setup_sessions SET state='failed',invalidated_by_closure=$2 WHERE account_id=$1 AND state IN ('created','requires_action','unknown')`, account.ID, op.ID); err != nil {
		return CloudClosure{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_methods SET status='revoked' WHERE account_id=$1 AND status<>'revoked'`, account.ID); err != nil {
		return CloudClosure{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_consents SET revoked_at=now(),revocation_reason='cloud_closure' WHERE account_id=$1 AND revoked_at IS NULL`, account.ID); err != nil {
		return CloudClosure{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE auto_topup_policies SET enabled=false,armed=false,version=version+1,updated_by='cloud_closure' WHERE account_id=$1 AND (enabled OR armed)`, account.ID); err != nil {
		return CloudClosure{}, err
	}
	if err := cloudClosureAuditTx(ctx, tx, in.Scope, "prepare", digest); err != nil {
		return CloudClosure{}, err
	}
	return op, tx.Commit(ctx)
}

func loadCloudClosureTx(ctx context.Context, tx pgx.Tx, scope CloudClosureScope) (payment.CommercialAccount, CloudClosure, error) {
	account, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM commercial_accounts WHERE organization_id=$1 AND currency='TWD' FOR UPDATE`, scope.OrganizationID))
	if err != nil {
		return account, CloudClosure{}, err
	}
	op, err := scanCloudClosure(tx.QueryRow(ctx, `SELECT `+cloudClosureColumns+` FROM billing_cloud_closures WHERE id=$1 AND account_id=$2 FOR UPDATE`, scope.OperationID, account.ID))
	if err == nil && (op.OwnerUserID != scope.OwnerUserID || op.OwnershipVersion != scope.OwnershipVersion) {
		err = ErrOwnershipVersionConflict
	}
	return account, op, err
}
func requireClosureOwnerTx(ctx context.Context, tx pgx.Tx, op CloudClosure) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_responsibility_periods WHERE id=$1 AND account_id=$2 AND owner_user_id=$3 AND ownership_version=$4 AND effective_until IS NULL)`, op.SourcePeriodID, op.AccountID, op.OwnerUserID, op.OwnershipVersion).Scan(&valid)
	if err == nil && !valid {
		return ErrOwnershipVersionConflict
	}
	return err
}
func cloudClosureAuditTx(ctx context.Context, tx pgx.Tx, scope CloudClosureScope, action, digest string) error {
	_, err := tx.Exec(ctx, `INSERT INTO billing_audit_events(organization_id,event_type,actor_type,actor_id,subject_type,subject_id,request_id,payload)
        VALUES($1,$2,'service','cloud_closure_coordinator','cloud_closure',$3,$4,jsonb_build_object('request_sha256',$4::text))`, scope.OrganizationID, "billing.cloud_closure."+action, scope.OperationID, digest)
	return err
}

type CloudClosureRevocation struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
}

func (s *Store) PendingCloudClosureRevocations(ctx context.Context, scope CloudClosureScope) ([]CloudClosureRevocation, error) {
	if !scope.valid() {
		return nil, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	_, op, err := loadCloudClosureTx(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT t.subject_type,t.subject_id::text FROM billing_cloud_closure_revocations t WHERE t.operation_id=$1 AND NOT EXISTS(
        SELECT 1 FROM billing_cloud_closure_revocation_acks a WHERE (a.operation_id,a.subject_type,a.subject_id)=(t.operation_id,t.subject_type,t.subject_id)) ORDER BY t.subject_type,t.subject_id`, op.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []CloudClosureRevocation{}
	for rows.Next() {
		var t CloudClosureRevocation
		if err := rows.Scan(&t.SubjectType, &t.SubjectID); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return tasks, tx.Commit(ctx)
}

// Trusted provider adapter only: receipt must attest revoke/cancel (or verified
// absence) for this exact stored task. There is no coordinator/browser HTTP path.
func (s *Store) RecordCloudClosureRevocation(ctx context.Context, scope CloudClosureScope, task CloudClosureRevocation, receiptSHA string) error {
	if !scope.valid() || !canonicalUUID(task.SubjectID) || (task.SubjectType != "method" && task.SubjectType != "setup") || !validLowerSHA256(receiptSHA) {
		return ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, op, err := loadCloudClosureTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	var prior string
	err = tx.QueryRow(ctx, `SELECT receipt_sha256 FROM billing_cloud_closure_revocation_acks WHERE operation_id=$1 AND subject_type=$2 AND subject_id=$3`, op.ID, task.SubjectType, task.SubjectID).Scan(&prior)
	if err == nil {
		if prior != receiptSHA {
			return ErrIdempotencyConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if op.Phase != "preparing" && op.Phase != "canceling" {
		return ErrConflict
	}
	tag, err := tx.Exec(ctx, `INSERT INTO billing_cloud_closure_revocation_acks(operation_id,subject_type,subject_id,receipt_sha256)
        SELECT operation_id,subject_type,subject_id,$4 FROM billing_cloud_closure_revocations WHERE operation_id=$1 AND subject_type=$2 AND subject_id=$3`, op.ID, task.SubjectType, task.SubjectID, receiptSHA)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	if err := cloudClosureAuditTx(ctx, tx, scope, "provider_revocation", receiptSHA); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type CloudClosureState struct {
	Scope              CloudClosureScope
	SHA256             string
	Financial          billing.FinancialEvidence
	Cutoff, ObservedAt time.Time
}

func captureCloudClosureStateTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount, op CloudClosure, scope CloudClosureScope) (CloudClosureState, error) {
	state, err := captureSettlementStateTx(ctx, tx, account)
	if err != nil {
		return CloudClosureState{}, err
	}
	var manifest string
	var observed time.Time
	err = tx.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(t.subject_type,t.subject_id,a.receipt_sha256) ORDER BY t.subject_type,t.subject_id),'[]'::jsonb)::text,clock_timestamp()
        FROM billing_cloud_closure_revocations t LEFT JOIN billing_cloud_closure_revocation_acks a USING(operation_id,subject_type,subject_id) WHERE t.operation_id=$1`, op.ID).Scan(&manifest, &observed)
	if err != nil {
		return CloudClosureState{}, err
	}
	digest, err := handoffDigest(struct{ Financial, Manifest, Request string }{state.SHA256, manifest, op.RequestSHA256})
	if err != nil {
		return CloudClosureState{}, err
	}
	return CloudClosureState{Scope: scope, SHA256: digest, Financial: state.Financial, Cutoff: op.Cutoff, ObservedAt: observed}, nil
}
func (s *Store) CaptureCloudClosureState(ctx context.Context, scope CloudClosureScope) (CloudClosureState, error) {
	if !scope.valid() {
		return CloudClosureState{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CloudClosureState{}, err
	}
	defer tx.Rollback(ctx)
	account, op, err := loadCloudClosureTx(ctx, tx, scope)
	if err != nil {
		return CloudClosureState{}, err
	}
	if op.Phase != "preparing" {
		return CloudClosureState{}, ErrConflict
	}
	if err := requireClosureOwnerTx(ctx, tx, op); err != nil {
		return CloudClosureState{}, err
	}
	out, err := captureCloudClosureStateTx(ctx, tx, account, op, scope)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

type RecordCloudClosureSettlementInput struct {
	State                                                                    CloudClosureState
	ReceiptID                                                                string
	CoveredThrough, ExpiresAt                                                time.Time
	UsageCheckpointSHA256, InvoiceCheckpointSHA256, ProviderCheckpointSHA256 string
}

func (s *Store) RecordCloudClosureSettlement(ctx context.Context, in RecordCloudClosureSettlementInput) error {
	f := in.State.Financial
	if !in.State.Scope.valid() || !canonicalUUID(in.ReceiptID) || !validLowerSHA256(in.State.SHA256) || !validCheckpoint(f.UsageSettled, in.UsageCheckpointSHA256) ||
		!validCheckpoint(f.InvoicesReconciled, in.InvoiceCheckpointSHA256) || !validCheckpoint(f.ProviderWorkReconciled, in.ProviderCheckpointSHA256) || in.CoveredThrough.Before(in.State.Cutoff) || in.CoveredThrough.After(in.State.ObservedAt) ||
		in.State.ObservedAt.IsZero() || !in.ExpiresAt.After(in.State.ObservedAt) || in.ExpiresAt.After(in.State.ObservedAt.Add(5*time.Minute)) {
		return ErrConflict
	}
	for _, n := range []int64{f.UnpaidInvoiceCount, f.DebtMinor, f.PendingPaymentCount, f.PendingRefundCount, f.OpenDisputeCount, f.PendingSetupCount, f.UnresolvedProviderEventCount} {
		if n < 0 {
			return ErrConflict
		}
	}
	in.State.Cutoff = databaseTime(in.State.Cutoff)
	in.State.ObservedAt = databaseTime(in.State.ObservedAt)
	in.CoveredThrough = databaseTime(in.CoveredThrough)
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
	account, op, err := loadCloudClosureTx(ctx, tx, in.State.Scope)
	if err != nil {
		return err
	}
	var prior string
	err = tx.QueryRow(ctx, `SELECT request_sha256 FROM billing_cloud_closure_settlements WHERE id=$1`, in.ReceiptID).Scan(&prior)
	if err == nil {
		if prior != digest {
			return ErrIdempotencyConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if op.Phase != "preparing" || !op.Cutoff.Equal(in.State.Cutoff) {
		return ErrConflict
	}
	if err := requireClosureOwnerTx(ctx, tx, op); err != nil {
		return err
	}
	current, err := captureCloudClosureStateTx(ctx, tx, account, op, in.State.Scope)
	if err != nil {
		return err
	}
	if current.SHA256 != in.State.SHA256 || in.State.ObservedAt.After(current.ObservedAt) || !in.ExpiresAt.After(current.ObservedAt) || !f.BalanceKnown || f.Currency != current.Financial.Currency || f.BalanceMinor != current.Financial.BalanceMinor {
		return ErrSettlementEvidenceStale
	}
	encoded, err := json.Marshal(mergeSettlementEvidence(f, current.Financial))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_cloud_closure_settlements(id,operation_id,request_sha256,state_sha256,financial_evidence,usage_checkpoint_sha256,invoice_checkpoint_sha256,provider_checkpoint_sha256,covered_through,observed_at,expires_at)
        VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,$10,$11)`, in.ReceiptID, op.ID, digest, current.SHA256, encoded, in.UsageCheckpointSHA256, in.InvoiceCheckpointSHA256, in.ProviderCheckpointSHA256, in.CoveredThrough, in.State.ObservedAt, in.ExpiresAt); err != nil {
		return err
	}
	if err := cloudClosureAuditTx(ctx, tx, in.State.Scope, "settlement", digest); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type CloudClosureStatus struct {
	Operation CloudClosure `json:"operation"`
	Ready     bool         `json:"ready"`
	ReceiptID string       `json:"receipt_id,omitempty"`
	Blockers  []string     `json:"blockers"`
}

func cloudClosureStatusTx(ctx context.Context, tx pgx.Tx, account payment.CommercialAccount, op CloudClosure, scope CloudClosureScope) (CloudClosureStatus, error) {
	out := CloudClosureStatus{Operation: op, Blockers: []string{}}
	if op.Phase != "preparing" {
		return out, nil
	}
	if err := requireClosureOwnerTx(ctx, tx, op); err != nil {
		return out, err
	}
	state, err := captureCloudClosureStateTx(ctx, tx, account, op, scope)
	if err != nil {
		return out, err
	}
	financial := state.Financial
	var raw []byte
	var sha string
	var observed, expires, covered time.Time
	err = tx.QueryRow(ctx, `SELECT id::text,state_sha256,financial_evidence,observed_at,expires_at,covered_through FROM billing_cloud_closure_settlements WHERE operation_id=$1 ORDER BY observed_at DESC,created_at DESC,id DESC LIMIT 1`, op.ID).Scan(&out.ReceiptID, &sha, &raw, &observed, &expires, &covered)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if err == nil && sha == state.SHA256 && !observed.After(state.ObservedAt) && expires.After(state.ObservedAt) && !covered.Before(op.Cutoff) {
		var evidence billing.FinancialEvidence
		if err := json.Unmarshal(raw, &evidence); err != nil {
			return out, err
		}
		financial = mergeSettlementEvidence(evidence, financial)
	} else {
		out.ReceiptID = ""
		out.Blockers = append(out.Blockers, "evidence_unavailable")
	}
	out.Blockers = append(out.Blockers, billing.CloudClosureBlockers(financial)...)
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_cloud_closure_revocations t WHERE t.operation_id=$1 AND NOT EXISTS(SELECT 1 FROM billing_cloud_closure_revocation_acks a WHERE (a.operation_id,a.subject_type,a.subject_id)=(t.operation_id,t.subject_type,t.subject_id)))`, op.ID).Scan(&pending); err != nil {
		return out, err
	}
	if pending {
		out.Blockers = append(out.Blockers, "provider_revocations_pending")
	}
	if account.State != payment.AccountStateActive {
		out.Blockers = append(out.Blockers, "account_not_active")
	}
	slices.Sort(out.Blockers)
	out.Blockers = slices.Compact(out.Blockers)
	out.Ready = len(out.Blockers) == 0
	return out, nil
}
func (s *Store) GetCloudClosureStatus(ctx context.Context, scope CloudClosureScope) (CloudClosureStatus, error) {
	if !scope.valid() {
		return CloudClosureStatus{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CloudClosureStatus{}, err
	}
	defer tx.Rollback(ctx)
	account, op, err := loadCloudClosureTx(ctx, tx, scope)
	if err != nil {
		return CloudClosureStatus{}, err
	}
	out, err := cloudClosureStatusTx(ctx, tx, account, op, scope)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

type CloseCloudInput struct {
	Scope                           CloudClosureScope
	SettlementID, AMReadinessSHA256 string
}
type CloudClosureAck struct {
	OperationID   string    `json:"operation_id"`
	Phase         string    `json:"phase"`
	ClosedAt      time.Time `json:"closed_at"`
	ReceiptSHA256 string    `json:"receipt_sha256"`
}

// AMReadinessSHA256 names the coordinator's durable, verified empty-resource
// and producer-fence decision. This is not an owner/browser assertion. Billing
// closes first; a lost response or later AM failure must retry forward.
func (s *Store) CloseCloud(ctx context.Context, in CloseCloudInput) (CloudClosureAck, error) {
	if !in.Scope.valid() || !canonicalUUID(in.SettlementID) || !validLowerSHA256(in.AMReadinessSHA256) {
		return CloudClosureAck{}, ErrConflict
	}
	digest, err := handoffDigest(in)
	if err != nil {
		return CloudClosureAck{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CloudClosureAck{}, err
	}
	defer tx.Rollback(ctx)
	account, op, err := loadCloudClosureTx(ctx, tx, in.Scope)
	if err != nil {
		return CloudClosureAck{}, err
	}
	ack := CloudClosureAck{OperationID: op.ID, Phase: "closed"}
	var prior string
	err = tx.QueryRow(ctx, `SELECT request_sha256,closed_at,receipt_sha256 FROM billing_cloud_closure_completions WHERE operation_id=$1`, op.ID).Scan(&prior, &ack.ClosedAt, &ack.ReceiptSHA256)
	if err == nil {
		if prior != digest {
			return CloudClosureAck{}, ErrIdempotencyConflict
		}
		return ack, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ack, err
	}
	status, err := cloudClosureStatusTx(ctx, tx, account, op, in.Scope)
	if err != nil {
		return ack, err
	}
	if !status.Ready || status.ReceiptID != in.SettlementID {
		return ack, ErrCloudClosureNotReady
	}
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&ack.ClosedAt); err != nil {
		return ack, err
	}
	ack.ReceiptSHA256, err = handoffDigest(struct {
		Request  string
		ClosedAt time.Time
	}{digest, ack.ClosedAt})
	if err != nil {
		return ack, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_cloud_closure_completions(operation_id,settlement_id,am_readiness_sha256,request_sha256,receipt_sha256,closed_at) VALUES($1,$2,$3,$4,$5,$6)`, op.ID, in.SettlementID, in.AMReadinessSHA256, digest, ack.ReceiptSHA256, ack.ClosedAt); err != nil {
		return ack, err
	}
	if _, err := tx.Exec(ctx, `UPDATE billing_cloud_closures SET phase='closed',version=version+1,updated_at=now() WHERE id=$1`, op.ID); err != nil {
		return ack, err
	}
	if _, err := tx.Exec(ctx, `UPDATE commercial_accounts SET state='closed',version=version+1 WHERE id=$1`, account.ID); err != nil {
		return ack, err
	}
	if _, err := tx.Exec(ctx, `UPDATE billing_responsibility_periods SET effective_until=$2 WHERE id=$1 AND effective_until IS NULL`, op.SourcePeriodID, ack.ClosedAt); err != nil {
		return ack, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_access_states(organization_id,state,reason_code,updated_by) VALUES($1,'closed','cloud_deleted','cloud_closure') ON CONFLICT(organization_id) DO UPDATE SET state='closed',reason_code='cloud_deleted',updated_by='cloud_closure',version=billing_access_states.version+1,updated_at=now()`, in.Scope.OrganizationID); err != nil {
		return ack, err
	}
	if err := cloudClosureAuditTx(ctx, tx, in.Scope, "closed", digest); err != nil {
		return ack, err
	}
	return ack, tx.Commit(ctx)
}

func (s *Store) CancelCloudClosure(ctx context.Context, scope CloudClosureScope, cancellationID, decisionSHA string) (CloudClosure, error) {
	if !scope.valid() || !canonicalUUID(cancellationID) || !validLowerSHA256(decisionSHA) {
		return CloudClosure{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CloudClosure{}, err
	}
	defer tx.Rollback(ctx)
	_, op, err := loadCloudClosureTx(ctx, tx, scope)
	if err != nil {
		return op, err
	}
	var id, sha string
	err = tx.QueryRow(ctx, `SELECT cancellation_id::text,am_cancellation_sha256 FROM billing_cloud_closure_cancellations WHERE operation_id=$1`, op.ID).Scan(&id, &sha)
	if err == nil {
		if id != cancellationID || sha != decisionSHA {
			return op, ErrIdempotencyConflict
		}
		return op, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return op, err
	}
	if op.Phase != "preparing" {
		return op, ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_cloud_closure_cancellations(operation_id,cancellation_id,am_cancellation_sha256) VALUES($1,$2,$3)`, op.ID, cancellationID, decisionSHA); err != nil {
		return op, err
	}
	op, err = scanCloudClosure(tx.QueryRow(ctx, `UPDATE billing_cloud_closures SET phase='canceling',version=version+1,updated_at=now() WHERE id=$1 RETURNING `+cloudClosureColumns, op.ID))
	if err != nil {
		return op, err
	}
	if err := cloudClosureAuditTx(ctx, tx, scope, "canceling", decisionSHA); err != nil {
		return op, err
	}
	return op, tx.Commit(ctx)
}

// Only the verified release adapter calls this. Revoked methods/consents are
// never restored; elapsed time, missing operation or cancellation alone is not release.
func (s *Store) CompleteCloudClosureCancellation(ctx context.Context, scope CloudClosureScope, cancellationID, releaseSHA string) (CloudClosure, error) {
	if !scope.valid() || !canonicalUUID(cancellationID) || !validLowerSHA256(releaseSHA) {
		return CloudClosure{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CloudClosure{}, err
	}
	defer tx.Rollback(ctx)
	_, op, err := loadCloudClosureTx(ctx, tx, scope)
	if err != nil {
		return op, err
	}
	var prior string
	err = tx.QueryRow(ctx, `SELECT c.cancellation_id::text FROM billing_cloud_closure_cancellations c WHERE operation_id=$1`, op.ID).Scan(&prior)
	if err != nil {
		return op, mapNotFound(err)
	}
	if prior != cancellationID {
		return op, ErrIdempotencyConflict
	}
	err = tx.QueryRow(ctx, `SELECT receipt_sha256 FROM billing_cloud_closure_release_acks WHERE operation_id=$1`, op.ID).Scan(&prior)
	if err == nil {
		if prior != releaseSHA {
			return op, ErrIdempotencyConflict
		}
		return op, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return op, err
	}
	if op.Phase != "canceling" {
		return op, ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_cloud_closure_release_acks(operation_id,receipt_sha256) VALUES($1,$2)`, op.ID, releaseSHA); err != nil {
		return op, err
	}
	op, err = scanCloudClosure(tx.QueryRow(ctx, `UPDATE billing_cloud_closures SET phase='canceled',version=version+1,updated_at=now() WHERE id=$1 RETURNING `+cloudClosureColumns, op.ID))
	if err != nil {
		return op, err
	}
	if err := cloudClosureAuditTx(ctx, tx, scope, "canceled", releaseSHA); err != nil {
		return op, err
	}
	return op, tx.Commit(ctx)
}
