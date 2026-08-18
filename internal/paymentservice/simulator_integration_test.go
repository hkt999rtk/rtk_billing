package paymentservice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	simulatorprovider "github.com/hkt999rtk/rtk_billing/internal/paymentprovider/simulator"
	"github.com/hkt999rtk/rtk_billing/internal/paymentsimulator"
)

const (
	simulatorIntegrationSharedSecret   = "simulator-integration-shared-0123456789"
	simulatorIntegrationCallbackSecret = "simulator-integration-callback-0123456"
)

func newSimulatorIntegrationProvider(t *testing.T, env integrationEnv, runID, scenario string) *simulatorprovider.Client {
	t.Helper()
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(callback.Close)
	server, err := paymentsimulator.New(env.db, paymentsimulator.Config{
		Environment: "test", PublicBaseURL: "https://payment-simulator.test",
		CallbackURL: callback.URL, SharedSecret: simulatorIntegrationSharedSecret,
		CallbackSecret: simulatorIntegrationCallbackSecret, Retention: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	provider, err := simulatorprovider.New(simulatorprovider.Config{
		BaseURL: httpServer.URL, SharedSecret: simulatorIntegrationSharedSecret,
		RunID: runID, Scenario: scenario, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestSimulatorChargeCreditsExactlyOnceAndRefundIsIdempotent(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)}
	fixture := createIntegrationFixtureForProvider(t, env, "simulator-success", simulatorprovider.ProviderName, clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "simulator-success")
	provider := newSimulatorIntegrationProvider(t, env, "payment-service-success", "success")
	service := newProviderIntegrationService(t, env, provider, clock, true)

	processed, err := service.RunOnce(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("charge processed=%d err=%v", processed, err)
	}
	processed, err = service.RunOnce(context.Background())
	if err != nil || processed != 0 {
		t.Fatalf("completed simulator charge retried: processed=%d err=%v", processed, err)
	}
	stored, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateSucceeded || stored.ProviderTransactionReference == "" {
		t.Fatalf("intent=%+v err=%v", stored, err)
	}
	account, err := env.store.GetCommercialAccount(context.Background(), fixture.account.ID)
	if err != nil || account.AvailableBalanceMinor != 59000 {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	refundRequest := payment.RefundRequest{
		IntentID: intent.ID, AmountMinor: 300, Currency: payment.CurrencyTWD,
		ProviderTransactionReference: stored.ProviderTransactionReference,
		IdempotencyKey:               "simulator-refund-1", CorrelationID: "simulator-refund-correlation-1",
	}
	firstRefund, err := provider.Refund(context.Background(), refundRequest)
	if err != nil || firstRefund.State != payment.PaymentIntentStateSucceeded || firstRefund.ProviderTransactionReference == "" {
		t.Fatalf("first refund=%+v err=%v", firstRefund, err)
	}
	secondRefund, err := provider.Refund(context.Background(), refundRequest)
	if err != nil || secondRefund.State != firstRefund.State || secondRefund.ProviderTransactionReference != firstRefund.ProviderTransactionReference || secondRefund.ProviderCode != firstRefund.ProviderCode {
		t.Fatalf("refund replay=%+v first=%+v err=%v", secondRefund, firstRefund, err)
	}
	var charges, refunds, credits int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_simulator_operations WHERE operation = 'charge' AND intent_id = $1`, intent.ID).Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_simulator_operations WHERE operation = 'refund' AND intent_id = $1`, intent.ID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger_entries WHERE idempotency_scope = 'payment_intent' AND idempotency_key = $1`, intent.ID).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if charges != 1 || refunds != 1 || credits != 1 {
		t.Fatalf("charges=%d refunds=%d credits=%d", charges, refunds, credits)
	}
	isolatedProvider := newSimulatorIntegrationProvider(t, env, "payment-service-isolated", "declined")
	isolatedResult, err := isolatedProvider.Charge(context.Background(), payment.ChargeRequest{
		IntentID: stored.ID, AmountMinor: stored.AmountMinor, Currency: stored.Currency,
		OpaqueMethodReference: "fake-vaulted-method-token", MerchantOrderReference: stored.MerchantOrderReference,
		IdempotencyKey: stored.IdempotencyKey, CorrelationID: stored.CorrelationID,
	})
	if err != nil || isolatedResult.State != payment.PaymentIntentStateFailed {
		t.Fatalf("isolated run result=%+v err=%v", isolatedResult, err)
	}
	var successfulRunRows, isolatedRunRows int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_simulator_operations WHERE run_id = 'payment-service-success' AND operation = 'charge' AND intent_id = $1`, intent.ID).Scan(&successfulRunRows); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_simulator_operations WHERE run_id = 'payment-service-isolated' AND operation = 'charge' AND intent_id = $1`, intent.ID).Scan(&isolatedRunRows); err != nil {
		t.Fatal(err)
	}
	if successfulRunRows != 1 || isolatedRunRows != 1 {
		t.Fatalf("successful_run_rows=%d isolated_run_rows=%d", successfulRunRows, isolatedRunRows)
	}
}

func TestSimulatorUnknownChargeQueriesToConclusiveSuccess(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC)}
	fixture := createIntegrationFixtureForProvider(t, env, "simulator-query", simulatorprovider.ProviderName, clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "simulator-query")
	provider := newSimulatorIntegrationProvider(t, env, "payment-service-query", "temporary_error")
	service := newProviderIntegrationService(t, env, provider, clock, true)

	processed, err := service.RunOnce(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("unknown charge processed=%d err=%v", processed, err)
	}
	stored, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateUnknown {
		t.Fatalf("unknown intent=%+v err=%v", stored, err)
	}
	if _, err := env.db.Exec(context.Background(), `UPDATE payment_simulator_operations SET state = 'succeeded', scenario = 'success' WHERE operation = 'charge' AND intent_id = $1`, intent.ID); err != nil {
		t.Fatal(err)
	}
	clock.Add(2 * time.Minute)
	processed, err = service.RunOnce(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("query processed=%d err=%v", processed, err)
	}
	stored, err = env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateSucceeded {
		t.Fatalf("reconciled intent=%+v err=%v", stored, err)
	}
	var attempts, charges, credits int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_attempts WHERE intent_id = $1`, intent.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_simulator_operations WHERE operation = 'charge' AND intent_id = $1`, intent.ID).Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger_entries WHERE idempotency_scope = 'payment_intent' AND idempotency_key = $1`, intent.ID).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || charges != 1 || credits != 1 {
		t.Fatalf("attempts=%d charges=%d credits=%d", attempts, charges, credits)
	}
}

func TestSimulatorDeclineFailsWithoutCredit(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)}
	fixture := createIntegrationFixtureForProvider(t, env, "simulator-decline", simulatorprovider.ProviderName, clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "simulator-decline")
	provider := newSimulatorIntegrationProvider(t, env, "payment-service-decline", "declined")
	service := newProviderIntegrationService(t, env, provider, clock, true)

	processed, err := service.RunOnce(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("decline processed=%d err=%v", processed, err)
	}
	stored, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateFailed {
		t.Fatalf("declined intent=%+v err=%v", stored, err)
	}
	policy, err := env.store.GetAutoTopUpPolicy(context.Background(), fixture.account.ID)
	if err != nil || policy.ConsecutiveFailureCount != 1 || !policy.Enabled {
		t.Fatalf("policy=%+v err=%v", policy, err)
	}
	var credits int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger_entries WHERE idempotency_scope = 'payment_intent' AND idempotency_key = $1`, intent.ID).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if credits != 0 {
		t.Fatalf("declined simulator charge credits=%d", credits)
	}
}
