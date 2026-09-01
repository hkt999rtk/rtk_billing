package paymentstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// CloudCreation is an immutable event emitted by AM in the new cloud/owner
// transaction. It is not a current-owner lookup or a legacy migration command.
type CloudCreation struct {
	EventID          string    `json:"event_id"`
	OrganizationID   string    `json:"cloud_id"`
	OwnerUserID      string    `json:"owner_user_id"`
	OwnershipVersion int64     `json:"ownership_version"`
	OccurredAt       time.Time `json:"occurred_at"`
	EvidenceSHA256   string    `json:"evidence_sha256"`
}

func (in CloudCreation) EvidenceDigest() string {
	// UUIDs have no newlines; UTC fixed microseconds are identical to the AM
	// event timestamp stored in PostgreSQL. No caller formatting enters the hash.
	raw := strings.Join([]string{"brand-cloud-created-v1", in.EventID, in.OrganizationID, in.OwnerUserID, "1", in.OccurredAt.UTC().Format("2006-01-02T15:04:05.000000Z")}, "\n")
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}
func (in CloudCreation) Valid() bool {
	return canonicalUUID(in.EventID) && canonicalUUID(in.OrganizationID) && canonicalUUID(in.OwnerUserID) && in.OwnershipVersion == 1 && !in.OccurredAt.IsZero() && in.OccurredAt.Equal(databaseTime(in.OccurredAt)) && in.EvidenceSHA256 == in.EvidenceDigest()
}

type CloudCreationReceipt struct {
	CloudCreation
	AccountID string `json:"account_id"`
}

func (s *Store) BootstrapBrandCloud(ctx context.Context, in CloudCreation) (CloudCreationReceipt, error) {
	if !in.Valid() {
		return CloudCreationReceipt{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CloudCreationReceipt{}, err
	}
	defer tx.Rollback(ctx)
	// No account row exists yet. Serialize only this cloud, not all bootstrap.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('billing-cloud-create:'||$1::text,0))`, in.OrganizationID); err != nil {
		return CloudCreationReceipt{}, err
	}
	var out CloudCreationReceipt
	err = tx.QueryRow(ctx, `SELECT event_id::text,organization_id::text,owner_user_id::text,ownership_version,occurred_at,evidence_sha256,account_id::text FROM billing_cloud_creation_receipts WHERE organization_id=$1 OR event_id=$2`, in.OrganizationID, in.EventID).Scan(&out.EventID, &out.OrganizationID, &out.OwnerUserID, &out.OwnershipVersion, &out.OccurredAt, &out.EvidenceSHA256, &out.AccountID)
	if err == nil {
		if out.EventID != in.EventID || out.OrganizationID != in.OrganizationID || out.OwnerUserID != in.OwnerUserID || out.OwnershipVersion != in.OwnershipVersion || !out.OccurredAt.Equal(in.OccurredAt) || out.EvidenceSHA256 != in.EvidenceSHA256 {
			return CloudCreationReceipt{}, ErrIdempotencyConflict
		}
		// Receipt replay remains valid after transfer/deletion. Never reopen a
		// period, change balance, activate access or overwrite the current owner.
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CloudCreationReceipt{}, err
	}
	var occurred bool
	if err := tx.QueryRow(ctx, `SELECT $1::timestamptz<=clock_timestamp()`, in.OccurredAt).Scan(&occurred); err != nil {
		return CloudCreationReceipt{}, err
	}
	if !occurred {
		return CloudCreationReceipt{}, ErrConflict
	}
	var accountID string
	err = tx.QueryRow(ctx, `INSERT INTO commercial_accounts(organization_id,currency) VALUES($1,'TWD') ON CONFLICT(organization_id,currency) DO NOTHING RETURNING id::text`, in.OrganizationID).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudCreationReceipt{}, ErrOwnershipEvidenceMissing
	}
	if err != nil {
		return CloudCreationReceipt{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_responsibility_periods(account_id,owner_user_id,ownership_version,effective_from,source_evidence_sha256) VALUES($1,$2,1,$3,$4)`, accountID, in.OwnerUserID, in.OccurredAt, in.EvidenceSHA256); err != nil {
		return CloudCreationReceipt{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_cloud_creation_receipts(event_id,organization_id,account_id,owner_user_id,ownership_version,occurred_at,evidence_sha256) VALUES($1,$2,$3,$4,1,$5,$6)`, in.EventID, in.OrganizationID, accountID, in.OwnerUserID, in.OccurredAt, in.EvidenceSHA256); err != nil {
		return CloudCreationReceipt{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_audit_events(organization_id,event_type,actor_type,actor_id,subject_type,subject_id,request_id,payload) VALUES($1,'billing.cloud_creation.bootstrap','service','account_manager_cloud_creation','commercial_account',$2,$3,jsonb_build_object('owner_user_id',$4::text,'ownership_version',1,'evidence_sha256',$5::text))`, in.OrganizationID, accountID, in.EventID, in.OwnerUserID, in.EvidenceSHA256); err != nil {
		return CloudCreationReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CloudCreationReceipt{}, err
	}
	return CloudCreationReceipt{CloudCreation: in, AccountID: accountID}, nil
}
