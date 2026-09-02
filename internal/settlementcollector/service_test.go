package settlementcollector

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/usagecheckpoint"
)

const (
	testCloudID     = "11111111-1111-4111-8111-111111111111"
	testOwnerID     = "22222222-2222-4222-8222-222222222222"
	testOperationID = "33333333-3333-4333-8333-333333333333"
	testReceiptID   = "44444444-4444-4444-8444-444444444444"
	testDigest      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type fakeStore struct {
	preflightScopes []paymentstore.CloudPreflightScope
	preflightState  paymentstore.CloudPreflightState
	targets         []paymentstore.HandoffCollectorTarget
	handoffState    paymentstore.SettlementState
	reconciled      paymentstore.ReconciledSettlementEvidence
	reconcileInputs []paymentstore.ReconcileSettlementInput
	preflightWrites []paymentstore.RecordCloudPreflightInput
	handoffWrites   []paymentstore.RecordSettlementInput
}

func (f *fakeStore) ListCloudPreflightScopes(context.Context, string, int) ([]paymentstore.CloudPreflightScope, error) {
	return f.preflightScopes, nil
}
func (f *fakeStore) CaptureCloudPreflightState(context.Context, paymentstore.CloudPreflightScope) (paymentstore.CloudPreflightState, error) {
	return f.preflightState, nil
}
func (f *fakeStore) ReconcileSettlementEvidence(_ context.Context, in paymentstore.ReconcileSettlementInput) (paymentstore.ReconciledSettlementEvidence, error) {
	f.reconcileInputs = append(f.reconcileInputs, in)
	return f.reconciled, nil
}
func (f *fakeStore) RecordCloudPreflightEvidence(_ context.Context, in paymentstore.RecordCloudPreflightInput) error {
	f.preflightWrites = append(f.preflightWrites, in)
	return nil
}
func (f *fakeStore) ListHandoffCollectorTargets(context.Context, int) ([]paymentstore.HandoffCollectorTarget, error) {
	return f.targets, nil
}
func (f *fakeStore) CaptureHandoffSettlementState(context.Context, paymentstore.HandoffScope) (paymentstore.SettlementState, error) {
	return f.handoffState, nil
}
func (f *fakeStore) RecordHandoffSettlement(_ context.Context, in paymentstore.RecordSettlementInput) (paymentstore.HandoffSettlementStatus, error) {
	f.handoffWrites = append(f.handoffWrites, in)
	return paymentstore.HandoffSettlementStatus{}, nil
}

type fakeProducer struct {
	scopes []usagecheckpoint.Scope
	err    error
}

func (f *fakeProducer) Checkpoint(_ context.Context, scope usagecheckpoint.Scope) (usagecheckpoint.Evidence, error) {
	f.scopes = append(f.scopes, scope)
	return usagecheckpoint.Evidence{Scope: scope, Complete: true, CheckpointSHA256: testDigest,
		ObservedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}, f.err
}

func TestRunOnceRecordsBoundPreflightAndHandoffEvidence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	financial := billing.FinancialEvidence{BalanceKnown: true, Currency: billing.CurrencyTWD,
		UsageSettled: true, InvoicesReconciled: true, ProviderWorkReconciled: true}
	store := &fakeStore{
		preflightScopes: []paymentstore.CloudPreflightScope{{OrganizationID: testCloudID, OwnerUserID: testOwnerID, OwnershipVersion: 7}},
		preflightState: paymentstore.CloudPreflightState{Scope: paymentstore.CloudPreflightScope{OrganizationID: testCloudID, OwnerUserID: testOwnerID, OwnershipVersion: 7},
			SettlementState: paymentstore.SettlementState{SHA256: testDigest, Financial: financial}, ObservedAt: now},
		targets: []paymentstore.HandoffCollectorTarget{{Scope: paymentstore.HandoffScope{OrganizationID: testCloudID, OperationID: testOperationID, OwnershipVersion: 7},
			SourceUserID: testOwnerID, Cutoff: now.Add(-time.Minute)}},
		handoffState: paymentstore.SettlementState{SHA256: testDigest, Financial: financial},
		reconciled: paymentstore.ReconciledSettlementEvidence{Financial: financial, UsageCheckpointSHA256: testDigest,
			InvoiceCheckpointSHA256: testDigest, ProviderCheckpointSHA256: testDigest},
	}
	producer := &fakeProducer{}
	service, err := New(store, producer, 25)
	if err != nil {
		t.Fatal(err)
	}
	service.newID = func() (string, error) { return testReceiptID, nil }
	report, err := service.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report != (Report{PreflightRecorded: 1, HandoffRecorded: 1}) {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(producer.scopes) != 2 || producer.scopes[0].CoveredThrough != now || producer.scopes[1].CoveredThrough != store.targets[0].Cutoff {
		t.Fatalf("producer scopes are not bound to their settlement horizons: %+v", producer.scopes)
	}
	if len(store.reconcileInputs) != 2 || store.reconcileInputs[0].ProducerCheckpointSHA256 != testDigest || store.reconcileInputs[1].ProducerCheckpointSHA256 != testDigest {
		t.Fatalf("producer checkpoint was not passed to local reconciliation: %+v", store.reconcileInputs)
	}
	if len(store.preflightWrites) != 1 || store.preflightWrites[0].ReceiptID != testReceiptID || len(store.handoffWrites) != 1 || store.handoffWrites[0].ReceiptID != testReceiptID {
		t.Fatalf("collector receipts were not recorded: preflight=%+v handoff=%+v", store.preflightWrites, store.handoffWrites)
	}
}

func TestRunOnceFailsClosedWhenProducerIsUnavailable(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	store := &fakeStore{
		preflightScopes: []paymentstore.CloudPreflightScope{{OrganizationID: testCloudID, OwnerUserID: testOwnerID, OwnershipVersion: 1}},
		preflightState: paymentstore.CloudPreflightState{Scope: paymentstore.CloudPreflightScope{OrganizationID: testCloudID, OwnerUserID: testOwnerID, OwnershipVersion: 1},
			SettlementState: paymentstore.SettlementState{SHA256: testDigest}, ObservedAt: now},
	}
	service, err := New(store, &fakeProducer{err: usagecheckpoint.ErrUnavailable}, 1)
	if err != nil {
		t.Fatal(err)
	}
	report, err := service.RunOnce(context.Background())
	if !errors.Is(err, usagecheckpoint.ErrUnavailable) || report.Failed != 1 {
		t.Fatalf("expected fail-closed producer error, report=%+v err=%v", report, err)
	}
	if len(store.reconcileInputs) != 0 || len(store.preflightWrites) != 0 {
		t.Fatal("unavailable producer must not create local completeness evidence")
	}
}

func TestRandomUUIDIsCanonicalV4(t *testing.T) {
	id, err := randomUUID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("not a canonical UUIDv4: %q", id)
	}
}
