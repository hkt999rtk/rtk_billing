package paymentstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func cloudPreflightFixture(t *testing.T, balance int64) (paymentIntegrationEnv, payment.CommercialAccount, CloudPreflightScope) {
	t.Helper()
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	account, _, err := env.store.EnsureCommercialAccount(ctx, testutil.OrganizationID(t.Name()), payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	owner := testutil.OrganizationID("preflight-owner")
	if err := env.store.InitializeResponsibility(ctx, InitialResponsibilityInput{AccountID: account.ID, OwnerUserID: owner, OwnershipVersion: 1, EffectiveFrom: time.Now().Add(-time.Hour), SourceEvidenceSHA256: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	if balance != 0 {
		direction, reason, amount := payment.LedgerDirectionCredit, payment.LedgerReasonManualAdjustmentCredit, balance
		if balance < 0 {
			direction, reason, amount = payment.LedgerDirectionDebit, payment.LedgerReasonManualAdjustmentDebit, -balance
		}
		posted, err := env.store.PostLedgerEntry(ctx, PostLedgerEntryInput{AccountID: account.ID, Direction: direction, Reason: reason, AmountMinor: amount, Currency: payment.CurrencyTWD, IdempotencyScope: "test", IdempotencyKey: "opening", ActorType: "service", ActorID: "test", RequestID: "opening"})
		if err != nil {
			t.Fatal(err)
		}
		account = posted.Account
	}
	return env, account, CloudPreflightScope{OrganizationID: account.OrganizationID, OwnerUserID: owner, OwnershipVersion: 1}
}

// Explicit synthetic collector checkpoint, not production reconciliation proof.
func cloudPreflightReceipt(t *testing.T, env paymentIntegrationEnv, scope CloudPreflightScope, name string) RecordCloudPreflightInput {
	t.Helper()
	state, err := env.store.CaptureCloudPreflightState(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	state.Financial.UsageSettled = true
	state.Financial.InvoicesReconciled = true
	state.Financial.ProviderWorkReconciled = true
	return RecordCloudPreflightInput{State: state, ReceiptID: testutil.OrganizationID(name), UsageCheckpointSHA256: strings.Repeat("a", 64), InvoiceCheckpointSHA256: strings.Repeat("b", 64), ProviderCheckpointSHA256: strings.Repeat("c", 64), ExpiresAt: state.ObservedAt.Add(time.Minute)}
}

func TestCloudDeletionPreflightZeroBalanceAndIndependentEvidence(t *testing.T) {
	for _, balance := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprint(balance), func(t *testing.T) {
			env, _, scope := cloudPreflightFixture(t, balance)
			ctx := context.Background()
			missing, err := env.store.GetCloudDeletionPreflight(ctx, scope)
			if err != nil || missing.Eligible || !slices.Contains(missing.Blockers, "evidence_unavailable") {
				t.Fatalf("empty tables imply settled: %+v %v", missing, err)
			}
			receipt := cloudPreflightReceipt(t, env, scope, "receipt")
			if err := env.store.RecordCloudPreflightEvidence(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			if err := New(env.db).RecordCloudPreflightEvidence(ctx, receipt); err != nil {
				t.Fatalf("replay: %v", err)
			}
			result, err := New(env.db).GetCloudDeletionPreflight(ctx, scope)
			if err != nil || result.Eligible != (balance == 0) || result.BalanceMinor != balance || slices.Contains(result.Blockers, "evidence_unavailable") {
				t.Fatalf("financial result: %+v %v", result, err)
			}
			if balance != 0 && !slices.Contains(result.Blockers, "balance_nonzero") {
				t.Fatalf("nonzero not blocked: %+v", result)
			}
			var count int
			if err := env.db.QueryRow(ctx, `SELECT count(*) FROM billing_cloud_preflight_receipts`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("receipt duplication: %d %v", count, err)
			}
			if _, err := env.db.Exec(ctx, `UPDATE billing_cloud_preflight_receipts SET expires_at=expires_at`); err == nil {
				t.Fatal("mutable receipt")
			}
			receipt.ExpiresAt = receipt.ExpiresAt.Add(time.Second)
			if err := env.store.RecordCloudPreflightEvidence(ctx, receipt); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("key changed: %v", err)
			}
		})
	}
}

func TestCloudPreflightRejectsStaleScopeAndReconciledState(t *testing.T) {
	env, account, scope := cloudPreflightFixture(t, 0)
	ctx := context.Background()
	receipt := cloudPreflightReceipt(t, env, scope, "stale-receipt")
	for _, bad := range []CloudPreflightScope{{OrganizationID: scope.OrganizationID, OwnerUserID: testutil.OrganizationID("other"), OwnershipVersion: 1}, {OrganizationID: scope.OrganizationID, OwnerUserID: scope.OwnerUserID, OwnershipVersion: 2}} {
		if _, err := env.store.GetCloudDeletionPreflight(ctx, bad); !errors.Is(err, ErrOwnershipVersionConflict) {
			t.Fatalf("wrong owner/version: %v", err)
		}
	}
	if err := env.store.RecordCloudPreflightEvidence(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PostLedgerEntry(ctx, PostLedgerEntryInput{AccountID: account.ID, Direction: payment.LedgerDirectionCredit, Reason: payment.LedgerReasonManualAdjustmentCredit, AmountMinor: 1, Currency: payment.CurrencyTWD, IdempotencyScope: "test", IdempotencyKey: "change", ActorType: "service", ActorID: "test", RequestID: "change"}); err != nil {
		t.Fatal(err)
	}
	result, err := env.store.GetCloudDeletionPreflight(ctx, scope)
	if err != nil || result.Eligible || !slices.Contains(result.Blockers, "evidence_unavailable") || !slices.Contains(result.Blockers, "balance_nonzero") {
		t.Fatalf("old receipt reused: %+v %v", result, err)
	}
	receipt.ReceiptID = testutil.OrganizationID("after-change")
	if err := env.store.RecordCloudPreflightEvidence(ctx, receipt); !errors.Is(err, ErrSettlementEvidenceStale) {
		t.Fatalf("stale collector accepted: %v", err)
	}
	receipt = cloudPreflightReceipt(t, env, scope, "bad-proof")
	receipt.UsageCheckpointSHA256 = ""
	if err := env.store.RecordCloudPreflightEvidence(ctx, receipt); !errors.Is(err, ErrConflict) {
		t.Fatalf("complete without checkpoint: %v", err)
	}
	receipt = cloudPreflightReceipt(t, env, scope, "expired")
	receipt.State.ObservedAt = receipt.State.ObservedAt.Add(-time.Hour)
	receipt.ExpiresAt = receipt.State.ObservedAt.Add(time.Minute)
	if err := env.store.RecordCloudPreflightEvidence(ctx, receipt); !errors.Is(err, ErrSettlementEvidenceStale) {
		t.Fatalf("expired proof: %v", err)
	}
}

func TestCloudPreflightRetainsIndependentFinancialBlockers(t *testing.T) {
	env, _, scope := cloudPreflightFixture(t, 0)
	ctx := context.Background()
	in := cloudPreflightReceipt(t, env, scope, "blocked")
	in.State.Financial.UnpaidInvoiceCount = 1
	in.State.Financial.PendingPaymentCount = 1
	in.State.Financial.PendingRefundCount = 1
	in.State.Financial.OpenDisputeCount = 1
	if err := env.store.RecordCloudPreflightEvidence(ctx, in); err != nil {
		t.Fatal(err)
	}
	result, err := env.store.GetCloudDeletionPreflight(ctx, scope)
	if err != nil || result.Eligible {
		t.Fatalf("zero bypassed blockers: %+v %v", result, err)
	}
	for _, code := range []string{"debt_outstanding", "payment_pending", "refund_pending", "dispute_pending"} {
		if !slices.Contains(result.Blockers, code) {
			t.Fatalf("missing %s: %+v", code, result)
		}
	}
}
