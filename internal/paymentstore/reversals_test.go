package paymentstore

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

// Synthetic paid-provider fixtures exercise local accounting, not signature
// validation or producer/provider settlement completeness.
func paidReversalIntent(t *testing.T, env paymentIntegrationEnv, fixture paymentFixture) payment.PaymentIntent {
	t.Helper()
	ctx := context.Background()
	created, err := env.store.CreateManualTopUp(ctx, CreateManualTopUpInput{
		AccountID: fixture.account.ID, PaymentMethodID: fixture.method.ID, AmountMinor: 10000, Currency: payment.CurrencyTWD,
		IdempotencyKey: "original-paid-intent", CorrelationID: "original-paid", Now: testTime(10, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []payment.PaymentIntentState{payment.PaymentIntentStateProcessing, payment.PaymentIntentStateSucceeded} {
		if _, err := env.store.TransitionIntent(ctx, TransitionIntentInput{IntentID: created.Intent.ID, ToState: state, ProviderTransactionReference: "paid-" + created.Intent.ID, Now: testTime(10, 1)}); err != nil {
			t.Fatal(err)
		}
	}
	// This test supplies terminal reconciliation evidence explicitly, not a real worker.
	if _, err := env.db.Exec(ctx, `UPDATE payment_reconciliation_jobs SET status='completed' WHERE intent_id=$1`, created.Intent.ID); err != nil {
		t.Fatal(err)
	}
	return created.Intent
}

func reversalInput(accountID, intentID, event string) RecordProviderReversalInput {
	return RecordProviderReversalInput{AccountID: accountID, OriginalIntentID: intentID, Provider: "fake", EventReference: event,
		AmountMinor: 100, Currency: payment.CurrencyTWD, Reason: payment.LedgerReasonRefundDebit, ProviderPayloadSHA256: strings.Repeat("e", 64), RequestID: "verified-worker"}
}

func TestProviderReversalRequiresDedicatedPathAndValidEvidence(t *testing.T) {
	s := &Store{}
	for _, reason := range []payment.LedgerReason{payment.LedgerReasonRefundDebit, payment.LedgerReasonChargebackDebit} {
		in := debitInput(testutil.OrganizationID("account"), "refund", 100, testTime(12, 0))
		in.Reason = reason
		if _, err := s.PostLedgerEntry(context.Background(), in); !errors.Is(err, ErrProviderReversalRequired) {
			t.Fatalf("generic reversal: %v", err)
		}
	}
	for _, mutate := range []func(*RecordProviderReversalInput){
		func(in *RecordProviderReversalInput) { in.AccountID = "invalid" },
		func(in *RecordProviderReversalInput) { in.OriginalIntentID = "missing" },
		func(in *RecordProviderReversalInput) { in.Provider = " " },
		func(in *RecordProviderReversalInput) { in.EventReference = strings.Repeat("x", 201) },
		func(in *RecordProviderReversalInput) { in.ProviderPayloadSHA256 = "not-proof" },
		func(in *RecordProviderReversalInput) { in.Reason = payment.LedgerReasonInvoiceDebit },
		func(in *RecordProviderReversalInput) { in.RequestID = " " },
	} {
		in := reversalInput(testutil.OrganizationID("a"), testutil.OrganizationID("i"), "e")
		mutate(&in)
		if _, err := s.RecordProviderReversal(context.Background(), in); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid input: %v", err)
		}
	}
}

func TestProviderReversalConcurrencyCapAndCrossCloudIsolation(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	f := createPaymentFixture(t, env, "reversal-concurrency", 20000, 10000, 50000)
	_ = handoffInput(t, env, f.account)
	i := paidReversalIntent(t, env, f)
	in := reversalInput(f.account.ID, i.ID, "same-event")
	in.AmountMinor = 6000
	var wg sync.WaitGroup
	results := make([]ProviderReversalResult, 8)
	errs := make([]error, 8)
	for n := range results {
		wg.Add(1)
		go func(n int) { defer wg.Done(); results[n], errs[n] = env.store.RecordProviderReversal(ctx, in) }(n)
	}
	wg.Wait()
	duplicates := 0
	for n, r := range results {
		if errs[n] != nil || r.Disposition != "current_balance" || r.Entry == nil || r.Account.AvailableBalanceMinor != 24000 {
			t.Fatalf("result=%+v err=%v", r, errs[n])
		}
		if r.Duplicate {
			duplicates++
		}
		if r.EventID != results[0].EventID {
			t.Fatal("duplicate event IDs")
		}
	}
	if duplicates != 7 {
		t.Fatalf("duplicates=%d", duplicates)
	}
	changed := in
	changed.ProviderPayloadSHA256 = strings.Repeat("f", 64)
	if _, err := env.store.RecordProviderReversal(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed event: %v", err)
	}
	other := createPaymentFixture(t, env, "reversal-other-cloud", 20000, 10000, 50000)
	changed = in
	changed.AccountID = other.account.ID
	if _, err := env.store.RecordProviderReversal(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("global provider ID reused: %v", err)
	}
	changed.EventReference = "cross-cloud-original"
	r, err := env.store.RecordProviderReversal(ctx, changed)
	if err != nil || r.ReviewReason != "original_payment_unresolved" || r.Account.AvailableBalanceMinor != 20000 {
		t.Fatalf("cross-cloud=%+v err=%v", r, err)
	}
	in.EventReference = "remaining-chargeback"
	in.Reason = payment.LedgerReasonChargebackDebit
	in.AmountMinor = 4000
	r, err = env.store.RecordProviderReversal(ctx, in)
	if err != nil || r.Disposition != "current_balance" || r.Account.AvailableBalanceMinor != 20000 {
		t.Fatalf("remaining=%+v err=%v", r, err)
	}
	in.EventReference = "excess-refund"
	in.AmountMinor = 1
	r, err = env.store.RecordProviderReversal(ctx, in)
	if err != nil || r.ReviewReason != "reversal_exceeds_original_credit" || r.Entry != nil || r.Account.AvailableBalanceMinor != 20000 {
		t.Fatalf("excess=%+v err=%v", r, err)
	}
}

func TestProviderReversalLegacyEvidenceRequiresReviewedBinding(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	f := createPaymentFixture(t, env, "legacy-reversal", 20000, 10000, 50000)
	i := paidReversalIntent(t, env, f) // No responsibility period at creation.
	p := handoffInput(t, env, f.account)
	var period string
	if err := env.db.QueryRow(ctx, `SELECT id::text FROM billing_responsibility_periods WHERE account_id=$1`, f.account.ID).Scan(&period); err != nil {
		t.Fatal(err)
	}
	in := reversalInput(f.account.ID, i.ID, "legacy-event")
	r, err := env.store.RecordProviderReversal(ctx, in)
	if err != nil || r.ReviewReason != "payment_responsibility_unproven" || r.Account.AvailableBalanceMinor != 30000 {
		t.Fatalf("unproven=%+v err=%v", r, err)
	}
	resolution := ResolveProviderReversalInput{AccountID: f.account.ID, EventID: r.EventID, EvidenceSHA256: strings.Repeat("b", 64), RequestID: "reviewed-resolution"}
	if _, err := env.store.ResolveProviderReversal(ctx, resolution); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown attribution forced: %v", err)
	}
	binding := BindHistoricalPaymentResponsibilityInput{AccountID: f.account.ID, IntentID: i.ID, PeriodID: period, EvidenceSHA256: strings.Repeat("c", 64), ReviewerUserID: p.SourceUserID, RequestID: "historical-review"}
	wrong := binding
	wrong.PeriodID = testutil.OrganizationID("not-a-period")
	if err := env.store.BindHistoricalPaymentResponsibility(ctx, wrong); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown period: %v", err)
	}
	for n := 0; n < 2; n++ {
		if err := env.store.BindHistoricalPaymentResponsibility(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}
	wrong = binding
	wrong.EvidenceSHA256 = strings.Repeat("d", 64)
	if err := env.store.BindHistoricalPaymentResponsibility(ctx, wrong); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed binding: %v", err)
	}
	r, err = env.store.ResolveProviderReversal(ctx, resolution)
	if err != nil || r.Disposition != "current_balance" || r.Account.AvailableBalanceMinor != 29900 {
		t.Fatalf("resolved=%+v err=%v", r, err)
	}
	if replay, err := env.store.ResolveProviderReversal(ctx, resolution); err != nil || !replay.Duplicate || replay.Entry == nil || replay.Entry.ID != r.Entry.ID {
		t.Fatalf("resolution replay=%+v err=%v", replay, err)
	}
	if replay, err := env.store.RecordProviderReversal(ctx, in); err != nil || !replay.Duplicate || replay.Disposition != "current_balance" || replay.ReviewReason != "" {
		t.Fatalf("receipt replay after resolution=%+v err=%v", replay, err)
	}
	for _, table := range []string{"billing_payment_responsibility", "billing_provider_reversal_events", "billing_provider_reversal_reviews", "billing_provider_reversal_allocations"} {
		if _, err := env.db.Exec(ctx, `DELETE FROM `+table); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("immutable %s: %v", table, err)
		}
	}
}

