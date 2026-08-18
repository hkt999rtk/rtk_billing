package fake

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func TestChargeUsesStableMerchantOrderIdempotency(t *testing.T) {
	provider := New("secret")
	provider.QueueCharge(
		Outcome{Result: payment.ProviderResult{State: payment.PaymentIntentStateSucceeded, ProviderTransactionReference: "txn-1"}},
		Outcome{Result: payment.ProviderResult{State: payment.PaymentIntentStateFailed}},
	)
	request := payment.ChargeRequest{MerchantOrderReference: "order-1"}
	first, err := provider.Charge(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Charge(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != payment.PaymentIntentStateSucceeded || second.ProviderTransactionReference != "txn-1" {
		t.Fatalf("idempotent results first=%+v second=%+v", first, second)
	}
	if len(provider.ChargeCalls()) != 2 {
		t.Fatalf("charge calls=%d", len(provider.ChargeCalls()))
	}
}

func TestWebhookHMACAndStrictPayload(t *testing.T) {
	provider := New("secret")
	event := payment.WebhookEvent{
		ProviderEventReference: "event-1", MerchantOrderReference: "order-1",
		ProviderTransactionReference: "txn-1", AmountMinor: 50000,
		Currency: payment.CurrencyTWD, State: payment.PaymentIntentStateSucceeded,
		EventType: "payment.succeeded", ProviderCode: "00",
	}
	body, err := WebhookBody(event)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{"X-Fake-Signature": []string{SignWebhook("secret", body)}}
	verified, err := provider.VerifyWebhook(context.Background(), payment.WebhookRequest{Header: header, Body: body})
	if err != nil || verified.ProviderEventReference != event.ProviderEventReference {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
	if _, err := provider.VerifyWebhook(context.Background(), payment.WebhookRequest{Header: http.Header{"X-Fake-Signature": []string{"00"}}, Body: body}); err == nil {
		t.Fatal("invalid signature should fail")
	}
	malformed := append(body[:len(body)-1], []byte(`,"unexpected":true}`)...)
	malformedHeader := http.Header{"X-Fake-Signature": []string{SignWebhook("secret", malformed)}}
	_, err = provider.VerifyWebhook(context.Background(), payment.WebhookRequest{Header: malformedHeader, Body: malformed})
	var providerErr *payment.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Kind != payment.ProviderErrorInvalidRequest {
		t.Fatalf("malformed payload error=%v", err)
	}
}

func TestSetupUsesStableIdempotencyWithoutCardData(t *testing.T) {
	provider := New("secret")
	provider.QueueSetup(SetupOutcome{Result: payment.SetupResult{
		State: payment.PaymentIntentStateSucceeded, HostedURL: "https://fake-payments.invalid/setup/session-1",
		ProviderCustomerRef: "customer-opaque-1", ProviderMethodRef: "method-opaque-1", ProviderCode: "00",
	}}, SetupOutcome{Result: payment.SetupResult{
		State: payment.PaymentIntentStateSucceeded, HostedURL: "https://fake-payments.invalid/setup/session-2",
		ProviderCustomerRef: "customer-opaque-2", ProviderMethodRef: "method-opaque-2", ProviderCode: "00",
	}})
	request := payment.SetupRequest{AccountID: "account-1", IdempotencyKey: "setup-1", CorrelationID: "correlation-1"}
	first, err := provider.CreateSetup(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.CreateSetup(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ProviderMethodRef != "method-opaque-1" || second.HostedURL != first.HostedURL || len(provider.SetupCalls()) != 2 {
		t.Fatalf("first=%+v second=%+v calls=%d", first, second, len(provider.SetupCalls()))
	}
	otherTenant := request
	otherTenant.AccountID = "account-2"
	third, err := provider.CreateSetup(context.Background(), otherTenant)
	if err != nil {
		t.Fatal(err)
	}
	if third.ProviderMethodRef != "method-opaque-2" || third.HostedURL == first.HostedURL {
		t.Fatalf("tenant-scoped setup idempotency first=%+v third=%+v", first, third)
	}
}

func TestUnsupportedFakeProviderRefundIsExplicit(t *testing.T) {
	provider := New("secret")
	if _, err := provider.Refund(context.Background(), payment.RefundRequest{}); !errors.Is(err, payment.ErrProviderUnsupported) {
		t.Fatalf("refund err=%v", err)
	}
	capabilities := provider.Capabilities(context.Background())
	if !capabilities.HostedSetup || !capabilities.MerchantInitiatedCharge || capabilities.Refund {
		t.Fatalf("unexpected fake capabilities: %+v", capabilities)
	}
}
