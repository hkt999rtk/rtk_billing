package paymentstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/jackc/pgx/v5"
)

func TestTenantReadSnapshotCannotOutliveOwnershipFence(t *testing.T) {
	env, account, prepare, handoff := newSettlementFixture(t, 1)
	ctx := context.Background()
	claims, err := billingidentity.New(env.db).AuthorizeOwner(ctx, account.OrganizationID, prepare.SourceUserID, prepare.OwnershipVersion)
	if err != nil {
		t.Fatal(err)
	}
	tenant := billingidentity.WithScope(ctx, claims)
	// Model a reader whose PostgreSQL snapshot was taken just before it waited
	// for the account lock. No sleeps or wall-clock races are needed.
	read, err := env.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(ctx)
	if _, err := read.Exec(ctx, `SELECT id FROM commercial_accounts WHERE id=$1`, account.ID); err != nil {
		t.Fatal(err)
	}
	grant := authorizeFixtureHandoff(t, env, prepare, handoff)
	if err := billingidentity.LockAccount(tenant, read, account.ID); !errors.Is(err, billingidentity.ErrSnapshot) {
		t.Fatalf("pre-fence snapshot retained authority: %v", err)
	}
	if _, err := env.store.ListLedgerEntriesPage(tenant, account.ID, 25, 0); !errors.Is(err, billingidentity.ErrTransition) {
		t.Fatalf("fresh read ignored active commit fence: %v", err)
	}
	if _, err := env.store.FinalizeOwnershipHandoff(ctx, finalizeFixtureInput(handoff, prepare.TargetUserID, grant)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ListLedgerEntriesPage(tenant, account.ID, 25, 0); !errors.Is(err, billingidentity.ErrDenied) {
		t.Fatalf("departed request retained history access: %v", err)
	}
}

func TestTenantCannotAdoptUnprovenPolicyOrReplayUnprovenHostedIntent(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	legacy := createPaymentFixture(t, env, "unknown-policy", 10000, 100, 10000)
	hostedInput := CreateHostedTopUpInput{AccountID: legacy.account.ID, Provider: "fake", AmountMinor: 10000, Currency: payment.CurrencyTWD, IdempotencyKey: "old-hosted", CorrelationID: "old-private-reference", Now: testTime(10, 0)}
	if _, err := env.store.CreateHostedTopUp(ctx, hostedInput); err != nil {
		t.Fatal(err)
	}
	// Establish reviewed current ownership only; do not invent historical bindings.
	owner := handoffInput(t, env, legacy.account)
	claims, err := billingidentity.New(env.db).AuthorizeOwner(ctx, owner.OrganizationID, owner.SourceUserID, owner.OwnershipVersion)
	if err != nil {
		t.Fatal(err)
	}
	tenant := billingidentity.WithScope(ctx, claims)
	if replay, err := env.store.CreateHostedTopUp(tenant, hostedInput); !errors.Is(err, ErrIdempotencyConflict) || replay.Intent.ID != "" {
		t.Fatalf("unproven hosted replay=%+v err=%v", replay, err)
	}
	hostedInput.IdempotencyKey, hostedInput.CorrelationID = "current-hosted", "current-reference"
	current, err := env.store.CreateHostedTopUp(tenant, hostedInput)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := env.store.CreateHostedTopUp(tenant, hostedInput); err != nil || !replay.Duplicate || replay.Intent.ID != current.Intent.ID {
		t.Fatalf("current hosted replay=%+v err=%v", replay, err)
	}
	newConsent := func(kind string) payment.PaymentConsent {
		t.Helper()
		consent, err := env.store.CreateConsent(tenant, CreateConsentInput{AccountID: legacy.account.ID, ConsentType: kind, TextVersion: "v2", TextSHA256: strings.Repeat("e", 64), AcceptedActorType: "user", AcceptedActorID: owner.SourceUserID, AcceptedAt: testTime(11, 0), Locale: "zh-TW", Source: "privacy-test"})
		if err != nil {
			t.Fatal(err)
		}
		return consent
	}
	method, err := env.store.CreatePaymentMethod(tenant, CreatePaymentMethodInput{AccountID: legacy.account.ID, Provider: "fake", ProviderCustomerRefCiphertext: []byte("new-customer"), ProviderMethodRefCiphertext: []byte("new-method"), ProviderMethodRefSHA256: strings.Repeat("e", 64), Status: payment.PaymentMethodStatusActive, Capabilities: payment.ProviderCapabilities{MerchantInitiatedCharge: true}, ConsentID: newConsent("payment_method").ID})
	if err != nil {
		t.Fatal(err)
	}
	policyInput := PutAutoTopUpPolicyInput{AccountID: legacy.account.ID, Enabled: true, ThresholdMinor: 100, TopUpAmountMinor: 10000, Currency: payment.CurrencyTWD, PaymentMethodID: method.ID, DailyAttemptLimit: 2, DailyAmountLimitMinor: 20000, CooldownSeconds: 3600, ConsentID: newConsent("auto_topup").ID, ActorID: owner.SourceUserID}
	for _, disabled := range []bool{false, true} {
		if disabled {
			if _, err := env.db.Exec(ctx, `UPDATE auto_topup_policies SET enabled=false,armed=false WHERE account_id=$1`, legacy.account.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := env.store.GetAutoTopUpPolicy(tenant, legacy.account.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown policy visible: %v", err)
		}
		if _, err := env.store.DisableAutoTopUpPolicy(tenant, DisableAutoTopUpPolicyInput{AccountID: legacy.account.ID, ActorID: owner.SourceUserID}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown policy disable exposed: %v", err)
		}
		if _, err := env.store.PutAutoTopUpPolicy(tenant, policyInput); !errors.Is(err, ErrConflict) {
			t.Fatalf("unknown policy adopted (disabled=%t): %v", disabled, err)
		}
	}
	stored, err := env.store.GetAutoTopUpPolicy(ctx, legacy.account.ID)
	if err != nil || stored.PaymentMethodID != legacy.method.ID || stored.Version != legacy.policy.Version {
		t.Fatalf("unknown policy rewritten: %+v %v", stored, err)
	}
}
