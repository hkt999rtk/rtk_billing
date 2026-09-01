package paymentstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func newSettlementFixture(t *testing.T, balance int64) (paymentIntegrationEnv, payment.CommercialAccount, PrepareOwnershipHandoffInput, HandoffScope) {
	t.Helper()
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	account, _, err := env.store.EnsureCommercialAccount(ctx, testutil.OrganizationID(t.Name()), payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 0 {
		direction, reason, amount := payment.LedgerDirectionCredit, payment.LedgerReasonManualAdjustmentCredit, balance
		if balance < 0 {
			direction, reason, amount = payment.LedgerDirectionDebit, payment.LedgerReasonManualAdjustmentDebit, -balance
		}
		result, err := env.store.PostLedgerEntry(ctx, PostLedgerEntryInput{
			AccountID: account.ID, Direction: direction, Reason: reason, AmountMinor: amount, Currency: payment.CurrencyTWD,
			IdempotencyScope: "test", IdempotencyKey: "initial", ActorType: "service", ActorID: "test", RequestID: "initial",
		})
		if err != nil {
			t.Fatal(err)
		}
		account = result.Account
	}
	prepare := handoffInput(t, env, account)
	if _, err := env.store.PrepareOwnershipHandoff(ctx, prepare); err != nil {
		t.Fatal(err)
	}
	return env, account, prepare, HandoffScope{OrganizationID: account.OrganizationID, OperationID: prepare.OperationID, OwnershipVersion: prepare.OwnershipVersion}
}

// Synthetic collector evidence for protocol tests; not a production collector
// and not proof of cross-service/provider cutoff completeness.
func settlementReceipt(t *testing.T, env paymentIntegrationEnv, scope HandoffScope, name string) RecordSettlementInput {
	t.Helper()
	state, err := env.store.CaptureHandoffSettlementState(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	state.Financial.UsageSettled, state.Financial.InvoicesReconciled, state.Financial.ProviderWorkReconciled = true, true, true
	return RecordSettlementInput{
		Scope: scope, ReceiptID: testutil.OrganizationID(name), StateSHA256: state.SHA256, Financial: state.Financial,
		UsageCheckpointSHA256: strings.Repeat("a", 64), InvoiceCheckpointSHA256: strings.Repeat("b", 64), ProviderCheckpointSHA256: strings.Repeat("c", 64),
	}
}

func snapshotConfirmation(scope HandoffScope, actor string, snapshot *HandoffBalanceSnapshot) ConfirmHandoffSnapshotInput {
	return ConfirmHandoffSnapshotInput{Scope: scope, UserID: actor, SnapshotVersion: snapshot.Version, BalanceMinor: snapshot.BalanceMinor, Currency: snapshot.Currency}
}

func TestHandoffSettledSnapshotRequiresNonnegativeBalance(t *testing.T) {
	for _, balance := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprint(balance), func(t *testing.T) {
			env, _, _, scope := newSettlementFixture(t, balance)
			in := settlementReceipt(t, env, scope, "receipt")
			status, err := env.store.RecordHandoffSettlement(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if balance < 0 {
				if status.Snapshot != nil || !slices.Contains(status.Blockers, "balance_negative") || status.Phase != "preparing" {
					t.Fatalf("negative credit must not produce confirmable amount: %+v", status)
				}
			} else if status.Snapshot == nil || status.Snapshot.BalanceMinor != balance || len(status.Blockers) != 0 || status.Phase != "prepared" {
				t.Fatalf("zero/positive settled credit: %+v", status)
			}
			replay, err := New(env.db).RecordHandoffSettlement(context.Background(), in)
			if err != nil || replay.Phase != status.Phase || (replay.Snapshot == nil) != (status.Snapshot == nil) {
				t.Fatalf("durable replay=%+v err=%v", replay, err)
			}
			var count int
			if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM billing_handoff_settlement_receipts`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("receipt count=%d err=%v", count, err)
			}
		})
	}
}

func TestHandoffSettlementRequiresCompleteCheckpointsAndIndependentFinancialClearance(t *testing.T) {
	env, _, _, scope := newSettlementFixture(t, 100)
	ctx := context.Background()
	status, err := env.store.GetHandoffSettlementStatus(ctx, scope)
	if err != nil || status.Snapshot != nil || !slices.Contains(status.Blockers, "settlement_evidence_missing") {
		t.Fatalf("zero rows is not completeness: %+v err=%v", status, err)
	}
	for _, test := range []struct {
		code   string
		change func(*RecordSettlementInput)
	}{
		{"usage_unsettled", func(in *RecordSettlementInput) { in.Financial.UsageSettled = false; in.UsageCheckpointSHA256 = "" }},
		{"invoice_evidence_missing", func(in *RecordSettlementInput) {
			in.Financial.InvoicesReconciled = false
			in.InvoiceCheckpointSHA256 = ""
		}},
		{"provider_evidence_missing", func(in *RecordSettlementInput) {
			in.Financial.ProviderWorkReconciled = false
			in.ProviderCheckpointSHA256 = ""
		}},
		{"outstanding_debt", func(in *RecordSettlementInput) { in.Financial.DebtMinor = 1 }},
		{"refunds_pending", func(in *RecordSettlementInput) { in.Financial.PendingRefundCount = 1 }},
		{"disputes_open", func(in *RecordSettlementInput) { in.Financial.OpenDisputeCount = 1 }},
	} {
		in := settlementReceipt(t, env, scope, test.code)
		test.change(&in)
		got, err := env.store.RecordHandoffSettlement(ctx, in)
		if err != nil || got.Snapshot != nil || !slices.Contains(got.Blockers, test.code) {
			t.Fatalf("positive credit masked %s: %+v err=%v", test.code, got, err)
		}
	}
	in := settlementReceipt(t, env, scope, "false-completeness")
	in.ProviderCheckpointSHA256 = ""
	if _, err := env.store.RecordHandoffSettlement(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("true flag without checkpoint: %v", err)
	}
	in = settlementReceipt(t, env, scope, "invalid-count")
	in.Financial.PendingRefundCount = -1
	if _, err := env.store.RecordHandoffSettlement(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("negative count: %v", err)
	}
}

func TestHandoffSnapshotReconfirmationAndStaleAmount(t *testing.T) {
	env, account, prepare, scope := newSettlementFixture(t, 1)
	ctx := context.Background()
	firstReceipt := settlementReceipt(t, env, scope, "first")
	first, err := env.store.RecordHandoffSettlement(ctx, firstReceipt)
	if err != nil || first.Snapshot == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	confirmation := snapshotConfirmation(scope, prepare.SourceUserID, first.Snapshot)
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, confirmation); err != nil {
		t.Fatal(err)
	}
	changed := firstReceipt
	changed.ProviderCheckpointSHA256 = strings.Repeat("d", 64)
	if _, err := env.store.RecordHandoffSettlement(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("receipt cannot change on retry: %v", err)
	}
	// Settlement can change positive credit to zero without releasing the fence.
	if _, err := env.store.PostLedgerEntry(ctx, debitInput(account.ID, "late-cutoff", 1, testTime(13, 0))); err != nil {
		t.Fatal(err)
	}
	stale, err := env.store.GetHandoffSettlementStatus(ctx, scope)
	if err != nil || stale.Snapshot != nil || !slices.Contains(stale.Blockers, "settlement_evidence_stale") {
		t.Fatalf("old snapshot still visible: %+v err=%v", stale, err)
	}
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, confirmation); !errors.Is(err, ErrHandoffNotConfirmable) {
		t.Fatalf("even source's old confirmation replay must revalidate: %v", err)
	}
	changed = firstReceipt
	changed.ReceiptID = testutil.OrganizationID("stale-capture-new-id")
	if _, err := env.store.RecordHandoffSettlement(ctx, changed); !errors.Is(err, ErrSettlementEvidenceStale) {
		t.Fatalf("new receipt ID cannot make stale evidence fresh: %v", err)
	}
	nextReceipt := settlementReceipt(t, env, scope, "next")
	next, err := env.store.RecordHandoffSettlement(ctx, nextReceipt)
	if err != nil || next.Snapshot == nil || next.Snapshot.BalanceMinor != 0 || next.Snapshot.Version <= first.Snapshot.Version || next.Snapshot.SourceConfirmed {
		t.Fatalf("zero-credit replacement=%+v err=%v", next, err)
	}
	for _, actor := range []string{prepare.SourceUserID, prepare.TargetUserID} {
		if _, err := env.store.ConfirmHandoffSnapshot(ctx, snapshotConfirmation(scope, actor, next.Snapshot)); err != nil {
			t.Fatal(err)
		}
	}
	// A new evidence revision at the SAME amount also needs new confirmations.
	third, err := env.store.RecordHandoffSettlement(ctx, settlementReceipt(t, env, scope, "same-amount-new-evidence"))
	if err != nil || third.Snapshot == nil || third.Snapshot.SourceConfirmed || third.Snapshot.TargetConfirmed {
		t.Fatalf("old approvals revived for new version: %+v err=%v", third, err)
	}
	if _, err := env.store.PostLedgerEntry(ctx, debitInput(account.ID, "negative-cutoff", 1, testTime(14, 0))); err != nil {
		t.Fatal(err)
	}
	negative, err := env.store.RecordHandoffSettlement(ctx, settlementReceipt(t, env, scope, "negative-after-confirmation"))
	if err != nil || negative.Snapshot != nil || negative.Phase != "preparing" || !slices.Contains(negative.Blockers, "balance_negative") {
		t.Fatalf("negative cutoff reused an earlier nonnegative snapshot: %+v err=%v", negative, err)
	}
}

func TestHandoffNewUsageInvalidatesConfirmedSnapshotWithoutBalanceChange(t *testing.T) {
	env, account, prepare, scope := newSettlementFixture(t, 0)
	ctx := context.Background()
	status, err := env.store.RecordHandoffSettlement(ctx, settlementReceipt(t, env, scope, "usage-before"))
	if err != nil || status.Snapshot == nil {
		t.Fatalf("snapshot=%+v err=%v", status, err)
	}
	for _, actor := range []string{prepare.SourceUserID, prepare.TargetUserID} {
		if _, err := env.store.ConfirmHandoffSnapshot(ctx, snapshotConfirmation(scope, actor, status.Snapshot)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO billing_usage_facts
		(usage_id,organization_id,service_code,metric_code,quantity,unit,window_start,window_end,source,source_sha256)
		VALUES ('late-usage',$1,'mqtt','message',1,'message',$2,$3,'test',$4)`, account.OrganizationID,
		prepare.Cutoff.Add(-time.Hour), prepare.Cutoff, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	got, err := env.store.GetHandoffSettlementStatus(ctx, scope)
	if err != nil || got.Snapshot != nil || !slices.Contains(got.Blockers, "settlement_evidence_stale") {
		t.Fatalf("unrated late usage reused zero balance: %+v err=%v", got, err)
	}
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, snapshotConfirmation(scope, prepare.TargetUserID, status.Snapshot)); !errors.Is(err, ErrHandoffNotConfirmable) {
		t.Fatalf("late usage didn't invalidate target replay: %v", err)
	}
}

func TestHandoffLocalPendingPaymentCannotBeHiddenByCollectorZeros(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "settlement-local-blocker", 20000, 10000, 50000)
	ctx := context.Background()
	pending, err := env.store.CreateManualTopUp(ctx, CreateManualTopUpInput{
		AccountID: fixture.account.ID, PaymentMethodID: fixture.method.ID, AmountMinor: 50000,
		Currency: payment.CurrencyTWD, IdempotencyKey: "in-flight", CorrelationID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	prepare := handoffInput(t, env, fixture.account)
	if _, err := env.store.PrepareOwnershipHandoff(ctx, prepare); err != nil {
		t.Fatal(err)
	}
	scope := HandoffScope{OrganizationID: prepare.OrganizationID, OperationID: prepare.OperationID, OwnershipVersion: prepare.OwnershipVersion}
	in := settlementReceipt(t, env, scope, "hide-pending")
	in.Financial.PendingPaymentCount = 0
	status, err := env.store.RecordHandoffSettlement(ctx, in)
	if err != nil || status.Snapshot != nil || !slices.Contains(status.Blockers, "payments_pending") {
		t.Fatalf("local payment was masked: %+v err=%v", status, err)
	}
	for _, state := range []payment.PaymentIntentState{payment.PaymentIntentStateProcessing, payment.PaymentIntentStateFailed} {
		if _, err := env.store.TransitionIntent(ctx, TransitionIntentInput{IntentID: pending.Intent.ID, ToState: state}); err != nil {
			t.Fatal(err)
		}
	}
	// A failed reconciliation job needs resolution even if the intent itself is
	// terminal. Neither its failure nor a supplied zero refund count settles it.
	if _, err := env.db.Exec(ctx, `UPDATE payment_reconciliation_jobs SET reason='refund',status='failed' WHERE intent_id=$1`, pending.Intent.ID); err != nil {
		t.Fatal(err)
	}
	in = settlementReceipt(t, env, scope, "hide-failed-refund")
	in.Financial.PendingPaymentCount, in.Financial.PendingRefundCount = 0, 0
	status, err = env.store.RecordHandoffSettlement(ctx, in)
	if err != nil || status.Snapshot != nil || !slices.Contains(status.Blockers, "payments_pending") || !slices.Contains(status.Blockers, "refunds_pending") {
		t.Fatalf("failed reconciliation treated as complete: %+v err=%v", status, err)
	}
}

func TestHandoffSnapshotConfirmationScopeAndConcurrency(t *testing.T) {
	env, _, prepare, scope := newSettlementFixture(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := env.store.RecordHandoffSettlement(ctx, settlementReceipt(t, env, scope, "confirmation"))
	if err != nil || status.Snapshot == nil {
		t.Fatalf("snapshot=%+v err=%v", status, err)
	}
	in := snapshotConfirmation(scope, testutil.OrganizationID("stranger"), status.Snapshot)
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, in); !errors.Is(err, ErrHandoffParticipant) {
		t.Fatalf("nonparticipant: %v", err)
	}
	in.UserID = prepare.SourceUserID
	in.Scope.OrganizationID = testutil.OrganizationID("different-cloud")
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong cloud: %v", err)
	}
	in.Scope = scope
	in.Scope.OwnershipVersion++
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, in); !errors.Is(err, ErrOwnershipVersionConflict) {
		t.Fatalf("wrong ownership version: %v", err)
	}
	in.Scope = scope
	in.BalanceMinor++
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, in); !errors.Is(err, ErrHandoffNotConfirmable) {
		t.Fatalf("wrong amount: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		actor := prepare.SourceUserID
		if i%2 == 0 {
			actor = prepare.TargetUserID
		}
		wg.Add(1)
		go func(actor string) {
			defer wg.Done()
			_, err := env.store.ConfirmHandoffSnapshot(ctx, snapshotConfirmation(scope, actor, status.Snapshot))
			results <- err
		}(actor)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	confirmed, err := env.store.GetHandoffSettlementStatus(ctx, scope)
	if err != nil || confirmed.Snapshot == nil || !confirmed.Snapshot.SourceConfirmed || !confirmed.Snapshot.TargetConfirmed || confirmed.Phase != "prepared" {
		t.Fatalf("both confirmations must not switch owner: %+v err=%v", confirmed, err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM billing_handoff_confirmations WHERE operation_id=$1`, scope.OperationID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("confirmations=%d err=%v", count, err)
	}
	// Simulates the coordinator entering cancellation, not an abort implementation.
	if _, err := env.db.Exec(ctx, `UPDATE billing_ownership_handoffs SET phase='abort_pending' WHERE id=$1`, scope.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, snapshotConfirmation(scope, prepare.SourceUserID, status.Snapshot)); !errors.Is(err, ErrHandoffNotConfirmable) {
		t.Fatalf("canceling operation accepted stale confirmation: %v", err)
	}
}

func TestHandoffSettlementValidationWithoutDatabase(t *testing.T) {
	s := &Store{}
	if _, err := s.RecordHandoffSettlement(context.Background(), RecordSettlementInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing receipt: %v", err)
	}
	if _, err := s.ConfirmHandoffSnapshot(context.Background(), ConfirmHandoffSnapshotInput{Currency: billing.CurrencyTWD}); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing confirmation: %v", err)
	}
}

func TestHandoffLocalInvoicesAndSettlementLinksAreChecked(t *testing.T) {
	env, account, _, scope := newSettlementFixture(t, 100)
	ctx := context.Background()
	var invoiceID string
	if err := env.db.QueryRow(ctx, `WITH pricing AS (
		INSERT INTO pricing_plan_versions(plan_key,version,currency,effective_from,created_by)
		VALUES ('handoff-test',1,'TWD',$3,'test') RETURNING id
	), period AS (
		INSERT INTO billing_periods(organization_id,currency,period_start,period_end)
		VALUES ($1,'TWD',$3,$4) RETURNING id
	)
	INSERT INTO billing_invoices(invoice_number,organization_id,account_id,period_id,pricing_version_id,currency,
		state,period_start,period_end,subtotal_minor,total_minor,amount_due_minor,recipient_snapshot,issued_at)
	SELECT 'handoff-invoice',$1,$2,period.id,pricing.id,'TWD','issued',$3,$4,10,10,10,'{}'::jsonb,now()
	FROM pricing,period RETURNING id::text`, account.OrganizationID, account.ID, testTime(8, 0), testTime(9, 0)).Scan(&invoiceID); err != nil {
		t.Fatal(err)
	}
	in := settlementReceipt(t, env, scope, "invoice-due")
	in.Financial.UnpaidInvoiceCount, in.Financial.DebtMinor = 0, 0
	status, err := env.store.RecordHandoffSettlement(ctx, in)
	if err != nil || status.Snapshot != nil || !slices.Contains(status.Blockers, "unpaid_invoices") || !slices.Contains(status.Blockers, "outstanding_debt") {
		t.Fatalf("local invoice hidden: %+v err=%v", status, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE billing_invoices SET state='settled',amount_due_minor=0,amount_settled_minor=10 WHERE id=$1`, invoiceID); err != nil {
		t.Fatal(err)
	}
	in = settlementReceipt(t, env, scope, "missing-ledger-settlement")
	in.Financial.UnpaidInvoiceCount = 0
	status, err = env.store.RecordHandoffSettlement(ctx, in)
	if err != nil || status.Snapshot != nil || !slices.Contains(status.Blockers, "unpaid_invoices") {
		t.Fatalf("settled label without ledger evidence accepted: %+v err=%v", status, err)
	}
}

func TestHandoffSnapshotAuditFailureRollsBackAndEvidenceIsImmutable(t *testing.T) {
	env, _, prepare, scope := newSettlementFixture(t, 0)
	ctx := context.Background()
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events ADD CONSTRAINT test_settlement_audit_failure
		CHECK (event_type NOT IN ('billing.ownership_handoff.settlement','billing.ownership_handoff.confirm')) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := env.db.Exec(context.Background(), `ALTER TABLE billing_audit_events DROP CONSTRAINT IF EXISTS test_settlement_audit_failure`); err != nil {
			t.Error(err)
		}
	})
	in := settlementReceipt(t, env, scope, "audit-failure")
	if _, err := env.store.RecordHandoffSettlement(ctx, in); err == nil {
		t.Fatal("receipt without durable audit accepted")
	}
	op, err := env.store.GetOwnershipHandoff(ctx, scope.OrganizationID, scope.OperationID)
	if err != nil || op.Phase != "preparing" || op.Version != 1 {
		t.Fatalf("failed receipt partially advanced operation: %+v err=%v", op, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events DROP CONSTRAINT test_settlement_audit_failure`); err != nil {
		t.Fatal(err)
	}
	status, err := env.store.RecordHandoffSettlement(ctx, in)
	if err != nil || status.Snapshot == nil {
		t.Fatalf("retry receipt=%+v err=%v", status, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events ADD CONSTRAINT test_settlement_audit_failure
		CHECK (event_type <> 'billing.ownership_handoff.confirm') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	confirmation := snapshotConfirmation(scope, prepare.SourceUserID, status.Snapshot)
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, confirmation); err == nil {
		t.Fatal("confirmation without durable audit accepted")
	}
	got, err := env.store.GetHandoffSettlementStatus(ctx, scope)
	if err != nil || got.Snapshot == nil || got.Snapshot.SourceConfirmed {
		t.Fatalf("failed confirmation leaked: %+v err=%v", got, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events DROP CONSTRAINT test_settlement_audit_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, confirmation); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE billing_handoff_settlement_receipts SET state_sha256=repeat('d',64) WHERE operation_id=$1`,
		`UPDATE billing_handoff_balance_snapshots SET balance_minor=99 WHERE operation_id=$1`,
		`DELETE FROM billing_handoff_confirmations WHERE operation_id=$1`,
	} {
		if _, err := env.db.Exec(ctx, query, scope.OperationID); err == nil {
			t.Fatalf("historical evidence is mutable: %s", query)
		}
	}
}
