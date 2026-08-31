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
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

var (
	ErrOwnershipEvidenceMissing = errors.New("billing ownership evidence is missing")
	ErrOwnershipVersionConflict = errors.New("billing ownership version conflict")
	ErrHandoffFenced            = errors.New("billing ownership handoff monetary fence is active")
	ErrSetupInvalidated         = errors.New("payment method setup was invalidated by ownership handoff")
)

// InitialResponsibilityInput is supplied only by an authenticated provisioning
// or reviewed migration workflow. A today's-owner lookup is not historical proof.
// This store is intentionally not exposed through a tenant HTTP route.
type InitialResponsibilityInput struct {
	AccountID            string
	OwnerUserID          string
	OwnershipVersion     int64
	EffectiveFrom        time.Time
	SourceEvidenceSHA256 string
}

func (s *Store) InitializeResponsibility(ctx context.Context, in InitialResponsibilityInput) error {
	if !canonicalUUID(in.AccountID) || !canonicalUUID(in.OwnerUserID) || in.OwnershipVersion < 1 ||
		in.EffectiveFrom.IsZero() || !validLowerSHA256(in.SourceEvidenceSHA256) {
		return ErrConflict
	}
	in.EffectiveFrom = databaseTime(in.EffectiveFrom)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	account, err := getAccountForUpdate(ctx, tx, in.AccountID)
	if err != nil {
		return err
	}
	if account.State == payment.AccountStateClosed {
		return ErrAccountClosed
	}
	var owner, evidence string
	var version int64
	var from time.Time
	var until *time.Time
	err = tx.QueryRow(ctx, `SELECT owner_user_id::text, ownership_version, effective_from,
		effective_until, source_evidence_sha256 FROM billing_responsibility_periods
		WHERE account_id=$1 ORDER BY ownership_version LIMIT 1`, in.AccountID).
		Scan(&owner, &version, &from, &until, &evidence)
	if err == nil {
		if owner != in.OwnerUserID || version != in.OwnershipVersion || !from.Equal(in.EffectiveFrom) || evidence != in.SourceEvidenceSHA256 {
			return ErrIdempotencyConflict
		}
		// Replay never reopens a period or replaces a subsequent owner.
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_responsibility_periods
		(account_id, owner_user_id, ownership_version, effective_from, source_evidence_sha256)
		VALUES ($1,$2,$3,$4,$5)`, in.AccountID, in.OwnerUserID, in.OwnershipVersion,
		in.EffectiveFrom, in.SourceEvidenceSHA256)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_audit_events
		(organization_id,event_type,actor_type,actor_id,subject_type,subject_id,request_id,payload)
		VALUES ($1,'billing.responsibility.initialize','service','billing_handoff_coordinator',
		'commercial_account',$2,$3,jsonb_build_object('owner_user_id',$4::text,'ownership_version',$5::bigint,'source_evidence_sha256',$3::text))`,
		account.OrganizationID, account.ID, in.SourceEvidenceSHA256, in.OwnerUserID, in.OwnershipVersion); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type PrepareOwnershipHandoffInput struct {
	OperationID      string
	OrganizationID   string
	SourceUserID     string
	TargetUserID     string
	OwnershipVersion int64
	Cutoff           time.Time
}

type OwnershipHandoff struct {
	ID               string    `json:"id"`
	AccountID        string    `json:"account_id"`
	SourceUserID     string    `json:"source_user_id"`
	TargetUserID     string    `json:"target_user_id"`
	OwnershipVersion int64     `json:"ownership_version"`
	Cutoff           time.Time `json:"cutoff"`
	Phase            string    `json:"phase"`
	Version          int64     `json:"version"`
	CreatedAt        time.Time `json:"created_at"`
	RequestSHA256    string    `json:"-"`
}

// PrepareOwnershipHandoff only installs a durable fence. It cannot produce a
// confirmable snapshot or authorize ownership commit. Reconciled cutoff evidence,
// both confirmations and the later commit protocol are separate prerequisites.
// Account Manager must already have authenticated the participants and operation.
func (s *Store) PrepareOwnershipHandoff(ctx context.Context, in PrepareOwnershipHandoffInput) (OwnershipHandoff, error) {
	if !canonicalUUID(in.OperationID) || !canonicalUUID(in.OrganizationID) || !canonicalUUID(in.SourceUserID) ||
		!canonicalUUID(in.TargetUserID) || in.SourceUserID == in.TargetUserID || in.OwnershipVersion < 1 || in.Cutoff.IsZero() {
		return OwnershipHandoff{}, ErrConflict
	}
	in.Cutoff = databaseTime(in.Cutoff)
	digest, err := handoffDigest(in)
	if err != nil {
		return OwnershipHandoff{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return OwnershipHandoff{}, err
	}
	defer tx.Rollback(ctx)
	account, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+`
		FROM commercial_accounts WHERE organization_id=$1 AND currency='TWD' FOR UPDATE`, in.OrganizationID))
	if err != nil {
		return OwnershipHandoff{}, err
	}
	previous, err := scanHandoff(tx.QueryRow(ctx, `SELECT `+handoffColumns+` FROM billing_ownership_handoffs WHERE id=$1`, in.OperationID))
	if err == nil {
		if previous.AccountID != account.ID || previous.RequestSHA256 != digest {
			return OwnershipHandoff{}, ErrIdempotencyConflict
		}
		// A replay returns persisted state, not a newly prepared operation.
		return previous, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return OwnershipHandoff{}, err
	}
	if account.State == payment.AccountStateClosed {
		return OwnershipHandoff{}, ErrAccountClosed
	}
	if err := requireNoHandoffTx(ctx, tx, account.ID); err != nil {
		return OwnershipHandoff{}, err
	}
	var periodID, ownerID string
	var ownershipVersion int64
	var effectiveFrom time.Time
	err = tx.QueryRow(ctx, `SELECT id::text, owner_user_id::text, ownership_version, effective_from
		FROM billing_responsibility_periods WHERE account_id=$1 AND effective_until IS NULL`, account.ID).
		Scan(&periodID, &ownerID, &ownershipVersion, &effectiveFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return OwnershipHandoff{}, ErrOwnershipEvidenceMissing
	}
	if err != nil {
		return OwnershipHandoff{}, err
	}
	if ownerID != in.SourceUserID || ownershipVersion != in.OwnershipVersion || in.Cutoff.Before(effectiveFrom) {
		return OwnershipHandoff{}, ErrOwnershipVersionConflict
	}
	op, err := scanHandoff(tx.QueryRow(ctx, `INSERT INTO billing_ownership_handoffs
		(id,account_id,source_period_id,source_user_id,target_user_id,ownership_version,request_sha256,cutoff)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+handoffColumns,
		in.OperationID, account.ID, periodID, in.SourceUserID, in.TargetUserID, in.OwnershipVersion, digest, in.Cutoff))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return OwnershipHandoff{}, ErrIdempotencyConflict
		}
		return OwnershipHandoff{}, err
	}
	// Serialize on the account with setup creation/completion. Invalidation is
	// permanent and does not pretend to cancel an external provider operation.
	if _, err := tx.Exec(ctx, `UPDATE payment_method_setup_sessions
		SET invalidated_by_handoff=$2, state='failed'
		WHERE account_id=$1 AND state IN ('created','requires_action','unknown') AND invalidated_by_handoff IS NULL`, account.ID, op.ID); err != nil {
		return OwnershipHandoff{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_methods SET status='revoked'
		WHERE id IN (SELECT payment_method_id FROM payment_method_setup_sessions WHERE invalidated_by_handoff=$1)`, op.ID); err != nil {
		return OwnershipHandoff{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_consents SET revoked_at=COALESCE(revoked_at,now()),
		revocation_reason=COALESCE(revocation_reason,'ownership_handoff_setup_invalidated')
		WHERE id IN (SELECT m.consent_id FROM payment_methods m JOIN payment_method_setup_sessions s ON s.payment_method_id=m.id
		WHERE s.invalidated_by_handoff=$1)`, op.ID); err != nil {
		return OwnershipHandoff{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_audit_events
		(organization_id,event_type,actor_type,actor_id,subject_type,subject_id,request_id,payload)
		VALUES ($1,'billing.ownership_handoff.prepare','service','billing_handoff_coordinator',
		'ownership_handoff',$2,$2,jsonb_build_object('source_user_id',$3::text,'target_user_id',$4::text,
		'ownership_version',$5::bigint,'request_sha256',$6::text))`,
		in.OrganizationID, op.ID, in.SourceUserID, in.TargetUserID, in.OwnershipVersion, digest); err != nil {
		return OwnershipHandoff{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OwnershipHandoff{}, err
	}
	return op, nil
}

// Require both identifiers: an operation UUID alone never grants another cloud's status.
func (s *Store) GetOwnershipHandoff(ctx context.Context, organizationID, operationID string) (OwnershipHandoff, error) {
	if !canonicalUUID(organizationID) || !canonicalUUID(operationID) {
		return OwnershipHandoff{}, ErrConflict
	}
	return scanHandoff(s.db.QueryRow(ctx, `SELECT `+handoffColumns+` FROM billing_ownership_handoffs
		WHERE id=$1 AND account_id IN (SELECT id FROM commercial_accounts WHERE organization_id=$2)`, operationID, organizationID))
}

// All callers hold the commercial account FOR UPDATE for the whole mutation.
// Abort_pending remains fenced; timeout, a positive balance, and active commercial
// access state are never substitutes for acknowledged cancellation/finalization.
func requireNoHandoffTx(ctx context.Context, tx pgx.Tx, accountID string) error {
	var fenced bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM billing_ownership_handoffs
		WHERE account_id=$1 AND phase NOT IN ('finalized','aborted'))`, accountID).Scan(&fenced)
	if err != nil {
		return err
	}
	if fenced {
		return ErrHandoffFenced
	}
	return nil
}

func canonicalUUID(value string) bool {
	var id pgtype.UUID
	if err := id.Scan(value); err != nil || !id.Valid || id.Bytes == [16]byte{} {
		return false
	}
	return id.String() == value
}

func databaseTime(value time.Time) time.Time { return value.UTC().Truncate(time.Microsecond) }

func handoffDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

const handoffColumns = `id::text,account_id::text,source_user_id::text,target_user_id::text,
	ownership_version,cutoff,phase,version,created_at,request_sha256`

func scanHandoff(row rowScanner) (OwnershipHandoff, error) {
	var op OwnershipHandoff
	err := row.Scan(&op.ID, &op.AccountID, &op.SourceUserID, &op.TargetUserID, &op.OwnershipVersion,
		&op.Cutoff, &op.Phase, &op.Version, &op.CreatedAt, &op.RequestSHA256)
	return op, mapNotFound(err)
}