func TestProviderReversalPredecessorAndReturningOwnerNeverDebitCurrentBalance(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	f := createPaymentFixture(t, env, "late-reversal", 20000, 10000, 50000)
	p := handoffInput(t, env, f.account)
	i := paidReversalIntent(t, env, f)
	var originalPeriod string
	if err := env.db.QueryRow(ctx, `SELECT period_id::text FROM billing_payment_responsibility WHERE intent_id=$1`, i.ID).Scan(&originalPeriod); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if _, err := env.store.PrepareOwnershipHandoff(ctx, p); err != nil {
			t.Fatal(err)
		}
		scope := HandoffScope{OrganizationID: p.OrganizationID, OperationID: p.OperationID, OwnershipVersion: p.OwnershipVersion}
		grant := authorizeFixtureHandoff(t, env, p, scope)
		finish := finalizeFixtureInput(scope, p.TargetUserID, grant)
		if _, err := env.store.FinalizeOwnershipHandoff(ctx, finish); err != nil {
			t.Fatal(err)
		}
		before, err := env.store.GetCommercialAccount(ctx, f.account.ID)
		if err != nil {
			t.Fatal(err)
		}
		policyBefore, err := env.store.GetAutoTopUpPolicy(ctx, f.account.ID)
		if err != nil {
			t.Fatal(err)
		}
		event := "late-first"
		if n == 1 {
			event = "late-returning"
		}
		r, err := env.store.RecordProviderReversal(ctx, reversalInput(f.account.ID, i.ID, event))
		if err != nil || r.Disposition != "predecessor_adjustment" || r.PeriodID != originalPeriod || r.Entry != nil || r.Account.Version != before.Version || r.Account.AvailableBalanceMinor != before.AvailableBalanceMinor {
			t.Fatalf("late n=%d result=%+v err=%v", n, r, err)
		}
		policyAfter, err := env.store.GetAutoTopUpPolicy(ctx, f.account.ID)
		if err != nil || policyAfter.Version != policyBefore.Version {
			t.Fatalf("old refund changed current policy: %v", err)
		}
		p = PrepareOwnershipHandoffInput{OperationID: testutil.OrganizationID("return-owner"), OrganizationID: p.OrganizationID, SourceUserID: p.TargetUserID, TargetUserID: p.SourceUserID, OwnershipVersion: p.OwnershipVersion + 1, Cutoff: finish.CommittedAt.Add(time.Microsecond)}
	}
	if _, err := env.db.Exec(ctx, `UPDATE commercial_accounts SET state='closed' WHERE id=$1`, f.account.ID); err != nil {
		t.Fatal(err)
	}
	r, err := env.store.RecordProviderReversal(ctx, reversalInput(f.account.ID, i.ID, "after-closure"))
	if err != nil || r.Disposition != "predecessor_adjustment" || r.Account.AvailableBalanceMinor != 30000 {
		t.Fatalf("closed historical adjustment=%+v err=%v", r, err)
	}
}

