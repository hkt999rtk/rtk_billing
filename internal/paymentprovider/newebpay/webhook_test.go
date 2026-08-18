package newebpay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func buildWebhookBody(t *testing.T, status string, mutate func(url.Values)) []byte {
	t.Helper()
	fields := url.Values{
		"Status":          []string{status},
		"MerchantID":      []string{"MS127874575"},
		"Amt":             []string{"500"},
		"TradeNo":         []string{"26081512000000001"},
		"MerchantOrderNo": []string{"rtk_01234567890123456789012345"},
		"RespondCode":     []string{"00"},
		"PaymentType":     []string{"CREDIT"},
	}
	if mutate != nil {
		mutate(fields)
	}
	tradeInfo, err := EncryptTradeInfo(fields.Encode(), officialHashKey, officialHashIV)
	if err != nil {
		t.Fatal(err)
	}
	outer := url.Values{
		"Status":     []string{status},
		"MerchantID": []string{"MS127874575"},
		"Version":    []string{"2.3"},
		"TradeInfo":  []string{tradeInfo},
		"TradeSha":   []string{TradeSHA(tradeInfo, officialHashKey, officialHashIV)},
	}
	return []byte(outer.Encode())
}

func TestVerifyWebhookAuthenticatesAndNormalizesOnlySafeFields(t *testing.T) {
	adapter, err := New(enabledConfig(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })))
	if err != nil {
		t.Fatal(err)
	}
	body := buildWebhookBody(t, "SUCCESS", nil)
	event, err := adapter.VerifyWebhook(context.Background(), payment.WebhookRequest{Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if event.State != payment.PaymentIntentStateSucceeded || event.AmountMinor != 500 ||
		event.Currency != payment.CurrencyTWD || event.ProviderTransactionReference != "26081512000000001" ||
		event.SafeSummary["payment_type"] != "CREDIT" {
		t.Fatalf("event=%+v", event)
	}
	if strings.Contains(strings.Join(mapValues(event.SafeSummary), " "), officialHashKey) {
		t.Fatal("safe summary contains a secret")
	}
}

func TestVerifyWebhookRejectsTamperMerchantAndStatusMismatch(t *testing.T) {
	adapter, err := New(enabledConfig(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })))
	if err != nil {
		t.Fatal(err)
	}
	tampered, _ := url.ParseQuery(string(buildWebhookBody(t, "SUCCESS", nil)))
	tampered.Set("TradeSha", strings.Repeat("0", 64))
	_, err = adapter.VerifyWebhook(context.Background(), payment.WebhookRequest{Body: []byte(tampered.Encode())})
	var providerErr *payment.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Kind != payment.ProviderErrorAuthentication {
		t.Fatalf("tamper err=%v", err)
	}

	mismatch := buildWebhookBody(t, "SUCCESS", func(fields url.Values) { fields.Set("Status", "MPG03009") })
	if _, err := adapter.VerifyWebhook(context.Background(), payment.WebhookRequest{Body: mismatch}); err == nil {
		t.Fatal("inner/outer status mismatch should fail")
	}
	merchantMismatch := buildWebhookBody(t, "SUCCESS", func(fields url.Values) { fields.Set("MerchantID", "OTHER") })
	if _, err := adapter.VerifyWebhook(context.Background(), payment.WebhookRequest{Body: merchantMismatch}); err == nil {
		t.Fatal("merchant mismatch should fail")
	}
}

func TestVerifyWebhookMapsFailureWithoutLeakingMessage(t *testing.T) {
	adapter, err := New(enabledConfig(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })))
	if err != nil {
		t.Fatal(err)
	}
	body := buildWebhookBody(t, "MPG03009", func(fields url.Values) {
		fields.Set("RespondCode", "declined")
		fields.Set("Message", "sensitive provider message")
	})
	event, err := adapter.VerifyWebhook(context.Background(), payment.WebhookRequest{Body: body})
	if err != nil || event.State != payment.PaymentIntentStateFailed || event.ProviderCode != "declined" {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	if strings.Contains(strings.Join(mapValues(event.SafeSummary), " "), "sensitive") {
		t.Fatal("provider message must not enter safe summary")
	}
}

func TestVerifyWebhookAcceptsOfficialJSONRespondType(t *testing.T) {
	adapter, err := New(enabledConfig(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })))
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"Status":  "SUCCESS",
		"Message": "authorization succeeded",
		"Result": map[string]any{
			"MerchantID": "MS127874575", "Amt": 500,
			"TradeNo": "26081512000000002", "MerchantOrderNo": "rtk_01234567890123456789012345",
			"RespondCode": "00", "PaymentType": "CREDIT",
		},
	}
	plaintext, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	tradeInfo, err := EncryptTradeInfo(string(plaintext), officialHashKey, officialHashIV)
	if err != nil {
		t.Fatal(err)
	}
	outer := url.Values{
		"Status": []string{"SUCCESS"}, "MerchantID": []string{"MS127874575"},
		"Version": []string{"2.3"}, "TradeInfo": []string{tradeInfo},
		"TradeSha": []string{TradeSHA(tradeInfo, officialHashKey, officialHashIV)},
	}
	event, err := adapter.VerifyWebhook(context.Background(), payment.WebhookRequest{Body: []byte(outer.Encode())})
	if err != nil || event.State != payment.PaymentIntentStateSucceeded || event.AmountMinor != 500 || event.ProviderTransactionReference != "26081512000000002" {
		t.Fatalf("event=%+v err=%v", event, err)
	}
}

func mapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}
