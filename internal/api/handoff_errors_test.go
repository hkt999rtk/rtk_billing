package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestHandoffPaymentErrorsAreExplicitConflicts(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{paymentstore.ErrHandoffFenced, "BILLING_OWNERSHIP_HANDOFF_FENCED"},
		{paymentstore.ErrSetupInvalidated, "PAYMENT_SETUP_INVALIDATED"},
	} {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		writePaymentSetupError(c, test.err)
		if r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), test.code) {
			t.Fatalf("error=%v status=%d body=%s", test.err, r.Code, r.Body.String())
		}
	}
}

func TestHandoffInvalidatedCallbackIsAuthenticatedAndAcknowledged(t *testing.T) {
	const secret = "handoff-test-simulator-callback-secret-0001"
	env := newIntegrationAPIWithOptions(t, func(options *PaymentAPIOptions) { options.SimulatorCallbackSecret = secret })
	store := paymentstore.New(env.db)
	ctx := context.Background()
	account, _, err := store.EnsureCommercialAccount(ctx, testutil.OrganizationID("handoff-callback"), payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	owner := testutil.OrganizationID("callback-owner")
	now := time.Now().UTC()
	if err := store.InitializeResponsibility(ctx, paymentstore.InitialResponsibilityInput{
		AccountID: account.ID, OwnerUserID: owner, OwnershipVersion: 1, EffectiveFrom: now.Add(-time.Hour), SourceEvidenceSHA256: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
	setup, err := store.BeginPaymentMethodSetup(ctx, paymentstore.BeginPaymentMethodSetupInput{
		AccountID: account.ID, Provider: "simulator", IdempotencyKey: "callback-setup", RequestSHA256: strings.Repeat("b", 64), CorrelationID: "test-setup",
		Consent: paymentstore.CreateConsentInput{
			AccountID: account.ID, ConsentType: "payment_method", TextVersion: "v1", TextSHA256: strings.Repeat("c", 64),
			AcceptedActorType: "user", AcceptedActorID: owner, Locale: "zh-TW", Source: "test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareOwnershipHandoff(ctx, paymentstore.PrepareOwnershipHandoffInput{
		OperationID: testutil.OrganizationID("callback-operation"), OrganizationID: account.OrganizationID,
		SourceUserID: owner, TargetUserID: testutil.OrganizationID("callback-target"), OwnershipVersion: 1, Cutoff: now,
	}); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(simulatorSetupCallback{
		AccountID: account.ID, SetupSessionID: setup.Session.ID, State: payment.PaymentIntentStateSucceeded,
		ProviderCode: "ok", HostedURL: "https://simulator.invalid/setup",
		ProviderCustomerRef: "old-customer-sensitive", ProviderMethodRef: "old-method-sensitive", LastFour: "4242",
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	for i, signature := range []string{"invalid", hex.EncodeToString(mac.Sum(nil)), hex.EncodeToString(mac.Sum(nil))} {
		r := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/internal/payment-simulator/setup-callback", bytes.NewReader(body))
		req.Header.Set("X-Payment-Simulator-Signature", signature)
		env.server.Router().ServeHTTP(r, req)
		if i == 0 {
			if r.Code != http.StatusUnauthorized {
				t.Fatalf("unsigned callback status=%d", r.Code)
			}
			continue
		}
		var result struct{ Accepted, Duplicate bool }
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil || r.Code != http.StatusOK || !result.Accepted || result.Duplicate != (i == 2) {
			t.Fatalf("callback %d status=%d body=%s err=%v", i, r.Code, r.Body.String(), err)
		}
		if strings.Contains(r.Body.String(), "sensitive") || strings.Contains(r.Body.String(), "4242") {
			t.Fatal("provider details leaked into acknowledgement")
		}
	}
	method, err := store.GetPaymentMethod(ctx, account.ID, setup.Method.ID)
	if err != nil || method.Status != payment.PaymentMethodStatusRevoked {
		t.Fatalf("late callback restored method: %+v err=%v", method, err)
	}
}