func TestProviderReversalDuringCommitRetainsFenceAndResolvesForward(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	f := createPaymentFixture(t, env, "commit-reversal", 20000, 10000, 50000)
	p := handoffInput(t, env, f.account)
	i := paidReversalIntent(t, env, f)
	if _, err := env.store.PrepareOwnershipHandoff(ctx, p); err != nil {
		t.Fatal(err)
	}
	scope := HandoffScope{OrganizationID: p.OrganizationID, OperationID: p.OperationID, OwnershipVersion: p.OwnershipVersion}
	grant := authorizeFixtureHandoff(t, env, p, scope)
	r, err := env.store.RecordProviderReversal(ctx, reversalInput(f.account.ID, i.ID, "racing-with-commit"))
	if err != nil || r.ReviewReason != "ownership_commit_in_progress" || r.Account.AvailableBalanceMinor != 30000 {
		t.Fatalf("commit review=%+v err=%v", r, err)
	}
	finish := finalizeFixtureInput(scope, p.TargetUserID, grant)
	if _, err := env.store.FinalizeOwnershipHandoff(ctx, finish); !errors.Is(err, ErrSettlementEvidenceStale) {
		t.Fatalf("unresolved event must stop finalize: %v", err)
	}
	var phase string
	if err := env.db.QueryRow(ctx, `SELECT phase FROM billing_ownership_handoffs WHERE id=$1`, p.OperationID).Scan(&phase); err != nil || phase != "finalizing" {
		t.Fatalf("phase=%s err=%v", phase, err)
	}
	if _, err := env.store.PostLedgerEntry(ctx, debitInput(f.account.ID, "still-fenced", 1, testTime(14, 0))); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("fence lost: %v", err)
	}
	resolved, err := env.store.ResolveProviderReversal(ctx, ResolveProviderReversalInput{AccountID: f.account.ID, EventID: r.EventID, EvidenceSHA256: strings.Repeat("c", 64), RequestID: "AM-commit-observed"})
	if err != nil || resolved.Disposition != "predecessor_adjustment" || resolved.Entry != nil || resolved.Account.AvailableBalanceMinor != 30000 {
		t.Fatalf("forward resolution=%+v err=%v", resolved, err)
	}
	if ack, err := env.store.FinalizeOwnershipHandoff(ctx, finish); err != nil || ack.Phase != "finalized" {
		t.Fatalf("forward finalize=%+v err=%v", ack, err)
	}
}

