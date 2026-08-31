package paymentstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestOwnershipEligibilityIsReadOnlyAndAcceptsNonnegativeCredit(t *testing.T) {
	for _, balance := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprint(balance), func(t *testing.T) {
			env, account, scope := cloudPreflightFixture(t, balance)
			ctx := context.Background()
			in := OwnershipEligibilityRequest{CloudID: scope.OrganizationID, SourceUserID: scope.OwnerUserID, TargetUserID: testutil.OrganizationID("eligibility-target"), OwnershipVersion: 1, Action: "request"}
			missing, err := env.store.CheckOwnershipEligibility(ctx, in)
			if err != nil || missing.Complete || missing.ReceiptID != "" || missing.EvidenceSHA256 != "" || !slices.Contains(missing.Blockers, "evidence_unavailable") {
				t.Fatalf("missing evidence approved: %+v %v", missing, err)
			}
			receipt := cloudPreflightReceipt(t, env, scope, "eligibility-receipt")
			if err := env.store.RecordCloudPreflightEvidence(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			var before, after string
			snapshot := `SELECT jsonb_build_object('account',(SELECT to_jsonb(a) FROM commercial_accounts a WHERE id=$1),'holds',(SELECT count(*) FROM billing_ownership_handoffs),'closures',(SELECT count(*) FROM billing_cloud_closures),'receipts',(SELECT count(*) FROM billing_cloud_preflight_receipts),'audit',(SELECT count(*) FROM billing_audit_events),'periods',(SELECT count(*) FROM billing_responsibility_periods))::text`
			if err := env.db.QueryRow(ctx, snapshot, account.ID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			request, err := env.store.CheckOwnershipEligibility(ctx, in)
			if err != nil || !request.Complete || request.Request != in || request.BalanceMinor != balance || request.ReceiptID != receipt.ReceiptID || !validLowerSHA256(request.EvidenceSHA256) || (len(request.Blockers) == 0) != (balance >= 0) {
				t.Fatalf("request: %+v %v", request, err)
			}
			if balance < 0 && !slices.Contains(request.Blockers, "balance_negative") {
				t.Fatalf("negative balance not explicit: %+v", request)
			}
			in.Action, in.TransferID = "accept", testutil.OrganizationID("eligibility-transfer")
			accept, err := New(env.db).CheckOwnershipEligibility(ctx, in)
			if err != nil || !accept.Complete || accept.Request != in || accept.EvidenceSHA256 == request.EvidenceSHA256 {
				t.Fatalf("acceptance binding lost: %+v %v", accept, err)
			}
			if err := env.db.QueryRow(ctx, snapshot, account.ID).Scan(&after); err != nil || before != after {
				t.Fatalf("advisory query mutated state: %v", err)
			}
			deletion, err := env.store.GetCloudDeletionPreflight(ctx, scope)
			if err != nil || deletion.Eligible != (balance == 0) {
				t.Fatalf("transfer weakened deletion: %+v %v", deletion, err)
			}
			// An intervening local balance change invalidates the collector receipt.
			if _, err := env.store.PostLedgerEntry(ctx, PostLedgerEntryInput{AccountID: account.ID, Direction: payment.LedgerDirectionCredit, Reason: payment.LedgerReasonManualAdjustmentCredit, AmountMinor: 1, Currency: payment.CurrencyTWD, IdempotencyScope: "test", IdempotencyKey: "eligibility-change", ActorType: "service", ActorID: "test", RequestID: "eligibility-change"}); err != nil {
				t.Fatal(err)
			}
			stale, err := env.store.CheckOwnershipEligibility(ctx, in)
			if err != nil || stale.Complete || !slices.Contains(stale.Blockers, "evidence_unavailable") {
				t.Fatalf("stale evidence approved: %+v %v", stale, err)
			}
		})
	}
}

func TestOwnershipEligibilityRetainsBlockersAndRejectsInvalidScope(t *testing.T) {
	env, _, scope := cloudPreflightFixture(t, 1)
	ctx := context.Background()
	in := OwnershipEligibilityRequest{CloudID: scope.OrganizationID, SourceUserID: scope.OwnerUserID, TargetUserID: testutil.OrganizationID("target"), OwnershipVersion: 1, Action: "request"}
	receipt := cloudPreflightReceipt(t, env, scope, "eligibility-blocked")
	receipt.State.Financial.UnpaidInvoiceCount = 1
	receipt.State.Financial.PendingPaymentCount = 1
	receipt.State.Financial.PendingRefundCount = 1
	receipt.State.Financial.OpenDisputeCount = 1
	if err := env.store.RecordCloudPreflightEvidence(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	out, err := env.store.CheckOwnershipEligibility(ctx, in)
	if err != nil || !out.Complete {
		t.Fatalf("read blockers: %+v %v", out, err)
	}
	for _, code := range []string{"debt_outstanding", "payment_pending", "refund_pending", "dispute_pending"} {
		if !slices.Contains(out.Blockers, code) {
			t.Fatalf("positive balance hid %s: %+v", code, out)
		}
	}
	for _, mutate := range []func(*OwnershipEligibilityRequest){
		func(r *OwnershipEligibilityRequest) { r.Action = "commit" }, func(r *OwnershipEligibilityRequest) { r.Action = "accept" }, func(r *OwnershipEligibilityRequest) { r.TransferID = testutil.OrganizationID("unexpected") }, func(r *OwnershipEligibilityRequest) { r.TargetUserID = r.SourceUserID }, func(r *OwnershipEligibilityRequest) { r.CloudID = "bad" }, func(r *OwnershipEligibilityRequest) { r.OwnershipVersion = 0 },
	} {
		bad := in
		mutate(&bad)
		if _, err := env.store.CheckOwnershipEligibility(ctx, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("bad scope: %+v %v", bad, err)
		}
	}
	wrong := in
	wrong.SourceUserID = testutil.OrganizationID("not-owner")
	if _, err := env.store.CheckOwnershipEligibility(ctx, wrong); !errors.Is(err, ErrOwnershipVersionConflict) {
		t.Fatalf("wrong owner: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := env.store.CheckOwnershipEligibility(canceled, in); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request: %v", err)
	}
	if out.ObservedAt.After(time.Now()) || !out.ExpiresAt.After(out.ObservedAt) || out.ExpiresAt.After(out.ObservedAt.Add(5*time.Minute)) {
		t.Fatalf("invalid lifetime: %+v", out)
	}
}
