package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentcrypto"
	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/newebpay"
	"github.com/hkt999rtk/rtk_billing/internal/paymentservice"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestIntegrationNewebPayHostedWebhookQueryCreditsExactlyOnce(t *testing.T) {
	const (
		merchantID = "MS123456789"
		hashKey    = "12345678901234567890123456789012"
		hashIV     = "1234567890123456"
		tradeNo    = "26082912000000001"
	)
	queryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/API/QueryTradeInfo" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		amount, err := strconv.ParseInt(r.Form.Get("Amt"), 10, 64)
		if err != nil || r.Form.Get("MerchantID") != merchantID || r.Form.Get("CheckValue") != newebpay.QueryCheckValue(amount, merchantID, r.Form.Get("MerchantOrderNo"), hashKey, hashIV) {
			t.Fatalf("invalid query form: %v", r.Form)
		}
		order := r.Form.Get("MerchantOrderNo")
		checkCode := newebpay.ResponseCheckCode(map[string]string{
			"Amt": strconv.FormatInt(amount, 10), "MerchantID": merchantID,
			"MerchantOrderNo": order, "TradeNo": tradeNo,
		}, hashKey, hashIV)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"Status":"SUCCESS","Message":"ok","Result":{"MerchantID":%q,"Amt":%d,"TradeNo":%q,"MerchantOrderNo":%q,"TradeStatus":"1","CheckCode":%q}}`, merchantID, amount, tradeNo, order, checkCode)
	}))
	t.Cleanup(queryServer.Close)

	provider, err := newebpay.New(newebpay.Config{
		Enabled: true, Environment: newebpay.EnvironmentSandbox,
		MerchantID: merchantID, HashKey: hashKey, HashIV: hashIV,
		EndpointBaseURL: queryServer.URL, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	env := newIntegrationAPIWithOptions(t, func(options *PaymentAPIOptions) {
		options.HostedChargeNotifyURL = "https://billing.test/v1/payment-webhooks/newebpay"
		options.HostedChargeReturnURL = "https://admin.test/console/billing/activity"
	}, provider)
	organizationID := testutil.OrganizationID("newebpay-hosted-credit")
	env.provisionOwner(t, organizationID)
	base := "/v1/orgs/" + organizationID
	if response := env.request(t, http.MethodGet, base+"/billing/account", "billing_account.read", "", nil); response.Code != http.StatusOK {
		t.Fatalf("create account status=%d body=%s", response.Code, response.Body.String())
	}

	checkout := env.request(t, http.MethodPost, base+"/topups/checkout", "payment_intent.create", "newebpay-hosted-1", map[string]any{
		"amount_minor": 500, "currency": "TWD", "provider": "newebpay",
	})
	if checkout.Code != http.StatusAccepted {
		t.Fatalf("checkout status=%d body=%s", checkout.Code, checkout.Body.String())
	}
	var created struct {
		PaymentIntent struct {
			ID string `json:"id"`
		} `json:"payment_intent"`
		PaymentAction struct {
			Fields map[string]string `json:"fields"`
		} `json:"payment_action"`
	}
	if err := json.Unmarshal(checkout.Body.Bytes(), &created); err != nil || created.PaymentIntent.ID == "" {
		t.Fatalf("checkout response=%s err=%v", checkout.Body.String(), err)
	}
	plain, err := newebpay.DecryptTradeInfo(created.PaymentAction.Fields["TradeInfo"], hashKey, hashIV)
	if err != nil {
		t.Fatal(err)
	}
	hostedFields, err := url.ParseQuery(plain)
	if err != nil {
		t.Fatal(err)
	}
	order := hostedFields.Get("MerchantOrderNo")
	if order == "" || hostedFields.Get("NotifyURL") != "https://billing.test/v1/payment-webhooks/newebpay" {
		t.Fatalf("hosted fields=%v", hostedFields)
	}

	inner := url.Values{
		"Status": {"SUCCESS"}, "MerchantID": {merchantID}, "Amt": {"500"},
		"TradeNo": {tradeNo}, "MerchantOrderNo": {order}, "RespondCode": {"00"}, "PaymentType": {"CREDIT"},
	}
	tradeInfo, err := newebpay.EncryptTradeInfo(inner.Encode(), hashKey, hashIV)
	if err != nil {
		t.Fatal(err)
	}
	webhookBody := url.Values{
		"Status": {"SUCCESS"}, "MerchantID": {merchantID}, "Version": {"2.3"},
		"TradeInfo": {tradeInfo}, "TradeSha": {newebpay.TradeSHA(tradeInfo, hashKey, hashIV)},
	}.Encode()
	webhook := httptest.NewRequest(http.MethodPost, "/v1/payment-webhooks/newebpay", strings.NewReader(webhookBody))
	webhook.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	webhookResponse := httptest.NewRecorder()
	env.server.Router().ServeHTTP(webhookResponse, webhook)
	if webhookResponse.Code != http.StatusOK || !strings.Contains(webhookResponse.Body.String(), `"accepted":true`) {
		t.Fatalf("webhook status=%d body=%s", webhookResponse.Code, webhookResponse.Body.String())
	}

	resolver, err := paymentcrypto.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := paymentservice.New(paymentservice.Options{
		Store: paymentstore.New(env.db), Providers: []payment.PaymentProvider{provider}, ReferenceResolver: resolver,
		LeaseOwner: "newebpay-integration-worker", LeaseDuration: time.Minute, ReconciliationDelay: time.Minute,
		BatchSize: 10, ChargeEnabled: map[string]bool{"newebpay": false},
		Now: func() time.Time { return time.Date(2026, 8, 17, 9, 30, 1, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.RunOnce(context.Background()); err != nil || processed == 0 {
		t.Fatalf("worker processed=%d err=%v", processed, err)
	}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	intent, err := paymentstore.New(env.db).GetPaymentIntent(context.Background(), created.PaymentIntent.ID)
	if err != nil || intent.State != payment.PaymentIntentStateSucceeded || intent.ProviderTransactionReference != tradeNo {
		t.Fatalf("intent=%+v err=%v", intent, err)
	}
	var balance int64
	var credits int
	if err := env.db.QueryRow(context.Background(), `SELECT available_balance_minor FROM commercial_accounts WHERE organization_id=$1`, organizationID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger_entries WHERE idempotency_scope='payment_intent' AND idempotency_key=$1`, created.PaymentIntent.ID).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if balance != 500 || credits != 1 {
		t.Fatalf("balance=%d credits=%d", balance, credits)
	}
}
