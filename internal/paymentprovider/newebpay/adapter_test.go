package newebpay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

func enabledConfig(client HTTPDoer) Config {
	return Config{
		Enabled: true, Environment: EnvironmentSandbox, MerchantID: "MS127874575",
		HashKey: officialHashKey, HashIV: officialHashIV, HTTPClient: client, Timeout: time.Second,
	}
}

func TestAdapterDisabledByDefaultAndRejectsInvalidConfiguration(t *testing.T) {
	adapter, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if capabilities := adapter.Capabilities(context.Background()); capabilities.StatusQuery || capabilities.MerchantInitiatedCharge {
		t.Fatalf("disabled capabilities=%+v", capabilities)
	}
	if _, err := adapter.Query(context.Background(), payment.QueryRequest{}); err == nil {
		t.Fatal("disabled query should fail")
	}
	for _, config := range []Config{
		{Enabled: true, Environment: "unknown"},
		{Enabled: true, Environment: EnvironmentSandbox, MerchantID: "merchant", HashKey: "short", HashIV: "short"},
		{Enabled: true, Environment: EnvironmentSandbox, MerchantID: strings.Repeat("m", 16), HashKey: officialHashKey, HashIV: officialHashIV},
	} {
		_, err := New(config)
		if err == nil || config.HashKey != "" && strings.Contains(err.Error(), config.HashKey) || config.HashIV != "" && strings.Contains(err.Error(), config.HashIV) {
			t.Fatalf("unsafe configuration error=%v", err)
		}
	}
}

func TestAdapterAdvertisesOnlyImplementedApprovedCapabilities(t *testing.T) {
	adapter, err := New(enabledConfig(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})))
	if err != nil {
		t.Fatal(err)
	}
	capabilities := adapter.Capabilities(context.Background())
	if !capabilities.StatusQuery || !capabilities.Webhook || capabilities.MerchantInitiatedCharge || capabilities.VaultedMethod {
		t.Fatalf("capabilities=%+v", capabilities)
	}
	if _, err := adapter.Charge(context.Background(), payment.ChargeRequest{}); !errors.Is(err, payment.ErrProviderUnsupported) {
		t.Fatalf("charge err=%v", err)
	}
	if _, err := adapter.CreateSetup(context.Background(), payment.SetupRequest{}); !errors.Is(err, payment.ErrProviderUnsupported) {
		t.Fatalf("setup err=%v", err)
	}
	if _, err := adapter.Refund(context.Background(), payment.RefundRequest{}); !errors.Is(err, payment.ErrProviderUnsupported) {
		t.Fatalf("refund err=%v", err)
	}
	if adapter.mpgURL() != sandboxMPGURL || adapter.Name() != "newebpay" {
		t.Fatalf("adapter endpoint=%q name=%q", adapter.mpgURL(), adapter.Name())
	}
}

