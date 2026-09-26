package paypal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

const testRef = "rtk_0123456789abcdef0123456789"
const testOrder = "1AB23456CD789012E"

func testAdapter(t *testing.T, handler http.HandlerFunc) *Adapter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	a, err := New(Config{Environment: "sandbox", ClientID: "client", ClientSecret: "secret", WebhookID: "WH-1", ReturnURL: server.URL + "/v1/payment-returns/paypal", CancelURL: server.URL + "/cancel", EndpointBaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCreateCaptureAndQueryOnlyCreditCompletedMatchingCapture(t *testing.T) {
	var captured atomic.Bool
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth2/token":
			user, pass, ok := r.BasicAuth()
			if !ok || user != "client" || pass != "secret" {
				t.Error("OAuth credentials missing")
			}
			w.Write([]byte(`{"access_token":"token"}`))
		case "/v2/checkout/orders":
			if r.Header.Get("PayPal-Request-Id") != testRef {
				t.Error("create idempotency key missing")
			}
			var request struct {
				PurchaseUnits []purchaseUnit `json:"purchase_units"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.PurchaseUnits) != 1 || !matchesUnit(request.PurchaseUnits[0], testRef, 300, payment.CurrencyTWD) {
				t.Error("create amount or reference mismatch")
			}
			w.Write([]byte(`{"id":"` + testOrder + `","status":"CREATED","purchase_units":[{"custom_id":"` + testRef + `","amount":{"currency_code":"TWD","value":"300"}}],"links":[{"rel":"payer-action","href":"https://www.sandbox.paypal.com/checkoutnow?token=` + testOrder + `"}]}`))
		case "/v2/checkout/orders/" + testOrder:
			status := "APPROVED"
			payments := ""
			if captured.Load() {
				status = "COMPLETED"
				payments = `,"payments":{"captures":[{"id":"CAPTURE-1","status":"COMPLETED","amount":{"currency_code":"TWD","value":"300"}}]}`
			}
			w.Write([]byte(`{"id":"` + testOrder + `","status":"` + status + `","purchase_units":[{"custom_id":"` + testRef + `","amount":{"currency_code":"TWD","value":"300"}` + payments + `}]}`))
		case "/v2/checkout/orders/" + testOrder + "/capture":
			if r.Header.Get("PayPal-Request-Id") != "capture-"+testRef {
				t.Error("capture idempotency key missing")
			}
			captured.Store(true)
			w.Write([]byte(`{"id":"` + testOrder + `","status":"COMPLETED"}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	action, err := a.CreateHostedCharge(ctx, payment.HostedChargeRequest{AmountMinor: 300, Currency: payment.CurrencyTWD, MerchantOrderReference: testRef})
	if err != nil || action.Method != "GET" || action.ProviderTransactionReference != testOrder || !strings.Contains(action.EndpointURL, testOrder) {
		t.Fatalf("action=%+v err=%v", action, err)
	}
	query := payment.QueryRequest{AmountMinor: 300, Currency: payment.CurrencyTWD, MerchantOrderReference: testRef, ProviderTransactionReference: testOrder}
	before, err := a.Query(ctx, query)
	if err != nil || before.State != payment.PaymentIntentStateRequiresAction {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	after, err := a.Capture(ctx, query)
	if err != nil || after.State != payment.PaymentIntentStateSucceeded || after.ProviderTransactionReference != testOrder {
		t.Fatalf("after=%+v err=%v", after, err)
	}
	wrong := query
	wrong.AmountMinor = 301
	if _, err := a.Query(ctx, wrong); err == nil {
		t.Fatal("wrong amount accepted")
	}
}

func TestWebhookRequiresPayPalVerification(t *testing.T) {
	var verified atomic.Bool
	var fetched atomic.Bool
	a := testAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/token" {
			w.Write([]byte(`{"access_token":"token"}`))
			return
		}
		if r.URL.Path == "/v2/checkout/orders/"+testOrder {
			fetched.Store(true)
			w.Write([]byte(`{"id":"` + testOrder + `","status":"COMPLETED","purchase_units":[{"custom_id":"` + testRef + `","amount":{"currency_code":"TWD","value":"300"},"payments":{"captures":[{"id":"CAPTURE-1","status":"COMPLETED","amount":{"currency_code":"TWD","value":"300"}}]}}]}`))
			return
		}
		if r.URL.Path != "/v1/notifications/verify-webhook-signature" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["webhook_id"] != "WH-1" {
			t.Error("verification payload invalid")
		}
		status := "FAILURE"
		if verified.Load() {
			status = "SUCCESS"
		}
		w.Write([]byte(`{"verification_status":"` + status + `"}`))
	})
	request := payment.WebhookRequest{Header: http.Header{}, Body: []byte(`{"id":"WH-EVENT-1","event_type":"PAYMENT.CAPTURE.COMPLETED","resource":{"id":"CAPTURE-1","status":"COMPLETED","amount":{"currency_code":"TWD","value":"300"},"supplementary_data":{"related_ids":{"order_id":"` + testOrder + `"}}}}`)}
	for _, name := range []string{"Paypal-Transmission-Id", "Paypal-Transmission-Time", "Paypal-Cert-Url", "Paypal-Auth-Algo", "Paypal-Transmission-Sig"} {
		request.Header.Set(name, "signed")
	}
	if _, err := a.VerifyWebhook(context.Background(), request); err == nil {
		t.Fatal("unverified capture accepted")
	}
	if fetched.Load() {
		t.Fatal("unverified capture fetched an order")
	}
	verified.Store(true)
	event, err := a.VerifyWebhook(context.Background(), request)
	if err != nil || event.MerchantOrderReference != testRef || event.AmountMinor != 300 || event.State != payment.PaymentIntentStateSucceeded {
		t.Fatalf("event=%+v err=%v", event, err)
	}
}

func TestProductionRejectsEndpointOverride(t *testing.T) {
	_, err := New(Config{Environment: "production", EndpointBaseURL: "http://localhost:1234"})
	if err == nil {
		t.Fatal("production override accepted")
	}
}
