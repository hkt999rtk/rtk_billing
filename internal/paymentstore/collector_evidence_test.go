package paymentstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func reconcilePreflight(t *testing.T, env paymentIntegrationEnv, scope CloudPreflightScope) (CloudPreflightState, ReconciledSettlementEvidence) {
	t.Helper()
	state, err := env.store.CaptureCloudPreflightState(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := env.store.ReconcileSettlementEvidence(context.Background(), ReconcileSettlementInput{
		OrganizationID: scope.OrganizationID, State: state.SettlementState, CoveredThrough: state.ObservedAt,
		ProducerCheckpointSHA256: strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return state, evidence
}

func TestCollectorReconcilesEmptyCloudOnlyWithProducerCheckpoint(t *testing.T) {
	env, _, scope := cloudPreflightFixture(t, 0)
	ctx := context.Background()
	state, evidence := reconcilePreflight(t, env, scope)
	if !evidence.Financial.UsageSettled || !evidence.Financial.InvoicesReconciled || !evidence.Financial.ProviderWorkReconciled ||
		!validLowerSHA256(evidence.UsageCheckpointSHA256) || !validLowerSHA256(evidence.InvoiceCheckpointSHA256) || !validLowerSHA256(evidence.ProviderCheckpointSHA256) {
		t.Fatalf("empty cloud evidence = %+v", evidence)
	}
	if err := env.store.RecordCloudPreflightEvidence(ctx, RecordCloudPreflightInput{
		State:     CloudPreflightState{Scope: scope, SettlementState: SettlementState{SHA256: state.SHA256, Financial: evidence.Financial}, ObservedAt: state.ObservedAt},
		ReceiptID: "11111111-1111-4111-8111-111111111111", UsageCheckpointSHA256: evidence.UsageCheckpointSHA256,
		InvoiceCheckpointSHA256: evidence.InvoiceCheckpointSHA256, ProviderCheckpointSHA256: evidence.ProviderCheckpointSHA256, ExpiresAt: state.ObservedAt.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	preflight, err := env.store.GetCloudDeletionPreflight(ctx, scope)
	if err != nil || !preflight.Eligible {
		t.Fatalf("eligible preflight = %+v, %v", preflight, err)
	}
	bad := ReconcileSettlementInput{OrganizationID: scope.OrganizationID, State: state.SettlementState, CoveredThrough: state.ObservedAt}
	if _, err := env.store.ReconcileSettlementEvidence(ctx, bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing producer checkpoint accepted: %v", err)
	}
}

func TestCollectorLeavesUnratedUsageAndOpenPeriodsBlocked(t *testing.T) {
	env, _, scope := cloudPreflightFixture(t, 0)
	ctx := context.Background()
	cutoff := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := env.db.Exec(ctx, `INSERT INTO billing_usage_facts
		(usage_id,organization_id,service_code,metric_code,quantity,unit,window_start,window_end,source,source_sha256)
		VALUES ('collector-unrated',$1,'mqtt','message',1,'message',$2,$3,'test',$4)`, scope.OrganizationID, cutoff.Add(-time.Minute), cutoff, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	state, evidence := reconcilePreflight(t, env, scope)
	if evidence.Financial.UsageSettled || evidence.UsageCheckpointSHA256 != "" || !evidence.Financial.InvoicesReconciled {
		t.Fatalf("unrated usage evidence = %+v", evidence)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO billing_periods(organization_id,currency,period_start,period_end,state)
		VALUES($1,'TWD',$2,$3,'open')`, scope.OrganizationID, state.ObservedAt.Add(-time.Hour), state.ObservedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, evidence = reconcilePreflight(t, env, scope)
	if evidence.Financial.InvoicesReconciled || evidence.InvoiceCheckpointSHA256 != "" || evidence.Financial.UsageSettled {
		t.Fatalf("open period evidence = %+v", evidence)
	}
}

func TestCollectorRejectsStateChangedAfterCaptureAndListsTargets(t *testing.T) {
	env, account, scope := cloudPreflightFixture(t, 0)
	ctx := context.Background()
	state, err := env.store.CaptureCloudPreflightState(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PostLedgerEntry(ctx, PostLedgerEntryInput{AccountID: account.ID, Direction: payment.LedgerDirectionCredit,
		Reason: payment.LedgerReasonManualAdjustmentCredit, AmountMinor: 1, Currency: payment.CurrencyTWD,
		IdempotencyScope: "collector", IdempotencyKey: "changed", ActorType: "service", ActorID: "test", RequestID: "changed"}); err != nil {
		t.Fatal(err)
	}
	_, err = env.store.ReconcileSettlementEvidence(ctx, ReconcileSettlementInput{OrganizationID: scope.OrganizationID, State: state.SettlementState,
		CoveredThrough: state.ObservedAt, ProducerCheckpointSHA256: strings.Repeat("d", 64)})
	if !errors.Is(err, ErrSettlementEvidenceStale) {
		t.Fatalf("stale state error = %v", err)
	}
	scopes, err := env.store.ListCloudPreflightScopes(ctx, "", 10)
	if err != nil || len(scopes) != 1 || scopes[0] != scope {
		t.Fatalf("preflight scopes = %+v, %v", scopes, err)
	}
	if scopes, err = env.store.ListCloudPreflightScopes(ctx, scope.OrganizationID, 10); err != nil || len(scopes) != 0 {
		t.Fatalf("preflight cursor = %+v, %v", scopes, err)
	}

	if _, err := env.store.ListHandoffCollectorTargets(ctx, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid handoff batch accepted: %v", err)
	}
}

func TestCollectorListsActiveHandoffCutoff(t *testing.T) {
	env, _, prepare, scope := newSettlementFixture(t, 0)
	targets, err := env.store.ListHandoffCollectorTargets(context.Background(), 10)
	if err != nil || len(targets) != 1 {
		t.Fatalf("handoff targets = %+v, %v", targets, err)
	}
	target := targets[0]
	if target.Scope != scope || target.SourceUserID != prepare.SourceUserID || !target.Cutoff.Equal(prepare.Cutoff) {
		t.Fatalf("handoff target = %+v", target)
	}
}
