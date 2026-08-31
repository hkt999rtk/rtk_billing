package paymentstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

func TestHandoffAndClosureValidationFailClosedBeforeDatabaseAccess(t *testing.T) {
	s := New(nil)
	ctx := context.Background()
	checks := []error{}
	_, err := s.CaptureHandoffSettlementState(ctx, HandoffScope{})
	checks = append(checks, err)
	_, err = s.RecordHandoffSettlement(ctx, RecordSettlementInput{})
	checks = append(checks, err)
	_, err = s.GetHandoffSettlementStatus(ctx, HandoffScope{})
	checks = append(checks, err)
	_, err = s.ConfirmHandoffSnapshot(ctx, ConfirmHandoffSnapshotInput{})
	checks = append(checks, err)
	_, err = s.AuthorizeHandoffCommit(ctx, AuthorizeHandoffCommitInput{})
	checks = append(checks, err)
	_, err = s.FinalizeOwnershipHandoff(ctx, FinalizeHandoffInput{})
	checks = append(checks, err)
	_, err = s.BeginOwnershipHandoffAbort(ctx, BeginHandoffAbortInput{})
	checks = append(checks, err)
	_, err = s.CompleteOwnershipHandoffAbort(ctx, CompleteHandoffAbortInput{})
	checks = append(checks, err)
	_, err = s.CaptureCloudPreflightState(ctx, CloudPreflightScope{})
	checks = append(checks, err)
	checks = append(checks, s.RecordCloudPreflightEvidence(ctx, RecordCloudPreflightInput{}))
	_, err = s.GetCloudDeletionPreflight(ctx, CloudPreflightScope{})
	checks = append(checks, err)
	_, err = s.PrepareCloudClosure(ctx, PrepareCloudClosureInput{})
	checks = append(checks, err)
	_, err = s.GetCloudClosureStatus(ctx, CloudClosureScope{})
	checks = append(checks, err)
	_, err = s.CloseCloud(ctx, CloseCloudInput{})
	checks = append(checks, err)
	_, err = s.CancelCloudClosure(ctx, CloudClosureScope{}, "", "")
	checks = append(checks, err)
	_, err = s.RetireCloudClose(ctx, CloseCloudInput{})
	checks = append(checks, err)
	for i, err := range checks {
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("guard %d returned %v", i, err)
		}
	}
}

func TestHandoffScopeCheckpointsAndEvidenceMerge(t *testing.T) {
	scope := HandoffScope{OrganizationID: "11111111-1111-1111-1111-111111111111", OperationID: "22222222-2222-2222-2222-222222222222", OwnershipVersion: 1}
	if !validHandoffScope(scope) || validHandoffScope(HandoffScope{}) {
		t.Fatal("handoff scope validation")
	}
	preflight := CloudPreflightScope{OrganizationID: scope.OrganizationID, OwnerUserID: "33333333-3333-3333-3333-333333333333", OwnershipVersion: 1}
	if !preflight.valid() || !((CloudClosureScope{CloudPreflightScope: preflight, OperationID: scope.OperationID}).valid()) || (CloudClosureScope{}).valid() {
		t.Fatal("cloud lifecycle scope validation")
	}
	digest := strings.Repeat("a", 64)
	if !validCheckpoint(true, digest) || !validCheckpoint(false, "") || validCheckpoint(true, "") || validCheckpoint(false, "bad") {
		t.Fatal("checkpoint validation")
	}
	external := billing.FinancialEvidence{BalanceKnown: false, Currency: "USD", BalanceMinor: -100, UnpaidInvoiceCount: 1, DebtMinor: 2, PendingPaymentCount: 3, PendingRefundCount: 4, PendingSetupCount: 5, UnresolvedProviderEventCount: 6}
	local := billing.FinancialEvidence{BalanceKnown: true, Currency: billing.CurrencyTWD, BalanceMinor: 7, UnpaidInvoiceCount: 2, DebtMinor: 1, PendingPaymentCount: 4, PendingRefundCount: 3, PendingSetupCount: 6, UnresolvedProviderEventCount: 5}
	got := mergeSettlementEvidence(external, local)
	if !got.BalanceKnown || got.Currency != billing.CurrencyTWD || got.BalanceMinor != 7 || got.UnpaidInvoiceCount != 2 || got.DebtMinor != 2 || got.PendingPaymentCount != 4 || got.PendingRefundCount != 4 || got.PendingSetupCount != 6 || got.UnresolvedProviderEventCount != 6 {
		t.Fatalf("merged evidence: %+v", got)
	}
}