func TestProviderReversalReviewCannotBeMaskedByCollectorZero(t *testing.T) {
	env, account, _, scope := newSettlementFixture(t, 100)
	r, err := env.store.RecordProviderReversal(context.Background(), reversalInput(account.ID, testutil.OrganizationID("unresolved"), "unknown"))
	if err != nil || r.Disposition != "review" {
		t.Fatalf("review=%+v err=%v", r, err)
	}
	in := settlementReceipt(t, env, scope, "cannot-mask")
	in.Financial.UnresolvedProviderEventCount = 0
	status, err := env.store.RecordHandoffSettlement(context.Background(), in)
	if err != nil || status.Snapshot != nil || !slices.Contains(status.Blockers, "provider_events_unresolved") {
		t.Fatalf("collector masked review=%+v err=%v", status, err)
	}
}

func TestProviderReversalAuditFailureRollsBackMoneyAndReceipt(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	f := createPaymentFixture(t, env, "audit-reversal", 20000, 10000, 50000)
	_ = handoffInput(t, env, f.account)
	i := paidReversalIntent(t, env, f)
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events ADD CONSTRAINT test_reversal_audit_failure CHECK(event_type<>'billing.provider_reversal.allocated') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := env.db.Exec(context.Background(), `ALTER TABLE billing_audit_events DROP CONSTRAINT IF EXISTS test_reversal_audit_failure`); err != nil {
			t.Error(err)
		}
	})
	in := reversalInput(f.account.ID, i.ID, "atomic-audit")
	if _, err := env.store.RecordProviderReversal(ctx, in); err == nil {
		t.Fatal("expected audit failure")
	}
	var receipts int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM billing_provider_reversal_events`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("rolled-back receipts=%d err=%v", receipts, err)
	}
	account, err := env.store.GetCommercialAccount(ctx, f.account.ID)
	if err != nil || account.AvailableBalanceMinor != 30000 {
		t.Fatalf("rolled-back money=%+v err=%v", account, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events DROP CONSTRAINT test_reversal_audit_failure`); err != nil {
		t.Fatal(err)
	}
	if r, err := env.store.RecordProviderReversal(ctx, in); err != nil || r.Duplicate || r.Account.AvailableBalanceMinor != 29900 {
		t.Fatalf("retry=%+v err=%v", r, err)
	}
}

