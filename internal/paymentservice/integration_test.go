package paymentservice

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/fake"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

type integrationEnv struct {
	store *paymentstore.Store
	db    *pgxpool.Pool
}

type integrationFixture struct {
	account payment.CommercialAccount
	method  payment.PaymentMethod
	policy  payment.AutoTopUpPolicy
}

type testClock struct{ now time.Time }

func (c *testClock) Current() time.Time         { return c.now }
func (c *testClock) Add(duration time.Duration) { c.now = c.now.Add(duration) }

type testReferenceResolver struct {
	value string
	err   error
}

func (r testReferenceResolver) ResolveMethodReference(context.Context, []byte) (string, error) {
	return r.value, r.err
}

func newIntegrationEnv(t *testing.T) integrationEnv {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := database.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), `
		TRUNCATE
			payment_reconciliation_jobs,
			payment_webhook_inbox,
			payment_attempts,
			payment_intents,
			auto_topup_policies,
			payment_methods,
			payment_consents,
			balance_ledger_entries,
			commercial_accounts
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatal(err)
	}
	return integrationEnv{store: paymentstore.New(db), db: db}
}

func createIntegrationFixture(t *testing.T, env integrationEnv, suffix string, now time.Time) integrationFixture {
	return createIntegrationFixtureForProvider(t, env, suffix, "fake", now)
}

func createIntegrationFixtureForProvider(t *testing.T, env integrationEnv, suffix, providerName string, now time.Time) integrationFixture {
	t.Helper()
	ctx := context.Background()
	organizationID := testutil.OrganizationID("payment-service-" + suffix)
	actorID := "test-user-" + suffix
	account, _, err := env.store.EnsureCommercialAccount(ctx, organizationID, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := env.store.PostLedgerEntry(ctx, paymentstore.PostLedgerEntryInput{
		AccountID: account.ID, Direction: payment.LedgerDirectionCredit,
		AmountMinor: 20000, Currency: payment.CurrencyTWD, Reason: payment.LedgerReasonManualAdjustmentCredit,
		IdempotencyScope: "fixture", IdempotencyKey: "initial-" + suffix, Now: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	account = initial.Account
	methodConsent, err := env.store.CreateConsent(ctx, paymentstore.CreateConsentInput{
		AccountID: account.ID, ConsentType: "payment_method", TextVersion: "payment-method-v1",
		TextSHA256: strings.Repeat("a", 64), AcceptedActorType: "user", AcceptedActorID: actorID,
		AcceptedAt: now.Add(-50 * time.Minute), Locale: "zh-TW", Source: "integration-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	method, err := env.store.CreatePaymentMethod(ctx, paymentstore.CreatePaymentMethodInput{
		AccountID: account.ID, Provider: providerName,
		ProviderCustomerRefCiphertext: []byte("encrypted-fake-customer"),
		ProviderMethodRefCiphertext:   []byte("encrypted-fake-method"),
		ProviderMethodRefSHA256:       strings.Repeat("b", 64), CardBrand: "test", LastFour: "4242",
		Capabilities: payment.ProviderCapabilities{
			VaultedMethod: true, MerchantInitiatedCharge: true, StatusQuery: true, Webhook: true,
		},
		Status: payment.PaymentMethodStatusActive, ConsentID: methodConsent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	policyConsent, err := env.store.CreateConsent(ctx, paymentstore.CreateConsentInput{
		AccountID: account.ID, ConsentType: "auto_topup", TextVersion: "auto-topup-v1",
		TextSHA256: strings.Repeat("c", 64), AcceptedActorType: "user", AcceptedActorID: actorID,
		AcceptedAt: now.Add(-40 * time.Minute), Locale: "zh-TW", Source: "integration-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := env.store.PutAutoTopUpPolicy(ctx, paymentstore.PutAutoTopUpPolicyInput{
		AccountID: account.ID, Enabled: true, ThresholdMinor: 10000, TopUpAmountMinor: 50000,
		Currency: payment.CurrencyTWD, PaymentMethodID: method.ID, DailyAttemptLimit: 3,
		DailyAmountLimitMinor: 150000, CooldownSeconds: 3600, ConsentID: policyConsent.ID,
		ActorID: actorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return integrationFixture{account: account, method: method, policy: policy}
}

func triggerAutomaticIntent(t *testing.T, env integrationEnv, fixture integrationFixture, now time.Time, suffix string) payment.PaymentIntent {
	t.Helper()
	result, err := env.store.PostLedgerEntry(context.Background(), paymentstore.PostLedgerEntryInput{
		AccountID: fixture.account.ID, Direction: payment.LedgerDirectionDebit,
		AmountMinor: 11000, Currency: payment.CurrencyTWD, Reason: payment.LedgerReasonInvoiceDebit,
		IdempotencyScope: "invoice", IdempotencyKey: "invoice-" + suffix,
		ExternalType: "invoice", ExternalID: "invoice-" + suffix,
		ActorType: "service", ActorID: "billing", RequestID: "request-" + suffix, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Intent == nil {
		t.Fatal("expected automatic top-up intent")
	}
	return *result.Intent
}

func newIntegrationService(t *testing.T, env integrationEnv, provider *fake.Provider, clock *testClock, enabled bool) *Service {
	return newProviderIntegrationService(t, env, provider, clock, enabled)
}

func newProviderIntegrationService(t *testing.T, env integrationEnv, provider payment.PaymentProvider, clock *testClock, enabled bool) *Service {
	t.Helper()
	service, err := New(Options{
		Store: env.store, Providers: []payment.PaymentProvider{provider},
		ReferenceResolver: testReferenceResolver{value: "fake-vaulted-method-token"},
		LeaseOwner:        "integration-worker", LeaseDuration: 30 * time.Second,
		ReconciliationDelay: time.Minute, BatchSize: 20,
		ChargeEnabled: map[string]bool{provider.Name(): enabled}, Now: clock.Current,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestRunRejectsInvalidIntervalAndStopsOnCancellation(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)}
	service := newIntegrationService(t, env, fake.New("webhook-secret"), clock, false)
	if err := service.Run(context.Background(), 0); err == nil {
		t.Fatal("zero poll interval should fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if err := service.Run(ctx, time.Hour); err != nil {
		t.Fatalf("canceled worker returned error: %v", err)
	}
}

func TestSuccessfulChargeCreditsExactlyOnceWithoutQuery(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC)}
	fixture := createIntegrationFixture(t, env, "success", clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "success")
	provider := fake.New("webhook-secret")
	provider.QueueCharge(fake.Outcome{Result: payment.ProviderResult{
		State: payment.PaymentIntentStateSucceeded, ProviderTransactionReference: "fake-txn-success", ProviderCode: "00",
	}})
	service := newIntegrationService(t, env, provider, clock, true)

	processed, err := service.RunOnce(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("charge run processed=%d err=%v", processed, err)
	}
	processed, err = service.RunOnce(context.Background())
	if err != nil || processed != 0 {
		t.Fatalf("completed charge must not be retried: processed=%d err=%v", processed, err)
	}
	stored, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateSucceeded || stored.ProviderTransactionReference != "fake-txn-success" {
		t.Fatalf("intent=%+v err=%v", stored, err)
	}
	account, err := env.store.GetCommercialAccount(context.Background(), fixture.account.ID)
	if err != nil || account.AvailableBalanceMinor != 59000 || account.State != payment.AccountStateActive {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	var attempts, missingDigests, credits, completedJobs int
	if err := env.db.QueryRow(context.Background(), `
		SELECT count(*), count(*) FILTER (WHERE request_sha256 IS NULL OR response_sha256 IS NULL)
		FROM payment_attempts WHERE intent_id = $1
	`, intent.ID).Scan(&attempts, &missingDigests); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger_entries WHERE idempotency_scope = 'payment_intent' AND idempotency_key = $1`, intent.ID).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_reconciliation_jobs WHERE intent_id = $1 AND status = 'completed'`, intent.ID).Scan(&completedJobs); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || missingDigests != 0 || credits != 1 || completedJobs != 1 || len(provider.ChargeCalls()) != 1 || len(provider.QueryCalls()) != 0 {
		t.Fatalf("attempts=%d missing_digests=%d credits=%d completed_jobs=%d charge_calls=%d query_calls=%d", attempts, missingDigests, credits, completedJobs, len(provider.ChargeCalls()), len(provider.QueryCalls()))
	}
}

func TestDeclinedChargeFailsWithoutCreditOrRetry(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 15, 9, 45, 0, 0, time.UTC)}
	fixture := createIntegrationFixture(t, env, "declined", clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "declined")
	provider := fake.New("webhook-secret")
	provider.QueueCharge(fake.Outcome{Err: payment.NewProviderError(payment.ProviderErrorDeclined, "card_declined", false, nil)})
	service := newIntegrationService(t, env, provider, clock, true)

	processed, err := service.RunOnce(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("decline run processed=%d err=%v", processed, err)
	}
	processed, err = service.RunOnce(context.Background())
	if err != nil || processed != 0 {
		t.Fatalf("declined charge must not be retried: processed=%d err=%v", processed, err)
	}
	stored, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateFailed || stored.ProviderTransactionReference != "" {
		t.Fatalf("intent=%+v err=%v", stored, err)
	}
	account, err := env.store.GetCommercialAccount(context.Background(), fixture.account.ID)
	if err != nil || account.AvailableBalanceMinor != 9000 || account.State != payment.AccountStateActive {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	policy, err := env.store.GetAutoTopUpPolicy(context.Background(), fixture.account.ID)
	if err != nil || !policy.Enabled || policy.ConsecutiveFailureCount != 1 {
		t.Fatalf("policy=%+v err=%v", policy, err)
	}
	var attempts, credits, completedJobs int
	var normalizedResult, providerCode string
	if err := env.db.QueryRow(context.Background(), `
		SELECT count(*)::int, min(normalized_result), min(provider_code)
		FROM payment_attempts WHERE intent_id = $1
	`, intent.ID).Scan(&attempts, &normalizedResult, &providerCode); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger_entries WHERE idempotency_scope = 'payment_intent' AND idempotency_key = $1`, intent.ID).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_reconciliation_jobs WHERE intent_id = $1 AND status = 'completed'`, intent.ID).Scan(&completedJobs); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || normalizedResult != "failed" || providerCode != "card_declined" || credits != 0 || completedJobs != 1 || len(provider.ChargeCalls()) != 1 || len(provider.QueryCalls()) != 0 {
		t.Fatalf("attempts=%d result=%s code=%s credits=%d completed_jobs=%d charge_calls=%d query_calls=%d", attempts, normalizedResult, providerCode, credits, completedJobs, len(provider.ChargeCalls()), len(provider.QueryCalls()))
	}
}

func TestTimeoutReconcilesToOneCredit(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)}
	fixture := createIntegrationFixture(t, env, "timeout", clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "timeout")
	provider := fake.New("webhook-secret")
	provider.QueueCharge(fake.Outcome{Err: payment.NewProviderError(payment.ProviderErrorUnknown, "timeout", true, context.DeadlineExceeded)})
	provider.QueueQuery(fake.Outcome{Result: payment.ProviderResult{
		State: payment.PaymentIntentStateSucceeded, ProviderTransactionReference: "fake-txn-timeout", ProviderCode: "00",
	}})
	service := newIntegrationService(t, env, provider, clock, true)

	processed, err := service.RunOnce(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("charge run processed=%d err=%v", processed, err)
	}
	unknown, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || unknown.State != payment.PaymentIntentStateUnknown {
		t.Fatalf("unknown intent=%+v err=%v", unknown, err)
	}
	if len(provider.ChargeCalls()) != 1 || len(provider.QueryCalls()) != 0 {
		t.Fatalf("unexpected provider calls charge=%d query=%d", len(provider.ChargeCalls()), len(provider.QueryCalls()))
	}

	clock.Add(2 * time.Minute)
	processed, err = service.RunOnce(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("query run processed=%d err=%v", processed, err)
	}
	succeeded, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || succeeded.State != payment.PaymentIntentStateSucceeded {
		t.Fatalf("succeeded intent=%+v err=%v", succeeded, err)
	}
	account, err := env.store.GetCommercialAccount(context.Background(), fixture.account.ID)
	if err != nil || account.AvailableBalanceMinor != 59000 {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	if len(provider.ChargeCalls()) != 1 || len(provider.QueryCalls()) != 1 {
		t.Fatalf("provider calls charge=%d query=%d", len(provider.ChargeCalls()), len(provider.QueryCalls()))
	}
	var attempts, credits, missingDigests int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE request_sha256 IS NULL OR response_sha256 IS NULL) FROM payment_attempts WHERE intent_id = $1`, intent.ID).Scan(&attempts, &missingDigests); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger_entries WHERE idempotency_scope = 'payment_intent' AND idempotency_key = $1`, intent.ID).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || missingDigests != 0 || credits != 1 {
		t.Fatalf("attempts=%d missing_digests=%d credits=%d", attempts, missingDigests, credits)
	}
}

func TestStaleIncompleteAttemptDoesNotRepeatCharge(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 15, 11, 0, 0, 0, time.UTC)}
	fixture := createIntegrationFixture(t, env, "crash", clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "crash")
	jobs, err := env.store.ClaimPaymentJobs(context.Background(), clock.now, clock.now.Add(-time.Minute), "crashed-worker", 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim jobs=%+v err=%v", jobs, err)
	}
	work, err := env.store.BeginProviderAttempt(context.Background(), paymentstore.BeginProviderAttemptInput{
		JobID: jobs[0].ID, LeaseOwner: "crashed-worker", Operation: payment.ProviderOperationCharge, Now: clock.now,
	})
	if err != nil || work.RecoverIncompleteAttempt {
		t.Fatalf("begin work=%+v err=%v", work, err)
	}

	provider := fake.New("webhook-secret")
	provider.QueueQuery(fake.Outcome{Result: payment.ProviderResult{
		State: payment.PaymentIntentStateSucceeded, ProviderTransactionReference: "fake-txn-crash", ProviderCode: "00",
	}})
	service := newIntegrationService(t, env, provider, clock, true)
	clock.Add(time.Minute)
	processed, err := service.RunOnce(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("recovery processed=%d err=%v", processed, err)
	}
	if len(provider.ChargeCalls()) != 0 {
		t.Fatalf("recovered incomplete attempt must not charge again: %d calls", len(provider.ChargeCalls()))
	}
	unknown, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || unknown.State != payment.PaymentIntentStateUnknown {
		t.Fatalf("recovered intent=%+v err=%v", unknown, err)
	}
	clock.Add(2 * time.Minute)
	if processed, err = service.RunOnce(context.Background()); err != nil || processed != 1 {
		t.Fatalf("reconciliation processed=%d err=%v", processed, err)
	}
	if len(provider.QueryCalls()) != 1 {
		t.Fatalf("query calls=%d", len(provider.QueryCalls()))
	}
}

func TestVerifiedDuplicateWebhookSchedulesOneReconciliation(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)}
	fixture := createIntegrationFixture(t, env, "webhook", clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "webhook")
	provider := fake.New("webhook-secret")
	provider.QueueCharge(fake.Outcome{Err: payment.NewProviderError(payment.ProviderErrorUnknown, "timeout", true, context.DeadlineExceeded)})
	provider.QueueQuery(fake.Outcome{Result: payment.ProviderResult{
		State: payment.PaymentIntentStateSucceeded, ProviderTransactionReference: "fake-txn-webhook", ProviderCode: "00",
	}})
	service := newIntegrationService(t, env, provider, clock, true)
	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	event := payment.WebhookEvent{
		ProviderEventReference: "event-webhook-1", MerchantOrderReference: stored.MerchantOrderReference,
		ProviderTransactionReference: "fake-txn-webhook", AmountMinor: stored.AmountMinor,
		Currency: stored.Currency, State: payment.PaymentIntentStateSucceeded,
		EventType: "payment.succeeded", ProviderCode: "00",
	}
	body, err := fake.WebhookBody(event)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{"X-Fake-Signature": []string{fake.SignWebhook("webhook-secret", body)}}
	first, err := service.HandleWebhook(context.Background(), "fake", header, body)
	if err != nil || first.Duplicate || !first.Verified {
		t.Fatalf("first webhook=%+v err=%v", first, err)
	}
	second, err := service.HandleWebhook(context.Background(), "fake", header, body)
	if err != nil || !second.Duplicate || !second.Verified || second.Receipt.ID != first.Receipt.ID {
		t.Fatalf("duplicate webhook=%+v err=%v", second, err)
	}
	var receipts, webhookJobs int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_webhook_inbox WHERE provider = 'fake'`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_reconciliation_jobs WHERE intent_id = $1 AND reason = 'webhook'`, intent.ID).Scan(&webhookJobs); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || webhookJobs != 1 {
		t.Fatalf("receipts=%d webhook_jobs=%d", receipts, webhookJobs)
	}
	if processed, err := service.RunOnce(context.Background()); err != nil || processed != 1 {
		t.Fatalf("webhook query processed=%d err=%v", processed, err)
	}
	var processedReceipts int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_webhook_inbox WHERE processing_state = 'processed'`).Scan(&processedReceipts); err != nil {
		t.Fatal(err)
	}
	if processedReceipts != 1 {
		t.Fatalf("processed receipts=%d", processedReceipts)
	}
	clock.Add(2 * time.Minute)
	if processed, err := service.RunOnce(context.Background()); err != nil || processed != 1 {
		t.Fatalf("terminal stale job processed=%d err=%v", processed, err)
	}
	if len(provider.QueryCalls()) != 1 {
		t.Fatalf("terminal intent must not be queried again: %d calls", len(provider.QueryCalls()))
	}
}

