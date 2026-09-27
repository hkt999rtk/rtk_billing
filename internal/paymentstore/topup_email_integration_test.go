package paymentstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestPayPalTopUpCreditsAndQueuesOneBillingEmail(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	organizationID := testutil.OrganizationID("paypal-topup-email")
	account, _, err := env.store.EnsureCommercialAccount(ctx, organizationID, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.InitializeResponsibility(ctx, InitialResponsibilityInput{
		AccountID: account.ID, OwnerUserID: testutil.OrganizationID("paypal-topup-owner"),
		OwnershipVersion: 1, EffectiveFrom: testTime(8, 0), SourceEvidenceSHA256: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO billing_profiles
		(organization_id, legal_name, contact_email, delivery_preference, ownership_version, requires_configuration)
		VALUES ($1, 'RTK Test', 'billing@example.test', 'portal_and_email', 1, false)`, organizationID); err != nil {
		t.Fatal(err)
	}
	created, err := env.store.CreateHostedTopUp(ctx, CreateHostedTopUpInput{
		AccountID: account.ID, Provider: "paypal", AmountMinor: 12800, Currency: payment.CurrencyTWD,
		IdempotencyKey: "paypal-email-1", CorrelationID: "paypal-email-request", Now: testTime(15, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := testTime(15, 1)
	transition := TransitionIntentInput{IntentID: created.Intent.ID, ToState: payment.PaymentIntentStateSucceeded,
		ProviderTransactionReference: "TESTPAYPALORDER123", Now: completed}
	result, err := env.store.TransitionIntent(ctx, transition)
	if err != nil || result.CreditEntry == nil || result.Account.AvailableBalanceMinor != 12800 {
		t.Fatalf("credit result=%+v err=%v", result, err)
	}
	duplicate, err := env.store.TransitionIntent(ctx, transition)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate result=%+v err=%v", duplicate, err)
	}
	messages, err := env.store.ClaimTopUpEmails(ctx, completed.Add(time.Second), 10)
	if err != nil || len(messages) != 1 || messages[0].IntentID != created.Intent.ID ||
		messages[0].RecipientEmail != "billing@example.test" || messages[0].AmountMinor != 12800 ||
		!messages[0].PaymentCompletedAt.Equal(completed) {
		t.Fatalf("claimed=%+v err=%v", messages, err)
	}
	if ok, err := env.store.RetryTopUpEmail(ctx, messages[0].IntentID, messages[0].AttemptCount, completed.Add(time.Second)); err != nil || !ok {
		t.Fatalf("retry ok=%v err=%v", ok, err)
	}
	messages, err = env.store.ClaimTopUpEmails(ctx, completed.Add(32*time.Second), 10)
	if err != nil || len(messages) != 1 || messages[0].AttemptCount != 2 {
		t.Fatalf("reclaimed=%+v err=%v", messages, err)
	}
	if ok, err := env.store.MarkTopUpEmailSent(ctx, messages[0].IntentID, messages[0].AttemptCount, completed.Add(33*time.Second)); err != nil || !ok {
		t.Fatalf("sent ok=%v err=%v", ok, err)
	}
	messages, err = env.store.ClaimTopUpEmails(ctx, completed.Add(time.Hour), 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("sent email reclaimed=%+v err=%v", messages, err)
	}
}