func TestProviderReversalCompetingEventsCannotOverReverseOriginalPayment(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	f := createPaymentFixture(t, env, "competing-reversal", 20000, 10000, 50000)
	_ = handoffInput(t, env, f.account)
	i := paidReversalIntent(t, env, f)
	results := make([]ProviderReversalResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for n, event := range []string{"first-provider-event", "second-provider-event"} {
		wg.Add(1)
		go func(n int, event string) {
			defer wg.Done()
			in := reversalInput(f.account.ID, i.ID, event)
			in.AmountMinor = 6000
			results[n], errs[n] = env.store.RecordProviderReversal(ctx, in)
		}(n, event)
	}
	wg.Wait()
	allocated, review := 0, 0
	for n, r := range results {
		if errs[n] != nil {
			t.Fatal(errs[n])
		}
		if r.Disposition == "current_balance" {
			allocated++
		}
		if r.ReviewReason == "reversal_exceeds_original_credit" {
			review++
		}
	}
	a, err := env.store.GetCommercialAccount(ctx, f.account.ID)
	if err != nil || allocated != 1 || review != 1 || a.AvailableBalanceMinor != 24000 {
		t.Fatalf("allocated=%d review=%d account=%+v err=%v", allocated, review, a, err)
	}
}

func TestProviderReversalPreparationAllowsSettlementButInvalidatesConfirmations(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	f := createPaymentFixture(t, env, "preparing-reversal", 20000, 10000, 50000)
	p := handoffInput(t, env, f.account)
	i := paidReversalIntent(t, env, f)
	if _, err := env.store.PrepareOwnershipHandoff(ctx, p); err != nil {
		t.Fatal(err)
	}
	scope := HandoffScope{OrganizationID: p.OrganizationID, OperationID: p.OperationID, OwnershipVersion: p.OwnershipVersion}
	status, err := env.store.RecordHandoffSettlement(ctx, settlementReceipt(t, env, scope, "before-reversal"))
	if err != nil || status.Snapshot == nil {
		t.Fatalf("snapshot=%+v err=%v", status, err)
	}
	for _, user := range []string{p.SourceUserID, p.TargetUserID} {
		if _, err := env.store.ConfirmHandoffSnapshot(ctx, snapshotConfirmation(scope, user, status.Snapshot)); err != nil {
			t.Fatal(err)
		}
	}
	// Even bypassing Go's intent creator cannot introduce a new charge while held.
	_, err = env.db.Exec(ctx, `INSERT INTO payment_intents(account_id,amount_minor,currency,reason,provider,payment_method_id,state,idempotency_key,correlation_id)
		VALUES ($1,100,'TWD','manual_top_up','fake',$2,'created','bypass-handoff','bypass-handoff')`, f.account.ID, f.method.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "billing_handoff_commit_barrier" {
		t.Fatalf("SQL new intent fence: %v", err)
	}
	r, err := env.store.RecordProviderReversal(ctx, reversalInput(f.account.ID, i.ID, "before-commit-grant"))
	if err != nil || r.Disposition != "current_balance" || r.Account.AvailableBalanceMinor != 29900 {
		t.Fatalf("settlement reversal=%+v err=%v", r, err)
	}
	if _, err := env.store.AuthorizeHandoffCommit(ctx, AuthorizeHandoffCommitInput{Scope: scope, AuthorizationID: testutil.OrganizationID("stale-refund-grant"), SnapshotVersion: status.Snapshot.Version}); err == nil {
		t.Fatal("stale confirmed amount authorized")
	}
	fresh, err := env.store.RecordHandoffSettlement(ctx, settlementReceipt(t, env, scope, "after-reversal"))
	if err != nil || fresh.Snapshot == nil || fresh.Snapshot.BalanceMinor != 29900 || fresh.Snapshot.SourceConfirmed || fresh.Snapshot.TargetConfirmed {
		t.Fatalf("new amount requires new confirmations: %+v err=%v", fresh, err)
	}
}

func TestProviderReversalUnpaidOrWrongProviderCannotDebit(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	f := createPaymentFixture(t, env, "unpaid-reversal", 20000, 10000, 50000)
	_ = handoffInput(t, env, f.account)
	created, err := env.store.CreateManualTopUp(ctx, CreateManualTopUpInput{AccountID: f.account.ID, PaymentMethodID: f.method.ID, AmountMinor: 10000, Currency: payment.CurrencyTWD, IdempotencyKey: "unpaid", CorrelationID: "unpaid"})
	if err != nil {
		t.Fatal(err)
	}
	in := reversalInput(f.account.ID, created.Intent.ID, "not-paid")
	r, err := env.store.RecordProviderReversal(ctx, in)
	if err != nil || r.ReviewReason != "original_payment_not_settled" || r.Account.AvailableBalanceMinor != 20000 {
		t.Fatalf("unpaid=%+v err=%v", r, err)
	}
	in.Provider = "other"
	in.EventReference = "provider-mismatch"
	r, err = env.store.RecordProviderReversal(ctx, in)
	if err != nil || r.ReviewReason != "original_payment_mismatch" || r.Account.AvailableBalanceMinor != 20000 {
		t.Fatalf("mismatch=%+v err=%v", r, err)
	}
}