func TestInvalidWebhookIsQuarantinedAndProviderDisabledMakesNoCharge(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 15, 13, 0, 0, 0, time.UTC)}
	fixture := createIntegrationFixture(t, env, "disabled", clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "disabled")
	provider := fake.New("webhook-secret")
	service := newIntegrationService(t, env, provider, clock, false)
	if processed, err := service.RunOnce(context.Background()); err != nil || processed != 1 {
		t.Fatalf("disabled processed=%d err=%v", processed, err)
	}
	if len(provider.ChargeCalls()) != 0 {
		t.Fatal("disabled provider must not receive a charge")
	}
	stored, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateFailed {
		t.Fatalf("disabled intent=%+v err=%v", stored, err)
	}

	badBody := []byte(`{"not":"trusted"}`)
	result, err := service.HandleWebhook(context.Background(), "fake", http.Header{"X-Fake-Signature": []string{"00"}}, badBody)
	var providerErr *payment.ProviderError
	if !errors.As(err, &providerErr) || result.Verified || result.Receipt.ProcessingState != "quarantined" {
		t.Fatalf("invalid webhook result=%+v err=%v", result, err)
	}
	var rejected int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_webhook_inbox WHERE verification_result = 'rejected' AND processing_state = 'quarantined'`).Scan(&rejected); err != nil {
		t.Fatal(err)
	}
	if rejected != 1 {
		t.Fatalf("rejected webhook count=%d", rejected)
	}
}

func TestRequiresActionStopsAutomaticFlowAndMarksAttention(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC)}
	fixture := createIntegrationFixture(t, env, "requires-action", clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "requires-action")
	provider := fake.New("webhook-secret")
	provider.QueueCharge(fake.Outcome{Result: payment.ProviderResult{
		State: payment.PaymentIntentStateRequiresAction, ProviderTransactionReference: "fake-action-1", ProviderCode: "otp_required",
	}})
	service := newIntegrationService(t, env, provider, clock, true)
	if processed, err := service.RunOnce(context.Background()); err != nil || processed != 1 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	stored, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateRequiresAction {
		t.Fatalf("intent=%+v err=%v", stored, err)
	}
	account, err := env.store.GetCommercialAccount(context.Background(), fixture.account.ID)
	if err != nil || account.State != payment.AccountStateAttentionRequired || account.AvailableBalanceMinor != 9000 {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	var credits int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger_entries WHERE idempotency_scope = 'payment_intent' AND idempotency_key = $1`, intent.ID).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if credits != 0 {
		t.Fatalf("requires-action credits=%d", credits)
	}
}

