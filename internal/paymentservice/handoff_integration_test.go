package paymentservice

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/fake"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestHandoffAllowsCrashRecoveryAndProviderQueryWithoutNewCharge(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()
	clock := &testClock{now: time.Date(2026, 8, 15, 11, 0, 0, 0, time.UTC)}
	fixture := createIntegrationFixture(t, env, "handoff-crash", clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "handoff-crash")
	jobs, err := env.store.ClaimPaymentJobs(ctx, clock.now, clock.now.Add(-time.Minute), "crashed-worker", 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim=%+v err=%v", jobs, err)
	}
	if work, err := env.store.BeginProviderAttempt(ctx, paymentstore.BeginProviderAttemptInput{
		JobID: jobs[0].ID, LeaseOwner: "crashed-worker", Operation: payment.ProviderOperationCharge, Now: clock.now,
	}); err != nil || work.RecoverIncompleteAttempt {
		t.Fatalf("begin=%+v err=%v", work, err)
	}
	ownerID := testutil.OrganizationID("handoff-crash-owner")
	if err := env.store.InitializeResponsibility(ctx, paymentstore.InitialResponsibilityInput{
		AccountID: fixture.account.ID, OwnerUserID: ownerID, OwnershipVersion: 1,
		EffectiveFrom: clock.now.Add(-time.Hour), SourceEvidenceSHA256: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PrepareOwnershipHandoff(ctx, paymentstore.PrepareOwnershipHandoffInput{
		OperationID: testutil.OrganizationID("handoff-crash-operation"), OrganizationID: fixture.account.OrganizationID,
		SourceUserID: ownerID, TargetUserID: testutil.OrganizationID("handoff-crash-target"), OwnershipVersion: 1, Cutoff: clock.now,
	}); err != nil {
		t.Fatal(err)
	}
	provider := fake.New("webhook-secret")
	provider.QueueQuery(fake.Outcome{Result: payment.ProviderResult{
		State: payment.PaymentIntentStateSucceeded, ProviderTransactionReference: "handoff-original-txn", ProviderCode: "00",
	}})
	service := newIntegrationService(t, env, provider, clock, true)
	clock.Add(time.Minute)
	if processed, err := service.RunOnce(ctx); err != nil || processed != 1 {
		t.Fatalf("recover: processed=%d err=%v", processed, err)
	}
	clock.Add(2 * time.Minute)
	if processed, err := service.RunOnce(ctx); err != nil || processed != 1 {
		t.Fatalf("query: processed=%d err=%v", processed, err)
	}
	if len(provider.ChargeCalls()) != 0 || len(provider.QueryCalls()) != 1 {
		t.Fatalf("new charges=%d queries=%d", len(provider.ChargeCalls()), len(provider.QueryCalls()))
	}
	stored, err := env.store.GetPaymentIntent(ctx, intent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateSucceeded {
		t.Fatalf("reconciled=%+v err=%v", stored, err)
	}
}
