package paymentstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

type paymentIntegrationEnv struct {
	store *Store
	db    *pgxpool.Pool
}

func newPaymentIntegrationEnv(t *testing.T) paymentIntegrationEnv {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
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
	return paymentIntegrationEnv{store: New(db), db: db}
}

type paymentFixture struct {
	account payment.CommercialAccount
	method  payment.PaymentMethod
	policy  payment.AutoTopUpPolicy
}

func createPaymentFixture(t *testing.T, env paymentIntegrationEnv, suffix string, initialBalance, threshold, topUp int64) paymentFixture {
	t.Helper()
	ctx := context.Background()
	organizationID := testutil.OrganizationID("payment-" + suffix)
	actorID := "test-user-" + suffix
	account, created, err := env.store.EnsureCommercialAccount(ctx, organizationID, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("commercial account should be created")
	}
	if initialBalance != 0 {
		result, err := env.store.PostLedgerEntry(ctx, PostLedgerEntryInput{
			AccountID:        account.ID,
			Direction:        payment.LedgerDirectionCredit,
			AmountMinor:      initialBalance,
			Currency:         payment.CurrencyTWD,
			Reason:           payment.LedgerReasonManualAdjustmentCredit,
			IdempotencyScope: "fixture",
			IdempotencyKey:   "initial-" + suffix,
			ExternalType:     "test_fixture",
			ExternalID:       suffix,
			ActorType:        "test",
			ActorID:          "integration",
			RequestID:        "fixture-" + suffix,
			Now:              testTime(9, 0),
		})
		if err != nil {
			t.Fatal(err)
		}
		account = result.Account
	}

	methodConsent, err := env.store.CreateConsent(ctx, CreateConsentInput{
		AccountID:         account.ID,
		ConsentType:       "payment_method",
		TextVersion:       "payment-method-v1",
		TextSHA256:        strings.Repeat("a", 64),
		AcceptedActorType: "user",
		AcceptedActorID:   actorID,
		AcceptedAt:        testTime(9, 5),
		Locale:            "zh-TW",
		Source:            "integration-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	method, err := env.store.CreatePaymentMethod(ctx, CreatePaymentMethodInput{
		AccountID:                     account.ID,
		Provider:                      "fake",
		ProviderCustomerRefCiphertext: []byte("encrypted-customer"),
		ProviderMethodRefCiphertext:   []byte("encrypted-method"),
		ProviderMethodRefSHA256:       strings.Repeat("b", 64),
		CardBrand:                     "test",
		LastFour:                      "4242",
		Capabilities: payment.ProviderCapabilities{
			VaultedMethod:           true,
			MerchantInitiatedCharge: true,
			StatusQuery:             true,
			Webhook:                 true,
		},
		Status:    payment.PaymentMethodStatusActive,
		ConsentID: methodConsent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	policyConsent, err := env.store.CreateConsent(ctx, CreateConsentInput{
		AccountID:         account.ID,
		ConsentType:       "auto_topup",
		TextVersion:       "auto-topup-v1",
		TextSHA256:        strings.Repeat("c", 64),
		AcceptedActorType: "user",
		AcceptedActorID:   actorID,
		AcceptedAt:        testTime(9, 10),
		Locale:            "zh-TW",
		Source:            "integration-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := env.store.PutAutoTopUpPolicy(ctx, PutAutoTopUpPolicyInput{
		AccountID:             account.ID,
		Enabled:               true,
		ThresholdMinor:        threshold,
		TopUpAmountMinor:      topUp,
		Currency:              payment.CurrencyTWD,
		PaymentMethodID:       method.ID,
		DailyAttemptLimit:     3,
		DailyAmountLimitMinor: topUp * 3,
		CooldownSeconds:       3600,
		ConsentID:             policyConsent.ID,
		ActorID:               actorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return paymentFixture{account: account, method: method, policy: policy}
}

func testTime(hour, minute int) time.Time {
	return time.Date(2026, 8, 15, hour, minute, 0, 0, time.UTC)
}

func debitInput(accountID, key string, amount int64, now time.Time) PostLedgerEntryInput {
	return PostLedgerEntryInput{
		AccountID:        accountID,
		Direction:        payment.LedgerDirectionDebit,
		AmountMinor:      amount,
		Currency:         payment.CurrencyTWD,
		Reason:           payment.LedgerReasonInvoiceDebit,
		IdempotencyScope: "invoice",
		IdempotencyKey:   key,
		ExternalType:     "invoice",
		ExternalID:       key,
		ActorType:        "service",
		ActorID:          "invoice-test",
		RequestID:        "request-" + key,
		Now:              now,
	}
}

func TestLedgerTriggerAndIntentCreditAreExactlyOnce(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "exactly-once", 20000, 10000, 50000)
	ctx := context.Background()

	input := debitInput(fixture.account.ID, "invoice-1", 11000, testTime(10, 0))
	result, err := env.store.PostLedgerEntry(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Account.AvailableBalanceMinor != 9000 || result.Intent == nil || result.Duplicate {
		t.Fatalf("unexpected debit result: %+v", result)
	}
	intentID := result.Intent.ID

	duplicate, err := env.store.PostLedgerEntry(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate || duplicate.Entry.ID != result.Entry.ID || duplicate.Intent == nil || duplicate.Intent.ID != intentID {
		t.Fatalf("unexpected duplicate result: %+v", duplicate)
	}
	retried := input
	retried.RequestID = "request-invoice-1-retry"
	requestRetry, err := env.store.PostLedgerEntry(ctx, retried)
	if err != nil || !requestRetry.Duplicate || requestRetry.Entry.ID != result.Entry.ID {
		t.Fatalf("a transport retry may use a new request id: result=%+v err=%v", requestRetry, err)
	}
	conflicting := input
	conflicting.AmountMinor = 12000
	if _, err := env.store.PostLedgerEntry(ctx, conflicting); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting idempotency should fail, got %v", err)
	}

	for _, transition := range []TransitionIntentInput{
		{IntentID: intentID, ToState: payment.PaymentIntentStateProcessing, Now: testTime(10, 1)},
		{IntentID: intentID, ToState: payment.PaymentIntentStateUnknown, ProviderTransactionReference: "fake-txn-1", Now: testTime(10, 2)},
		{IntentID: intentID, ToState: payment.PaymentIntentStateSucceeded, ProviderTransactionReference: "fake-txn-1", Now: testTime(10, 3)},
	} {
		if _, err := env.store.TransitionIntent(ctx, transition); err != nil {
			t.Fatalf("transition %+v: %v", transition, err)
		}
	}

	succeeded, err := env.store.TransitionIntent(ctx, TransitionIntentInput{
		IntentID:                     intentID,
		ToState:                      payment.PaymentIntentStateSucceeded,
		ProviderTransactionReference: "fake-txn-1",
		Now:                          testTime(10, 4),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !succeeded.Duplicate || succeeded.Account.AvailableBalanceMinor != 59000 || succeeded.CreditEntry == nil {
		t.Fatalf("unexpected duplicate success: %+v", succeeded)
	}
	entries, err := env.store.ListLedgerEntries(ctx, fixture.account.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("ledger entries=%d, want initial+debit+credit", len(entries))
	}
	policy, err := env.store.GetAutoTopUpPolicy(ctx, fixture.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Armed || policy.LastSucceededAt == nil {
		t.Fatalf("policy should re-arm after sufficient credit: %+v", policy)
	}
}

func TestConcurrentDebitsCreateOneAutomaticIntent(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "concurrent", 20000, 10000, 50000)
	ctx := context.Background()

	inputs := []PostLedgerEntryInput{
		debitInput(fixture.account.ID, "invoice-a", 6000, testTime(11, 0)),
		debitInput(fixture.account.ID, "invoice-b", 6000, testTime(11, 0)),
	}
	results := make([]PostLedgerEntryResult, len(inputs))
	errs := make([]error, len(inputs))
	var wg sync.WaitGroup
	for i := range inputs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = env.store.PostLedgerEntry(ctx, inputs[i])
		}(i)
	}
	wg.Wait()
	intentCount := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("debit %d: %v", i, err)
		}
		if results[i].Intent != nil {
			intentCount++
		}
	}
	if intentCount != 1 {
		t.Fatalf("automatic intents=%d, want 1", intentCount)
	}
	account, err := env.store.GetCommercialAccount(ctx, fixture.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.AvailableBalanceMinor != 8000 {
		t.Fatalf("balance=%d, want 8000", account.AvailableBalanceMinor)
	}
	var dbIntentCount int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM payment_intents WHERE account_id = $1`, fixture.account.ID).Scan(&dbIntentCount); err != nil {
		t.Fatal(err)
	}
	if dbIntentCount != 1 {
		t.Fatalf("database intents=%d, want 1", dbIntentCount)
	}
}

func TestInsufficientTopUpRequiresAttentionWithoutRecursiveCharge(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "attention", 0, 10000, 10000)
	ctx := context.Background()

	debit, err := env.store.PostLedgerEntry(ctx, debitInput(fixture.account.ID, "deep-debit", 30000, testTime(12, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if debit.Intent == nil {
		t.Fatal("deep debit should create one intent")
	}
	if _, err := env.store.TransitionIntent(ctx, TransitionIntentInput{IntentID: debit.Intent.ID, ToState: payment.PaymentIntentStateProcessing, Now: testTime(12, 1)}); err != nil {
		t.Fatal(err)
	}
	success, err := env.store.TransitionIntent(ctx, TransitionIntentInput{
		IntentID:                     debit.Intent.ID,
		ToState:                      payment.PaymentIntentStateSucceeded,
		ProviderTransactionReference: "fake-deep-1",
		Now:                          testTime(12, 2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if success.Account.AvailableBalanceMinor != -20000 || success.Account.State != payment.AccountStateAttentionRequired {
		t.Fatalf("unexpected attention account: %+v", success.Account)
	}
	policy, err := env.store.GetAutoTopUpPolicy(ctx, fixture.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Armed {
		t.Fatal("insufficient top-up must remain disarmed")
	}
	if _, err := env.store.PostLedgerEntry(ctx, debitInput(fixture.account.ID, "another-debit", 1000, testTime(12, 3))); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM payment_intents WHERE account_id = $1`, fixture.account.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recursive intent count=%d, want 1", count)
	}
}

func TestRefundCompensationDisarmsAutoTopUpWithoutRecharge(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "refund-no-recharge", 20000, 10000, 50000)
	ctx := context.Background()

	debit, err := env.store.PostLedgerEntry(ctx, debitInput(fixture.account.ID, "refund-origin", 11000, testTime(12, 10)))
	if err != nil || debit.Intent == nil {
		t.Fatalf("origin debit=%+v err=%v", debit, err)
	}
	if _, err := env.store.TransitionIntent(ctx, TransitionIntentInput{
		IntentID: debit.Intent.ID, ToState: payment.PaymentIntentStateProcessing, Now: testTime(12, 11),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.TransitionIntent(ctx, TransitionIntentInput{
		IntentID: debit.Intent.ID, ToState: payment.PaymentIntentStateSucceeded,
		ProviderTransactionReference: "fake-refund-origin", Now: testTime(12, 12),
	}); err != nil {
		t.Fatal(err)
	}
	refundInput := PostLedgerEntryInput{
		AccountID: fixture.account.ID, Direction: payment.LedgerDirectionDebit,
		AmountMinor: 50000, Currency: payment.CurrencyTWD, Reason: payment.LedgerReasonRefundDebit,
		IdempotencyScope: "provider_refund", IdempotencyKey: "refund-1",
		ExternalType: "payment_intent", ExternalID: debit.Intent.ID,
		ActorType: "service", ActorID: "payment_worker", RequestID: "refund-request-1", Now: testTime(12, 13),
	}
	refund, err := env.store.PostLedgerEntry(ctx, refundInput)
	if err != nil || refund.Intent != nil || refund.Account.AvailableBalanceMinor != 9000 || refund.Account.State != payment.AccountStateAttentionRequired {
		t.Fatalf("refund=%+v err=%v", refund, err)
	}
	policy, err := env.store.GetAutoTopUpPolicy(ctx, fixture.account.ID)
	if err != nil || policy.Armed {
		t.Fatalf("policy=%+v err=%v", policy, err)
	}
	retry := refundInput
	retry.RequestID = "refund-request-2"
	duplicate, err := env.store.PostLedgerEntry(ctx, retry)
	if err != nil || !duplicate.Duplicate || duplicate.Entry.ID != refund.Entry.ID || duplicate.Intent != nil {
		t.Fatalf("duplicate refund=%+v err=%v", duplicate, err)
	}
	if next, err := env.store.PostLedgerEntry(ctx, debitInput(fixture.account.ID, "after-refund", 1000, testTime(12, 14))); err != nil || next.Intent != nil {
		t.Fatalf("post-refund debit must not recharge: result=%+v err=%v", next, err)
	}
	var intents, refunds int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM payment_intents WHERE account_id = $1`, fixture.account.ID).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM balance_ledger_entries WHERE account_id = $1 AND reason = 'refund_debit'`, fixture.account.ID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if intents != 1 || refunds != 1 {
		t.Fatalf("intents=%d refunds=%d", intents, refunds)
	}

	withoutPolicy := createPaymentFixture(t, env, "chargeback-no-policy", 20000, 10000, 50000)
	if _, err := env.db.Exec(ctx, `DELETE FROM auto_topup_policies WHERE account_id = $1`, withoutPolicy.account.ID); err != nil {
		t.Fatal(err)
	}
	chargeback, err := env.store.PostLedgerEntry(ctx, PostLedgerEntryInput{
		AccountID: withoutPolicy.account.ID, Direction: payment.LedgerDirectionDebit,
		AmountMinor: 100, Currency: payment.CurrencyTWD, Reason: payment.LedgerReasonChargebackDebit,
		IdempotencyScope: "provider_chargeback", IdempotencyKey: "chargeback-no-policy-1",
		ExternalType: "payment_intent", ExternalID: "external-payment-without-policy",
		ActorType: "service", ActorID: "payment_worker", RequestID: "chargeback-request-1", Now: testTime(12, 15),
	})
	if err != nil || chargeback.Intent != nil || chargeback.Account.AvailableBalanceMinor != 19900 || chargeback.Account.State != payment.AccountStateActive {
		t.Fatalf("chargeback without policy=%+v err=%v", chargeback, err)
	}
}

func TestBalanceLedgerRejectsMutationAtDatabaseBoundary(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "append-only", 10000, 5000, 10000)
	ctx := context.Background()
	entries, err := env.store.ListLedgerEntries(ctx, fixture.account.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d, want 1", len(entries))
	}
	if _, err := env.db.Exec(ctx, `UPDATE balance_ledger_entries SET amount_minor = amount_minor + 100 WHERE id = $1`, entries[0].ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("ledger update should be rejected, got %v", err)
	}
	if _, err := env.db.Exec(ctx, `DELETE FROM balance_ledger_entries WHERE id = $1`, entries[0].ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("ledger delete should be rejected, got %v", err)
	}
}

func TestCommercialAccountAndLedgerIdempotencyUnderConcurrency(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "idempotency", 20000, 10000, 50000)
	ctx := context.Background()

	again, created, err := env.store.EnsureCommercialAccount(ctx, fixture.account.OrganizationID, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	if created || again.ID != fixture.account.ID {
		t.Fatalf("ensure should return existing account: created=%v account=%+v", created, again)
	}

	input := debitInput(fixture.account.ID, "shared-invoice", 11000, testTime(13, 0))
	results := make([]PostLedgerEntryResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = env.store.PostLedgerEntry(ctx, input)
		}(i)
	}
	wg.Wait()
	duplicates := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		if results[i].Duplicate {
			duplicates++
		}
	}
	if duplicates != 1 || results[0].Entry.ID != results[1].Entry.ID {
		t.Fatalf("concurrent duplicate results=%+v", results)
	}

	entries, err := env.store.ListLedgerEntries(ctx, fixture.account.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var projected int64
	for _, entry := range entries {
		if entry.Direction == payment.LedgerDirectionCredit {
			projected += entry.AmountMinor
		} else {
			projected -= entry.AmountMinor
		}
	}
	account, err := env.store.GetCommercialAccount(ctx, fixture.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projected != account.AvailableBalanceMinor || projected != 9000 {
		t.Fatalf("ledger sum=%d account projection=%d", projected, account.AvailableBalanceMinor)
	}
}

func TestPolicyRequiresVersionConsentActiveMethodAndCapability(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "policy-guard", 20000, 10000, 50000)
	ctx := context.Background()

	input := PutAutoTopUpPolicyInput{
		AccountID:             fixture.account.ID,
		Enabled:               true,
		ThresholdMinor:        15000,
		TopUpAmountMinor:      50000,
		Currency:              payment.CurrencyTWD,
		PaymentMethodID:       fixture.method.ID,
		DailyAttemptLimit:     3,
		DailyAmountLimitMinor: 150000,
		CooldownSeconds:       3600,
		ConsentID:             fixture.policy.ConsentID,
		ActorID:               "policy-admin",
		ExpectedVersion:       fixture.policy.Version + 1,
	}
	if _, err := env.store.PutAutoTopUpPolicy(ctx, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale expected version should fail, got %v", err)
	}

	input.ExpectedVersion = fixture.policy.Version
	updated, err := env.store.PutAutoTopUpPolicy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != fixture.policy.Version+1 || updated.Generation != fixture.policy.Generation+1 {
		t.Fatalf("unexpected policy generation: %+v", updated)
	}

	if _, err := env.db.Exec(ctx, `UPDATE payment_methods SET capabilities = '{"merchant_initiated_charge":false}'::jsonb WHERE id = $1`, fixture.method.ID); err != nil {
		t.Fatal(err)
	}
	input.ExpectedVersion = updated.Version
	if _, err := env.store.PutAutoTopUpPolicy(ctx, input); !errors.Is(err, payment.ErrCapabilityUnsupported) {
		t.Fatalf("unsupported merchant-initiated charge should fail, got %v", err)
	}

	if _, err := env.db.Exec(ctx, `UPDATE payment_methods SET status = 'revoked' WHERE id = $1`, fixture.method.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PutAutoTopUpPolicy(ctx, input); !errors.Is(err, payment.ErrPaymentMethodInactive) {
		t.Fatalf("inactive method should fail, got %v", err)
	}
}

func TestCustomerPaymentReadsManualTopUpAndPolicyDisable(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "customer-api", 20000, 10000, 50000)
	ctx := context.Background()

	account, err := env.store.GetCommercialAccountByOrganization(ctx, fixture.account.OrganizationID, payment.CurrencyTWD)
	if err != nil || account.ID != fixture.account.ID {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	methods, err := env.store.ListPaymentMethods(ctx, account.ID, 25, 0)
	if err != nil || methods.Total != 1 || len(methods.Methods) != 1 || methods.Methods[0].ID != fixture.method.ID {
		t.Fatalf("methods=%+v err=%v", methods, err)
	}
	method, err := env.store.GetPaymentMethod(ctx, account.ID, fixture.method.ID)
	if err != nil || method.LastFour != "4242" {
		t.Fatalf("method=%+v err=%v", method, err)
	}
	ledgerPage, err := env.store.ListLedgerEntriesPage(ctx, account.ID, 25, 0)
	if err != nil || ledgerPage.Total == 0 || len(ledgerPage.Entries) == 0 {
		t.Fatalf("ledger page=%+v err=%v", ledgerPage, err)
	}

	input := CreateManualTopUpInput{
		AccountID: account.ID, AmountMinor: 30000, Currency: payment.CurrencyTWD,
		PaymentMethodID: fixture.method.ID, IdempotencyKey: "manual-1",
		CorrelationID: "request-manual-1", Now: testTime(15, 0),
	}
	created, err := env.store.CreateManualTopUp(ctx, input)
	if err != nil || created.Duplicate || created.Intent.Reason != payment.PaymentIntentReasonManualTopUp {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	duplicate, err := env.store.CreateManualTopUp(ctx, input)
	if err != nil || !duplicate.Duplicate || duplicate.Intent.ID != created.Intent.ID {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	conflict := input
	conflict.AmountMinor = 40000
	if _, err := env.store.CreateManualTopUp(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("manual idempotency conflict err=%v", err)
	}
	intents, err := env.store.ListPaymentIntents(ctx, account.ID, 25, 0)
	if err != nil || intents.Total != 1 || len(intents.Intents) != 1 {
		t.Fatalf("intents=%+v err=%v", intents, err)
	}
	intent, err := env.store.GetPaymentIntentForAccount(ctx, account.ID, created.Intent.ID)
	if err != nil || intent.ID != created.Intent.ID {
		t.Fatalf("intent=%+v err=%v", intent, err)
	}
	attempts, err := env.store.ListPaymentAttempts(ctx, intent.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("attempts=%+v err=%v", attempts, err)
	}
	claimed, err := env.store.ClaimPaymentJobs(ctx, testTime(15, 1), testTime(14, 0), "customer-api-worker", 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	if _, err := env.store.BeginProviderAttempt(ctx, BeginProviderAttemptInput{
		JobID: claimed[0].ID, LeaseOwner: "customer-api-worker", Operation: payment.ProviderOperationCharge, Now: testTime(15, 1),
	}); err != nil {
		t.Fatal(err)
	}
	attempts, err = env.store.ListPaymentAttempts(ctx, intent.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Operation != payment.ProviderOperationCharge {
		t.Fatalf("recorded attempts=%+v err=%v", attempts, err)
	}
	var jobs int
	if err := env.db.QueryRow(ctx, `SELECT count(*)::int FROM payment_reconciliation_jobs WHERE intent_id = $1 AND reason = 'charge'`, intent.ID).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("jobs=%d err=%v", jobs, err)
	}

	disabled, err := env.store.DisableAutoTopUpPolicy(ctx, DisableAutoTopUpPolicyInput{
		AccountID: account.ID, ActorID: "billing-owner", ExpectedVersion: fixture.policy.Version,
	})
	if err != nil || disabled.Enabled || disabled.Armed || disabled.Version != fixture.policy.Version+1 || disabled.Generation != fixture.policy.Generation+1 {
		t.Fatalf("disabled=%+v err=%v", disabled, err)
	}
	if _, err := env.store.DisableAutoTopUpPolicy(ctx, DisableAutoTopUpPolicyInput{
		AccountID: account.ID, ActorID: "billing-owner", ExpectedVersion: fixture.policy.Version,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale disable err=%v", err)
	}
	idempotent, err := env.store.DisableAutoTopUpPolicy(ctx, DisableAutoTopUpPolicyInput{
		AccountID: account.ID, ActorID: "billing-owner", ExpectedVersion: disabled.Version,
	})
	if err != nil || idempotent.ID != disabled.ID || idempotent.Enabled {
		t.Fatalf("idempotent disable=%+v err=%v", idempotent, err)
	}
}

func TestCustomerStoreRejectsInvalidInputsAndBoundsPages(t *testing.T) {
	ctx := context.Background()
	store := New(nil)
	checks := []struct {
		name string
		run  func() error
	}{
		{"list methods", func() error { _, err := store.ListPaymentMethods(ctx, "", 0, 0); return err }},
		{"get method", func() error { _, err := store.GetPaymentMethod(ctx, "", ""); return err }},
		{"revoke method", func() error { _, err := store.RevokePaymentMethod(ctx, RevokePaymentMethodInput{}); return err }},
		{"disable policy", func() error { _, err := store.DisableAutoTopUpPolicy(ctx, DisableAutoTopUpPolicyInput{}); return err }},
		{"manual topup", func() error { _, err := store.CreateManualTopUp(ctx, CreateManualTopUpInput{}); return err }},
		{"list intents", func() error { _, err := store.ListPaymentIntents(ctx, "", 0, 0); return err }},
		{"get intent", func() error { _, err := store.GetPaymentIntentForAccount(ctx, "", ""); return err }},
		{"list attempts", func() error { _, err := store.ListPaymentAttempts(ctx, ""); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, ErrConflict) {
				t.Fatalf("error=%v, want conflict", err)
			}
		})
	}
	if limit, offset := boundedPage(0, -10); limit != 100 || offset != 0 {
		t.Fatalf("default bounded page=%d/%d", limit, offset)
	}
	if limit, offset := boundedPage(201, 12); limit != 100 || offset != 12 {
		t.Fatalf("maximum bounded page=%d/%d", limit, offset)
	}
}

func TestPaymentMethodRevocationDisablesPolicyAndPreservesHistory(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "revoke-api", 20000, 10000, 50000)
	ctx := context.Background()

	result, err := env.store.RevokePaymentMethod(ctx, RevokePaymentMethodInput{
		AccountID: fixture.account.ID, MethodID: fixture.method.ID,
		ActorID: "billing-owner", Reason: "customer requested revocation", Now: testTime(16, 0),
	})
	if err != nil || result.Duplicate || !result.PolicyDisabled || result.Method.Status != payment.PaymentMethodStatusRevoked {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	policy, err := env.store.GetAutoTopUpPolicy(ctx, fixture.account.ID)
	if err != nil || policy.Enabled || policy.Armed || policy.Generation != fixture.policy.Generation+1 {
		t.Fatalf("policy=%+v err=%v", policy, err)
	}
	var revokedAt *time.Time
	var reason *string
	if err := env.db.QueryRow(ctx, `SELECT revoked_at, revocation_reason FROM payment_consents WHERE id = $1`, fixture.method.ConsentID).Scan(&revokedAt, &reason); err != nil || revokedAt == nil || reason == nil || *reason != "customer requested revocation" {
		t.Fatalf("revoked_at=%v reason=%v err=%v", revokedAt, reason, err)
	}
	duplicate, err := env.store.RevokePaymentMethod(ctx, RevokePaymentMethodInput{
		AccountID: fixture.account.ID, MethodID: fixture.method.ID,
		ActorID: "billing-owner", Reason: "repeat", Now: testTime(16, 1),
	})
	if err != nil || !duplicate.Duplicate || duplicate.Method.Status != payment.PaymentMethodStatusRevoked {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	if _, err := env.store.CreateManualTopUp(ctx, CreateManualTopUpInput{
		AccountID: fixture.account.ID, AmountMinor: 10000, Currency: payment.CurrencyTWD,
		PaymentMethodID: fixture.method.ID, IdempotencyKey: "revoked", CorrelationID: "revoked",
	}); !errors.Is(err, payment.ErrPaymentMethodInactive) {
		t.Fatalf("revoked manual top-up err=%v", err)
	}
}

func TestPaymentConsentVersionPreservedAndAccountOwnershipEnforced(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	first := createPaymentFixture(t, env, "consent-owner-a", 0, 10000, 50000)
	second := createPaymentFixture(t, env, "consent-owner-b", 0, 10000, 50000)
	ctx := context.Background()

	consent, err := env.store.CreateConsent(ctx, CreateConsentInput{
		AccountID:         first.account.ID,
		ConsentType:       "payment_method",
		TextVersion:       " Payment-Method-V2 ",
		TextSHA256:        strings.Repeat("D", 64),
		AcceptedActorType: "user",
		AcceptedActorID:   "consent-user",
		AcceptedAt:        testTime(14, 0),
		Locale:            "zh-TW",
		Source:            "integration-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if consent.TextVersion != "Payment-Method-V2" || consent.TextSHA256 != strings.Repeat("d", 64) {
		t.Fatalf("unexpected normalized consent: %+v", consent)
	}

	_, err = env.store.CreatePaymentMethod(ctx, CreatePaymentMethodInput{
		AccountID:                     second.account.ID,
		Provider:                      "fake",
		ProviderCustomerRefCiphertext: []byte("encrypted-customer"),
		ProviderMethodRefCiphertext:   []byte("encrypted-method"),
		ProviderMethodRefSHA256:       strings.Repeat("e", 64),
		Capabilities:                  payment.ProviderCapabilities{MerchantInitiatedCharge: true},
		Status:                        payment.PaymentMethodStatusActive,
		ConsentID:                     consent.ID,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-account consent should fail, got %v", err)
	}
}

func TestLedgerGuardsOverflowClosedAndSuspendedAccounts(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()

	overflow := createPaymentFixture(t, env, "overflow", 0, 10000, 50000)
	if _, err := env.db.Exec(ctx, `UPDATE commercial_accounts SET available_balance_minor = $2 WHERE id = $1`, overflow.account.ID, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	credit := PostLedgerEntryInput{
		AccountID: overflow.account.ID, Direction: payment.LedgerDirectionCredit,
		AmountMinor: 100, Currency: payment.CurrencyTWD, Reason: payment.LedgerReasonManualAdjustmentCredit,
		IdempotencyScope: "overflow", IdempotencyKey: "credit", Now: testTime(15, 0),
	}
	if _, err := env.store.PostLedgerEntry(ctx, credit); !errors.Is(err, payment.ErrBalanceOverflow) {
		t.Fatalf("overflow should fail, got %v", err)
	}

	closed := createPaymentFixture(t, env, "closed", 10000, 5000, 10000)
	if _, err := env.db.Exec(ctx, `UPDATE commercial_accounts SET state = 'closed' WHERE id = $1`, closed.account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PostLedgerEntry(ctx, debitInput(closed.account.ID, "closed-debit", 1000, testTime(15, 1))); !errors.Is(err, ErrAccountClosed) {
		t.Fatalf("closed account should reject entries, got %v", err)
	}

	suspended := createPaymentFixture(t, env, "suspended", 10000, 5000, 10000)
	if _, err := env.db.Exec(ctx, `UPDATE commercial_accounts SET state = 'suspended' WHERE id = $1`, suspended.account.ID); err != nil {
		t.Fatal(err)
	}
	result, err := env.store.PostLedgerEntry(ctx, debitInput(suspended.account.ID, "suspended-debit", 6000, testTime(15, 2)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Intent != nil || result.Account.State != payment.AccountStateSuspended || result.Account.AvailableBalanceMinor != 4000 {
		t.Fatalf("suspended account may record usage but must not charge: %+v", result)
	}
}

func TestIntentTransitionRejectsIllegalStateAndProviderReferenceConflict(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "intent-guard", 20000, 10000, 50000)
	ctx := context.Background()
	debit, err := env.store.PostLedgerEntry(ctx, debitInput(fixture.account.ID, "intent-debit", 11000, testTime(16, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if debit.Intent == nil {
		t.Fatal("expected automatic intent")
	}
	intentID := debit.Intent.ID
	if _, err := env.store.TransitionIntent(ctx, TransitionIntentInput{IntentID: intentID, ToState: payment.PaymentIntentStateSucceeded}); !errors.Is(err, payment.ErrInvalidTransition) {
		t.Fatalf("created -> succeeded should fail, got %v", err)
	}
	if _, err := env.store.TransitionIntent(ctx, TransitionIntentInput{
		IntentID: intentID, ToState: payment.PaymentIntentStateProcessing,
		ProviderTransactionReference: "provider-txn-a", Now: testTime(16, 1),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.TransitionIntent(ctx, TransitionIntentInput{
		IntentID: intentID, ToState: payment.PaymentIntentStateUnknown,
		ProviderTransactionReference: "provider-txn-b", Now: testTime(16, 2),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("provider reference replacement should fail, got %v", err)
	}
}

func TestFirstFailedIntentKeepsAccountActiveAndManualCreditRearmsPolicy(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "manual-recovery", 10000, 5000, 10000)
	ctx := context.Background()
	debit, err := env.store.PostLedgerEntry(ctx, debitInput(fixture.account.ID, "recovery-debit", 6000, testTime(17, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if debit.Intent == nil {
		t.Fatal("expected automatic intent")
	}
	if _, err := env.store.TransitionIntent(ctx, TransitionIntentInput{
		IntentID: debit.Intent.ID, ToState: payment.PaymentIntentStateProcessing, Now: testTime(17, 1),
	}); err != nil {
		t.Fatal(err)
	}
	failed, err := env.store.TransitionIntent(ctx, TransitionIntentInput{
		IntentID: debit.Intent.ID, ToState: payment.PaymentIntentStateFailed, Now: testTime(17, 2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Account.State != payment.AccountStateActive {
		t.Fatalf("first failed charge should keep account active: %+v", failed.Account)
	}
	policyAfterFailure, err := env.store.GetAutoTopUpPolicy(ctx, fixture.account.ID)
	if err != nil || policyAfterFailure.ConsecutiveFailureCount != 1 || !policyAfterFailure.Enabled {
		t.Fatalf("policy after first failure=%+v err=%v", policyAfterFailure, err)
	}

	recovery, err := env.store.PostLedgerEntry(ctx, PostLedgerEntryInput{
		AccountID: fixture.account.ID, Direction: payment.LedgerDirectionCredit,
		AmountMinor: 2000, Currency: payment.CurrencyTWD, Reason: payment.LedgerReasonManualAdjustmentCredit,
		IdempotencyScope: "support", IdempotencyKey: "recovery-credit",
		ActorType: "admin", ActorID: "support-user", RequestID: "recovery-request", Now: testTime(17, 3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Account.AvailableBalanceMinor != 6000 || recovery.Account.State != payment.AccountStateActive {
		t.Fatalf("unexpected recovered account: %+v", recovery.Account)
	}
	policy, err := env.store.GetAutoTopUpPolicy(ctx, fixture.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Armed {
		t.Fatal("sufficient manual credit should re-arm policy")
	}
	intent, err := env.store.GetPaymentIntent(ctx, debit.Intent.ID)
	if err != nil || intent.State != payment.PaymentIntentStateFailed {
		t.Fatalf("stored intent=%+v err=%v", intent, err)
	}
}

func TestThirdConsecutiveAutoTopUpFailureDisablesPolicy(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "three-failures", 10000, 5000, 300)
	ctx := context.Background()

	failAttempt := func(sequence, hour int) payment.CommercialAccount {
		t.Helper()
		debit, err := env.store.PostLedgerEntry(ctx, debitInput(
			fixture.account.ID, fmt.Sprintf("three-failures-debit-%d", sequence), 6000, testTime(hour, 0),
		))
		if err != nil || debit.Intent == nil {
			t.Fatalf("attempt %d debit=%+v err=%v", sequence, debit, err)
		}
		if _, err := env.store.TransitionIntent(ctx, TransitionIntentInput{
			IntentID: debit.Intent.ID, ToState: payment.PaymentIntentStateProcessing, Now: testTime(hour, 1),
		}); err != nil {
			t.Fatal(err)
		}
		failed, err := env.store.TransitionIntent(ctx, TransitionIntentInput{
			IntentID: debit.Intent.ID, ToState: payment.PaymentIntentStateFailed, Now: testTime(hour, 2),
		})
		if err != nil {
			t.Fatal(err)
		}
		return failed.Account
	}

	for sequence, hour := range []int{10, 12, 14} {
		account := failAttempt(sequence+1, hour)
		policy, err := env.store.GetAutoTopUpPolicy(ctx, fixture.account.ID)
		if err != nil {
			t.Fatal(err)
		}
		if policy.ConsecutiveFailureCount != sequence+1 {
			t.Fatalf("attempt %d failure count=%d", sequence+1, policy.ConsecutiveFailureCount)
		}
		if sequence < 2 {
			if !policy.Enabled || account.State != payment.AccountStateActive {
				t.Fatalf("attempt %d policy=%+v account=%+v", sequence+1, policy, account)
			}
			credit, err := env.store.PostLedgerEntry(ctx, PostLedgerEntryInput{
				AccountID: fixture.account.ID, Direction: payment.LedgerDirectionCredit,
				AmountMinor: 6000, Currency: payment.CurrencyTWD, Reason: payment.LedgerReasonManualAdjustmentCredit,
				IdempotencyScope: "support", IdempotencyKey: fmt.Sprintf("three-failures-credit-%d", sequence+1),
				ActorType: "admin", ActorID: "support-user", RequestID: fmt.Sprintf("three-failures-request-%d", sequence+1),
				Now: testTime(hour, 3),
			})
			if err != nil {
				t.Fatalf("attempt %d recovery=%+v err=%v", sequence+1, credit, err)
			}
			rearmed, err := env.store.GetAutoTopUpPolicy(ctx, fixture.account.ID)
			if err != nil || !rearmed.Armed {
				t.Fatalf("attempt %d rearmed policy=%+v err=%v", sequence+1, rearmed, err)
			}
		} else if policy.Enabled || account.State != payment.AccountStateAttentionRequired {
			t.Fatalf("third failure must disable policy and require attention: policy=%+v account=%+v", policy, account)
		}
	}
}

func TestPaymentJobLeaseOwnershipRetryAndCompletion(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "job-lease", 20000, 10000, 50000)
	ctx := context.Background()
	debit, err := env.store.PostLedgerEntry(ctx, debitInput(fixture.account.ID, "job-lease-debit", 11000, testTime(17, 30)))
	if err != nil || debit.Intent == nil {
		t.Fatalf("debit=%+v err=%v", debit, err)
	}

	claimed, err := env.store.ClaimPaymentJobs(ctx, testTime(17, 30), testTime(17, 29), "worker-a", 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	if err := env.store.CompletePaymentJob(ctx, claimed[0].ID, "wrong-worker"); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("wrong owner completion err=%v", err)
	}
	retryAt := testTime(17, 40)
	if err := env.store.RetryPaymentJob(ctx, claimed[0].ID, "worker-a", retryAt, strings.Repeat("safe", 100)); err != nil {
		t.Fatal(err)
	}
	claimed, err = env.store.ClaimPaymentJobs(ctx, testTime(17, 39), testTime(17, 38), "worker-b", 1)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("early retry claim=%+v err=%v", claimed, err)
	}
	claimed, err = env.store.ClaimPaymentJobs(ctx, retryAt, testTime(17, 39), "worker-b", 1)
	if err != nil || len(claimed) != 1 || claimed[0].AttemptCount != 2 {
		t.Fatalf("retry claim=%+v err=%v", claimed, err)
	}
	if err := env.store.CompletePaymentJob(ctx, claimed[0].ID, "worker-b"); err != nil {
		t.Fatal(err)
	}
	claimed, err = env.store.ClaimPaymentJobs(ctx, retryAt.Add(time.Hour), retryAt, "worker-c", 1)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("completed claim=%+v err=%v", claimed, err)
	}
}

func TestLedgerWorksWithoutAutoTopUpPolicy(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	account, _, err := env.store.EnsureCommercialAccount(ctx, testutil.OrganizationID("payment-no-policy"), payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	for index, in := range []PostLedgerEntryInput{
		{
			AccountID: account.ID, Direction: payment.LedgerDirectionCredit,
			AmountMinor: 10000, Currency: payment.CurrencyTWD, Reason: payment.LedgerReasonManualAdjustmentCredit,
			IdempotencyScope: "fixture", IdempotencyKey: "credit", Now: testTime(18, 0),
		},
		debitInput(account.ID, "no-policy-debit", 11000, testTime(18, 1)),
	} {
		result, err := env.store.PostLedgerEntry(ctx, in)
		if err != nil {
			t.Fatalf("entry %d: %v", index, err)
		}
		if result.Intent != nil {
			t.Fatalf("entry %d unexpectedly created intent", index)
		}
	}
}

func TestStoreRejectsInvalidAccountConsentAndMethodInputs(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	if _, _, err := env.store.EnsureCommercialAccount(ctx, "", payment.CurrencyTWD); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing organization should fail, got %v", err)
	}
	if _, _, err := env.store.EnsureCommercialAccount(ctx, "organization", "USD"); !errors.Is(err, payment.ErrInvalidCurrency) {
		t.Fatalf("unsupported currency should fail, got %v", err)
	}
	if _, err := env.store.CreateConsent(ctx, CreateConsentInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid consent should fail, got %v", err)
	}
	if _, err := env.store.CreatePaymentMethod(ctx, CreatePaymentMethodInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid method should fail, got %v", err)
	}
	if _, err := env.store.CreatePaymentMethod(ctx, CreatePaymentMethodInput{
		AccountID: "account", Provider: "fake", ConsentID: "consent",
		Status: payment.PaymentMethodStatusActive, LastFour: "42ab",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid active method should fail, got %v", err)
	}
}