func TestRepeatedUnknownQueryReusesDurableJobUntilConclusive(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 15, 15, 0, 0, 0, time.UTC)}
	fixture := createIntegrationFixture(t, env, "repeat-unknown", clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "repeat-unknown")
	provider := fake.New("webhook-secret")
	provider.QueueCharge(fake.Outcome{Err: payment.NewProviderError(payment.ProviderErrorUnknown, "timeout", true, context.DeadlineExceeded)})
	provider.QueueQuery(
		fake.Outcome{Result: payment.ProviderResult{State: payment.PaymentIntentStateUnknown, ProviderCode: "pending"}},
		fake.Outcome{Result: payment.ProviderResult{State: payment.PaymentIntentStateSucceeded, ProviderTransactionReference: "fake-repeat-1", ProviderCode: "00"}},
	)
	service := newIntegrationService(t, env, provider, clock, true)
	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Add(2 * time.Minute)
	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Add(2 * time.Minute)
	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateSucceeded {
		t.Fatalf("intent=%+v err=%v", stored, err)
	}
	var unknownJobs, unknownJobAttempts int
	if err := env.db.QueryRow(context.Background(), `
		SELECT count(*), COALESCE(max(attempt_count), 0)
		FROM payment_reconciliation_jobs
		WHERE intent_id = $1 AND reason = 'unknown'
	`, intent.ID).Scan(&unknownJobs, &unknownJobAttempts); err != nil {
		t.Fatal(err)
	}
	if unknownJobs != 1 || unknownJobAttempts != 2 || len(provider.QueryCalls()) != 2 {
		t.Fatalf("unknown_jobs=%d attempts=%d query_calls=%d", unknownJobs, unknownJobAttempts, len(provider.QueryCalls()))
	}
}

