package paymentstore

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

type AuthorizeHandoffCommitInput struct {
	Scope           HandoffScope
	AuthorizationID string
	SnapshotVersion int64
}

type HandoffCommitAuthorization struct {
	OperationID     string    `json:"operation_id"`
	AuthorizationID string    `json:"authorization_id"`
	SnapshotVersion int64     `json:"snapshot_version"`
	CreatedAt       time.Time `json:"created_at"`
	RequestSHA256   string    `json:"-"`
	StateSHA256     string    `json:"-"`
}

func scanCommitAuthorization(row rowScanner) (HandoffCommitAuthorization, error) {
	var out HandoffCommitAuthorization
	err := row.Scan(&out.OperationID, &out.AuthorizationID, &out.SnapshotVersion, &out.CreatedAt, &out.RequestSHA256, &out.StateSHA256)
	return out, mapNotFound(err)
}

const commitAuthorizationColumns = `operation_id::text,authorization_id::text,snapshot_version,created_at,request_sha256,state_sha256`

// The trusted Account Manager coordinator consumes this grant in its own owner
// compare-and-swap transaction. Producer holds/checkpoints must already be
// verified. No tenant route can mint a grant; neither a balance nor two supplied
// confirmation booleans is accepted as a substitute for persisted evidence.
func (s *Store) AuthorizeHandoffCommit(ctx context.Context, in AuthorizeHandoffCommitInput) (HandoffCommitAuthorization, error) {
	if !validHandoffScope(in.Scope) || !canonicalUUID(in.AuthorizationID) || in.SnapshotVersion < 2 {
		return HandoffCommitAuthorization{}, ErrConflict
	}
	digest, err := handoffDigest(in)
	if err != nil {
		return HandoffCommitAuthorization{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return HandoffCommitAuthorization{}, err
	}
	defer tx.Rollback(ctx)
	account, op, err := loadHandoffScopeTx(ctx, tx, in.Scope)
	if err != nil {
		return HandoffCommitAuthorization{}, err
	}
	prior, err := scanCommitAuthorization(tx.QueryRow(ctx, `SELECT `+commitAuthorizationColumns+` FROM billing_handoff_commit_authorizations WHERE operation_id=$1`, op.ID))
	if err == nil {
		if prior.RequestSHA256 != digest {
			return HandoffCommitAuthorization{}, ErrIdempotencyConflict
		}
		if op.Phase != "commit_authorized" {
			return HandoffCommitAuthorization{}, ErrConflict
		}
		state, err := captureSettlementStateTx(ctx, tx, account)
		if err != nil {
			return HandoffCommitAuthorization{}, err
		}
		if state.SHA256 != prior.StateSHA256 {
			return HandoffCommitAuthorization{}, ErrSettlementEvidenceStale
		}
		return prior, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return HandoffCommitAuthorization{}, err
	}
	status, err := handoffSettlementStatusTx(ctx, tx, account, op)
	if err != nil {
		return HandoffCommitAuthorization{}, err
	}
	if status.Snapshot == nil || status.Snapshot.Version != in.SnapshotVersion || !status.Snapshot.SourceConfirmed || !status.Snapshot.TargetConfirmed {
		return HandoffCommitAuthorization{}, ErrHandoffNotConfirmable
	}
	var stateSHA string
	if err := tx.QueryRow(ctx, `SELECT state_sha256 FROM billing_handoff_settlement_receipts WHERE operation_id=$1 AND operation_version=$2`,
		op.ID, in.SnapshotVersion).Scan(&stateSHA); err != nil {
		return HandoffCommitAuthorization{}, err
	}
	grant, err := scanCommitAuthorization(tx.QueryRow(ctx, `INSERT INTO billing_handoff_commit_authorizations
		(operation_id,authorization_id,snapshot_version,request_sha256,state_sha256) VALUES ($1,$2,$3,$4,$5)
		RETURNING `+commitAuthorizationColumns, op.ID, in.AuthorizationID, in.SnapshotVersion, digest, stateSHA))
	if err != nil {
		return HandoffCommitAuthorization{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE billing_ownership_handoffs SET phase='commit_authorized',version=version+1,updated_at=now() WHERE id=$1`, op.ID); err != nil {
		return HandoffCommitAuthorization{}, err
	}
	if err := handoffProtocolAuditTx(ctx, tx, in.Scope, "authorize_commit", in.AuthorizationID, digest); err != nil {
		return HandoffCommitAuthorization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HandoffCommitAuthorization{}, err
	}
	return grant, nil
}

type FinalizeHandoffInput struct {
	Scope                     HandoffScope
	AuthorizationID           string
	CommittedOwnerUserID      string
	CommittedOwnershipVersion int64
	CommittedAt               time.Time
	AMCommitSHA256            string
}

type HandoffProtocolAck struct {
	OperationID string `json:"operation_id"`
	Phase       string `json:"phase"`
}

// AMCommitSHA256 names Account Manager's durable committed decision, verified by
// the authenticated coordinator. It is not a browser assertion. Financial drift
// or unavailable evidence retains the fence; this path never restores old owner
// authority, even if a response is lost after Account Manager committed.
func (s *Store) FinalizeOwnershipHandoff(ctx context.Context, in FinalizeHandoffInput) (HandoffProtocolAck, error) {
	if !validHandoffScope(in.Scope) || !canonicalUUID(in.AuthorizationID) || !canonicalUUID(in.CommittedOwnerUserID) ||
		in.Scope.OwnershipVersion == math.MaxInt64 || in.CommittedOwnershipVersion != in.Scope.OwnershipVersion+1 ||
		in.CommittedAt.IsZero() || !validLowerSHA256(in.AMCommitSHA256) {
		return HandoffProtocolAck{}, ErrConflict
	}
	in.CommittedAt = databaseTime(in.CommittedAt)
	digest, err := handoffDigest(in)
	if err != nil {
		return HandoffProtocolAck{}, ErrConflict
	}
	// Preserve the irreversible AM decision in a separate transaction BEFORE
	// local side effects. An audit/profile/consent failure below must not make a
	// known committed owner swap appear eligible for precommit cancellation.
	if err := s.recordCommittedHandoffDecision(ctx, in, digest); err != nil {
		return HandoffProtocolAck{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return HandoffProtocolAck{}, err
	}
	defer tx.Rollback(ctx)
	account, op, err := lockHandoffOperationTx(ctx, tx, in.Scope)
	if err != nil {
		return HandoffProtocolAck{}, err
	}
	ack := HandoffProtocolAck{OperationID: op.ID, Phase: "finalized"}
	var priorDigest string
	err = tx.QueryRow(ctx, `SELECT request_sha256 FROM billing_handoff_finalizations WHERE operation_id=$1`, op.ID).Scan(&priorDigest)
	if err == nil {
		if priorDigest != digest {
			return HandoffProtocolAck{}, ErrIdempotencyConflict
		}
		return ack, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return HandoffProtocolAck{}, err
	}
	if op.Phase != "finalizing" || in.CommittedOwnerUserID != op.TargetUserID || in.CommittedAt.Before(op.Cutoff) {
		return HandoffProtocolAck{}, ErrConflict
	}
	if err := requireSourceResponsibilityTx(ctx, tx, account, op); err != nil {
		return HandoffProtocolAck{}, err
	}
	grant, err := scanCommitAuthorization(tx.QueryRow(ctx, `SELECT `+commitAuthorizationColumns+` FROM billing_handoff_commit_authorizations WHERE operation_id=$1`, op.ID))
	if err != nil {
		return HandoffProtocolAck{}, err
	}
	if grant.AuthorizationID != in.AuthorizationID {
		return HandoffProtocolAck{}, ErrConflict
	}
	state, err := captureSettlementStateTx(ctx, tx, account)
	if err != nil {
		return HandoffProtocolAck{}, err
	}
	if state.SHA256 != grant.StateSHA256 || account.AvailableBalanceMinor < 0 {
		return HandoffProtocolAck{}, ErrSettlementEvidenceStale
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_handoff_finalizations
		(operation_id,request_sha256,am_commit_sha256,committed_ownership_version,committed_at)
		VALUES ($1,$2,$3,$4,$5)`, op.ID, digest, in.AMCommitSHA256, in.CommittedOwnershipVersion, in.CommittedAt); err != nil {
		return HandoffProtocolAck{}, err
	}
	var sourcePeriodID string
	if err := tx.QueryRow(ctx, `UPDATE billing_responsibility_periods SET effective_until=$4
		WHERE account_id=$1 AND owner_user_id=$2 AND ownership_version=$3 AND effective_until IS NULL AND effective_from<$4
		RETURNING id::text`, account.ID, op.SourceUserID, op.OwnershipVersion, in.CommittedAt).Scan(&sourcePeriodID); err != nil {
		return HandoffProtocolAck{}, mapNotFound(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_responsibility_periods
		(account_id,owner_user_id,ownership_version,effective_from,source_evidence_sha256,opening_operation_id,opening_balance_minor)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, account.ID, op.TargetUserID, in.CommittedOwnershipVersion,
		in.CommittedAt, in.AMCommitSHA256, op.ID, account.AvailableBalanceMinor); err != nil {
		return HandoffProtocolAck{}, err
	}
	// Archive the prospective source profile, never rewrite invoice recipients.
	if _, err := tx.Exec(ctx, `INSERT INTO billing_retired_profiles(operation_id,source_period_id,profile_snapshot)
		SELECT $1,$2,to_jsonb(p) FROM billing_profiles p WHERE organization_id=$3`, op.ID, sourcePeriodID, account.OrganizationID); err != nil {
		return HandoffProtocolAck{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_profiles
		(organization_id,legal_name,ownership_version,requires_configuration,locale,timezone,delivery_preference)
		VALUES ($1,'',$2,true,'zh-TW','Asia/Taipei','portal')
		ON CONFLICT (organization_id) DO UPDATE SET legal_name='',tax_identifier=NULL,billing_address=NULL,contact_email=NULL,
		ownership_version=$2,requires_configuration=true,locale='zh-TW',timezone='Asia/Taipei',delivery_preference='portal',
		version=billing_profiles.version+1,updated_at=now()`, account.OrganizationID, in.CommittedOwnershipVersion); err != nil {
		return HandoffProtocolAck{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_consents SET revoked_at=COALESCE(revoked_at,now()),
		revocation_reason=COALESCE(revocation_reason,'ownership_transferred') WHERE account_id=$1`, account.ID); err != nil {
		return HandoffProtocolAck{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_methods SET status='revoked' WHERE account_id=$1 AND status<>'revoked'`, account.ID); err != nil {
		return HandoffProtocolAck{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE auto_topup_policies SET enabled=false,armed=false,version=version+1,generation=generation+1 WHERE account_id=$1`, account.ID); err != nil {
		return HandoffProtocolAck{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE billing_ownership_handoffs SET phase='finalized',version=version+1,updated_at=now() WHERE id=$1`, op.ID); err != nil {
		return HandoffProtocolAck{}, err
	}
	if err := handoffProtocolAuditTx(ctx, tx, in.Scope, "finalize", in.AuthorizationID, digest); err != nil {
		return HandoffProtocolAck{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HandoffProtocolAck{}, err
	}
	return ack, nil
}

func (s *Store) recordCommittedHandoffDecision(ctx context.Context, in FinalizeHandoffInput, digest string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	account, op, err := lockHandoffOperationTx(ctx, tx, in.Scope)
	if err != nil {
		return err
	}
	var priorDigest string
	err = tx.QueryRow(ctx, `SELECT request_sha256 FROM billing_handoff_committed_decisions WHERE operation_id=$1`, op.ID).Scan(&priorDigest)
	if err == nil {
		if priorDigest != digest {
			return ErrIdempotencyConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if op.Phase != "commit_authorized" || in.CommittedOwnerUserID != op.TargetUserID || in.CommittedAt.Before(op.Cutoff) {
		return ErrConflict
	}
	if err := requireSourceResponsibilityTx(ctx, tx, account, op); err != nil {
		return err
	}
	grant, err := scanCommitAuthorization(tx.QueryRow(ctx, `SELECT `+commitAuthorizationColumns+` FROM billing_handoff_commit_authorizations WHERE operation_id=$1`, op.ID))
	if err != nil {
		return err
	}
	// Logical authorization binding establishes ordering. Comparing timestamps
	// from the AM and Billing hosts would reject valid commits under clock skew.
	if grant.AuthorizationID != in.AuthorizationID {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_handoff_committed_decisions
		(operation_id,request_sha256,am_commit_sha256,committed_ownership_version,committed_at)
		VALUES ($1,$2,$3,$4,$5)`, op.ID, digest, in.AMCommitSHA256, in.CommittedOwnershipVersion, in.CommittedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE billing_ownership_handoffs SET phase='finalizing',version=version+1,updated_at=now() WHERE id=$1`, op.ID); err != nil {
		return err
	}
	if err := handoffProtocolAuditTx(ctx, tx, in.Scope, "commit_observed", in.AuthorizationID, digest); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type BeginHandoffAbortInput struct {
	Scope                HandoffScope
	CancellationID       string
	AMCancellationSHA256 string
	AuthorizationID      string
}

// This receipt must be produced by Account Manager's precommit cancellation
// compare-and-swap. An AM committed decision can never produce a cancellation
// receipt, even if Billing has not received its finalize message yet.
func (s *Store) BeginOwnershipHandoffAbort(ctx context.Context, in BeginHandoffAbortInput) (HandoffProtocolAck, error) {
	if !validHandoffScope(in.Scope) || !canonicalUUID(in.CancellationID) || !validLowerSHA256(in.AMCancellationSHA256) ||
		(in.AuthorizationID != "" && !canonicalUUID(in.AuthorizationID)) {
		return HandoffProtocolAck{}, ErrConflict
	}
	digest, err := handoffDigest(in)
	if err != nil {
		return HandoffProtocolAck{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return HandoffProtocolAck{}, err
	}
	defer tx.Rollback(ctx)
	account, op, err := lockHandoffOperationTx(ctx, tx, in.Scope)
	if err != nil {
		return HandoffProtocolAck{}, err
	}
	var priorDigest string
	err = tx.QueryRow(ctx, `SELECT request_sha256 FROM billing_handoff_cancellations WHERE operation_id=$1`, op.ID).Scan(&priorDigest)
	if err == nil {
		if priorDigest != digest {
			return HandoffProtocolAck{}, ErrIdempotencyConflict
		}
		return HandoffProtocolAck{OperationID: op.ID, Phase: op.Phase}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return HandoffProtocolAck{}, err
	}
	if op.Phase != "preparing" && op.Phase != "prepared" && op.Phase != "commit_authorized" {
		return HandoffProtocolAck{}, ErrConflict
	}
	if err := requireSourceResponsibilityTx(ctx, tx, account, op); err != nil {
		return HandoffProtocolAck{}, err
	}
	var grantID string
	err = tx.QueryRow(ctx, `SELECT authorization_id::text FROM billing_handoff_commit_authorizations WHERE operation_id=$1`, op.ID).Scan(&grantID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return HandoffProtocolAck{}, err
	}
	if grantID != in.AuthorizationID {
		return HandoffProtocolAck{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_handoff_cancellations(operation_id,cancellation_id,request_sha256,am_cancellation_sha256)
		VALUES ($1,$2,$3,$4)`, op.ID, in.CancellationID, digest, in.AMCancellationSHA256); err != nil {
		return HandoffProtocolAck{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE billing_ownership_handoffs SET phase='abort_pending',version=version+1,updated_at=now() WHERE id=$1`, op.ID); err != nil {
		return HandoffProtocolAck{}, err
	}
	if err := handoffProtocolAuditTx(ctx, tx, in.Scope, "abort_requested", in.CancellationID, digest); err != nil {
		return HandoffProtocolAck{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HandoffProtocolAck{}, err
	}
	return HandoffProtocolAck{OperationID: op.ID, Phase: "abort_pending"}, nil
}

type CompleteHandoffAbortInput struct {
	Scope             HandoffScope
	CancellationID    string
	HoldReleaseSHA256 string
}

func (s *Store) CompleteOwnershipHandoffAbort(ctx context.Context, in CompleteHandoffAbortInput) (HandoffProtocolAck, error) {
	if !validHandoffScope(in.Scope) || !canonicalUUID(in.CancellationID) || !validLowerSHA256(in.HoldReleaseSHA256) {
		return HandoffProtocolAck{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return HandoffProtocolAck{}, err
	}
	defer tx.Rollback(ctx)
	account, op, err := lockHandoffOperationTx(ctx, tx, in.Scope)
	if err != nil {
		return HandoffProtocolAck{}, err
	}
	var cancellationID string
	if err := tx.QueryRow(ctx, `SELECT cancellation_id::text FROM billing_handoff_cancellations WHERE operation_id=$1`, op.ID).Scan(&cancellationID); err != nil {
		return HandoffProtocolAck{}, mapNotFound(err)
	}
	if cancellationID != in.CancellationID {
		return HandoffProtocolAck{}, ErrIdempotencyConflict
	}
	var priorRelease string
	err = tx.QueryRow(ctx, `SELECT hold_release_sha256 FROM billing_handoff_abort_acknowledgments WHERE operation_id=$1`, op.ID).Scan(&priorRelease)
	if err == nil {
		if priorRelease != in.HoldReleaseSHA256 {
			return HandoffProtocolAck{}, ErrIdempotencyConflict
		}
		return HandoffProtocolAck{OperationID: op.ID, Phase: "aborted"}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return HandoffProtocolAck{}, err
	}
	if op.Phase != "abort_pending" {
		return HandoffProtocolAck{}, ErrConflict
	}
	if err := requireSourceResponsibilityTx(ctx, tx, account, op); err != nil {
		return HandoffProtocolAck{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_handoff_abort_acknowledgments(operation_id,hold_release_sha256) VALUES ($1,$2)`, op.ID, in.HoldReleaseSHA256); err != nil {
		return HandoffProtocolAck{}, err
	}
	// Do not restore saved policy/method snapshots: revocations or disabled
	// policies that happened while held must remain revoked/disabled.
	if _, err := tx.Exec(ctx, `UPDATE billing_ownership_handoffs SET phase='aborted',version=version+1,updated_at=now() WHERE id=$1`, op.ID); err != nil {
		return HandoffProtocolAck{}, err
	}
	if err := handoffProtocolAuditTx(ctx, tx, in.Scope, "abort_completed", in.CancellationID, in.HoldReleaseSHA256); err != nil {
		return HandoffProtocolAck{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HandoffProtocolAck{}, err
	}
	return HandoffProtocolAck{OperationID: op.ID, Phase: "aborted"}, nil
}

func handoffProtocolAuditTx(ctx context.Context, tx pgx.Tx, scope HandoffScope, event, requestID, digest string) error {
	_, err := tx.Exec(ctx, `INSERT INTO billing_audit_events
		(organization_id,event_type,actor_type,actor_id,subject_type,subject_id,request_id,payload)
		VALUES ($1,$2,'service','billing_handoff_coordinator','ownership_handoff',$3,$4,
		jsonb_build_object('ownership_version',$5::bigint,'evidence_sha256',$6::text))`,
		scope.OrganizationID, "billing.ownership_handoff."+event, scope.OperationID, requestID, scope.OwnershipVersion, digest)
	return err
}