func TestQueryUsesFixedSandboxEndpointAndVerifiesResponse(t *testing.T) {
	requestTime := time.Date(2026, 8, 15, 10, 30, 0, 0, time.UTC)
	orderReference := "rtk_01234567890123456789012345"
	tradeNumber := "26081510300000001"
	checkCode := ResponseCheckCode(map[string]string{
		"Amt": "30", "MerchantID": "MS127874575",
		"MerchantOrderNo": orderReference, "TradeNo": tradeNumber,
	}, officialHashKey, officialHashIV)
	client := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != sandboxQueryURL || request.Method != http.MethodPost {
			t.Fatalf("request=%s %s", request.Method, request.URL)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		if form.Get("MerchantID") != "MS127874575" || form.Get("Version") != "1.3" ||
			form.Get("RespondType") != "JSON" || form.Get("MerchantOrderNo") != orderReference ||
			form.Get("Amt") != "30" || form.Get("TimeStamp") != strconv.FormatInt(requestTime.Unix(), 10) ||
			form.Get("CheckValue") != QueryCheckValue(30, "MS127874575", orderReference, officialHashKey, officialHashIV) {
			t.Fatalf("form=%v", form)
		}
		response := `{"Status":"SUCCESS","Message":"ok","Result":{"MerchantID":"MS127874575","Amt":30,"TradeNo":"` + tradeNumber + `","MerchantOrderNo":"` + orderReference + `","TradeStatus":"1","CheckCode":"` + checkCode + `"}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	adapter, err := New(enabledConfig(client))
	if err != nil {
		t.Fatal(err)
	}
	adapter.now = func() time.Time { return requestTime }
	result, err := adapter.Query(context.Background(), payment.QueryRequest{
		IntentID: "intent-1", AmountMinor: 30, Currency: payment.CurrencyTWD,
		MerchantOrderReference: orderReference,
	})
	if err != nil || result.State != payment.PaymentIntentStateSucceeded || result.ProviderTransactionReference != tradeNumber {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestQueryRejectsMismatchedIntegrityAndUnsafeInputs(t *testing.T) {
	client := roundTripFunc(func(*http.Request) (*http.Response, error) {
		response := `{"Status":"SUCCESS","Result":{"MerchantID":"MS127874575","Amt":"30","TradeNo":"txn","MerchantOrderNo":"order_1","TradeStatus":"1","CheckCode":"` + strings.Repeat("0", 64) + `"}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	adapter, err := New(enabledConfig(client))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Query(context.Background(), payment.QueryRequest{
		AmountMinor: 30, Currency: payment.CurrencyTWD, MerchantOrderReference: "order_1",
	})
	var providerErr *payment.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Kind != payment.ProviderErrorAuthentication {
		t.Fatalf("integrity err=%v", err)
	}
	for _, request := range []payment.QueryRequest{
		{AmountMinor: 0, Currency: payment.CurrencyTWD, MerchantOrderReference: "order_1"},
		{AmountMinor: 30, Currency: "USD", MerchantOrderReference: "order_1"},
		{AmountMinor: 30, Currency: payment.CurrencyTWD, MerchantOrderReference: "order-with-dash"},
	} {
		if _, err := adapter.Query(context.Background(), request); err == nil {
			t.Fatalf("request=%+v should fail", request)
		}
	}
}

func TestQueryMapsTradeStatesAndTransportFailures(t *testing.T) {
	for tradeStatus, want := range map[string]payment.PaymentIntentState{
		"0": payment.PaymentIntentStateUnknown,
		"2": payment.PaymentIntentStateFailed,
		"3": payment.PaymentIntentStateCanceled,
		"6": payment.PaymentIntentStateCanceled,
	} {
		t.Run(tradeStatus, func(t *testing.T) {
			orderReference := "order_" + tradeStatus
			tradeNumber := "trade" + tradeStatus
			checkCode := ResponseCheckCode(map[string]string{
				"Amt": "30", "MerchantID": "MS127874575", "MerchantOrderNo": orderReference, "TradeNo": tradeNumber,
			}, officialHashKey, officialHashIV)
			client := roundTripFunc(func(*http.Request) (*http.Response, error) {
				response := `{"Status":"SUCCESS","Result":{"MerchantID":"MS127874575","Amt":30,"TradeNo":"` + tradeNumber + `","MerchantOrderNo":"` + orderReference + `","TradeStatus":"` + tradeStatus + `","CheckCode":"` + checkCode + `"}}`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
			})
			adapter, err := New(enabledConfig(client))
			if err != nil {
				t.Fatal(err)
			}
			result, err := adapter.Query(context.Background(), payment.QueryRequest{
				AmountMinor: 30, Currency: payment.CurrencyTWD, MerchantOrderReference: orderReference,
			})
			if err != nil || result.State != want {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}

	adapter, err := New(enabledConfig(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Query(context.Background(), payment.QueryRequest{
		AmountMinor: 30, Currency: payment.CurrencyTWD, MerchantOrderReference: "order_transport",
	})
	var providerErr *payment.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Kind != payment.ProviderErrorTemporary {
		t.Fatalf("transport err=%v", err)
	}
}