func TestVerifiedWebhookMustMatchIntentAmountAndCurrency(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 15, 16, 0, 0, 0, time.UTC)}
	fixture := createIntegrationFixture(t, env, "webhook-mismatch", clock.now)
	intent := triggerAutomaticIntent(t, env, fixture, clock.now, "webhook-mismatch")
	provider := fake.New("webhook-secret")
	provider.QueueCharge(fake.Outcome{Err: payment.NewProviderError(payment.ProviderErrorUnknown, "timeout", true, context.DeadlineExceeded)})
	service := newIntegrationService(t, env, provider, clock, true)
	if _, err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := env.store.GetPaymentIntent(context.Background(), intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := fake.WebhookBody(payment.WebhookEvent{
		ProviderEventReference: "event-mismatch-1", MerchantOrderReference: stored.MerchantOrderReference,
		ProviderTransactionReference: "fake-mismatch-1", AmountMinor: stored.AmountMinor + 100,
		Currency: stored.Currency, State: payment.PaymentIntentStateSucceeded, EventType: "payment.succeeded",
	})
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{"X-Fake-Signature": []string{fake.SignWebhook("webhook-secret", body)}}
	if _, err := service.HandleWebhook(context.Background(), "fake", header, body); !errors.Is(err, paymentstore.ErrConflict) {
		t.Fatalf("mismatched webhook err=%v", err)
	}
	var receipts int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM payment_webhook_inbox`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 {
		t.Fatalf("mismatched webhook receipts=%d", receipts)
	}
}

func TestLocalProviderFailuresMakeNoExternalChargeAndRemainAuditable(t *testing.T) {
	env := newIntegrationEnv(t)
	clock := &testClock{now: time.Date(2026, 8, 15, 17, 0, 0, 0, time.UTC)}

	missingFixture := createIntegrationFixture(t, env, "missing-provider", clock.now)
	missingIntent := triggerAutomaticIntent(t, env, missingFixture, clock.now, "missing-provider")
	missingService, err := New(Options{
		Store: env.store, ReferenceResolver: testReferenceResolver{value: "fake-method"},
		LeaseOwner: "missing-provider-worker", ChargeEnabled: map[string]bool{"fake": true}, Now: clock.Current,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missingService.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := env.store.GetPaymentIntent(context.Background(), missingIntent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateFailed {
		t.Fatalf("missing provider intent=%+v err=%v", stored, err)
	}

	clock.Add(time.Minute)
	resolverFixture := createIntegrationFixture(t, env, "resolver-failure", clock.now)
	resolverIntent := triggerAutomaticIntent(t, env, resolverFixture, clock.now, "resolver-failure")
	provider := fake.New("webhook-secret")
	resolverService, err := New(Options{
		Store: env.store, Providers: []payment.PaymentProvider{provider},
		ReferenceResolver: testReferenceResolver{err: errors.New("kms unavailable")},
		LeaseOwner:        "resolver-worker", ChargeEnabled: map[string]bool{"fake": true}, Now: clock.Current,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolverService.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err = env.store.GetPaymentIntent(context.Background(), resolverIntent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateFailed || len(provider.ChargeCalls()) != 0 {
		t.Fatalf("resolver intent=%+v charge_calls=%d err=%v", stored, len(provider.ChargeCalls()), err)
	}

	clock.Add(time.Minute)
	invalidFixture := createIntegrationFixture(t, env, "invalid-result", clock.now)
	invalidIntent := triggerAutomaticIntent(t, env, invalidFixture, clock.now, "invalid-result")
	invalidProvider := fake.New("webhook-secret")
	invalidProvider.QueueCharge(fake.Outcome{Result: payment.ProviderResult{State: payment.PaymentIntentStateProcessing, ProviderCode: "bad_state"}})
	invalidService := newIntegrationService(t, env, invalidProvider, clock, true)
	if _, err := invalidService.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err = env.store.GetPaymentIntent(context.Background(), invalidIntent.ID)
	if err != nil || stored.State != payment.PaymentIntentStateUnknown {
		t.Fatalf("invalid provider result intent=%+v err=%v", stored, err)
	}
}

func TestServiceConfigurationRejectsMissingAndDuplicateDependencies(t *testing.T) {
	env := newIntegrationEnv(t)
	provider := fake.New("secret")
	valid := Options{
		Store: env.store, Providers: []payment.PaymentProvider{provider},
		ReferenceResolver: testReferenceResolver{value: "method"}, LeaseOwner: "worker",
	}
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{name: "missing store", mutate: func(options *Options) { options.Store = nil }},
		{name: "missing resolver", mutate: func(options *Options) { options.ReferenceResolver = nil }},
		{name: "missing owner", mutate: func(options *Options) { options.LeaseOwner = "" }},
		{name: "nil provider", mutate: func(options *Options) { options.Providers = []payment.PaymentProvider{nil} }},
		{name: "duplicate provider", mutate: func(options *Options) { options.Providers = []payment.PaymentProvider{provider, provider} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			options := valid
			tc.mutate(&options)
			if _, err := New(options); err == nil {
				t.Fatal("invalid service configuration should fail")
			}
		})
	}
}
