package paymentstore

import (
	"context"
	"time"
)

type OwnershipEligibilityRequest struct {
	CloudID          string `json:"cloud_id"`
	SourceUserID     string `json:"source_user_id"`
	TargetUserID     string `json:"target_user_id"`
	TransferID       string `json:"transfer_id"`
	Action           string `json:"action"`
	OwnershipVersion int64  `json:"ownership_version"`
}

func (in OwnershipEligibilityRequest) Valid() bool {
	return canonicalUUID(in.CloudID) && canonicalUUID(in.SourceUserID) && canonicalUUID(in.TargetUserID) && in.SourceUserID != in.TargetUserID && in.OwnershipVersion > 0 &&
		((in.Action == "request" && in.TransferID == "") || (in.Action == "accept" && canonicalUUID(in.TransferID)))
}

type OwnershipEligibility struct {
	Request        OwnershipEligibilityRequest `json:"request"`
	ReceiptID      string                      `json:"receipt_id"`
	EvidenceSHA256 string                      `json:"evidence_sha256"`
	Currency       string                      `json:"currency"`
	BalanceMinor   int64                       `json:"balance_minor"`
	Complete       bool                        `json:"complete"`
	Blockers       []string                    `json:"blockers"`
	ObservedAt     time.Time                   `json:"observed_at"`
	ExpiresAt      time.Time                   `json:"expires_at"`
}

// Advisory read only. AM authenticates the target; Billing checks the source's
// current responsibility and independently recorded financial evidence.
func (s *Store) CheckOwnershipEligibility(ctx context.Context, in OwnershipEligibilityRequest) (OwnershipEligibility, error) {
	if !in.Valid() {
		return OwnershipEligibility{}, ErrConflict
	}
	view, err := s.getCloudFinancialPreflight(ctx, CloudPreflightScope{OrganizationID: in.CloudID, OwnerUserID: in.SourceUserID, OwnershipVersion: in.OwnershipVersion}, true)
	if err != nil {
		return OwnershipEligibility{}, err
	}
	out := OwnershipEligibility{Request: in, ReceiptID: view.receiptID, Currency: view.Currency, BalanceMinor: view.BalanceMinor, Complete: view.complete, Blockers: view.Blockers, ObservedAt: view.ObservedAt, ExpiresAt: view.ExpiresAt}
	if view.receiptID != "" {
		out.EvidenceSHA256, err = handoffDigest(struct {
			Request                  OwnershipEligibilityRequest
			ReceiptID, ReceiptSHA256 string
		}{in, view.receiptID, view.receiptSHA256})
	}
	return out, err
}
