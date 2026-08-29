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
	if !capabilities.HostedCharge || !capabilities.StatusQuery || !capabilities.Webhook || capabilities.MerchantInitiatedCharge || capabilities.VaultedMethod {
		t.Fatalf("capabilities=%+v", capabilities)
	}
	if _, err := adapter.Charge(context.Background(), payment.ChargeRequest{}); !errors.Is(err, payment.ErrProviderUnsupported) {
		t.Fatalf("charge err=%v", err)
	}
	if _, err := adapter.CreateSetup(context.Background(), payment.SetupRequest{}); !errors.Is(err, payment.ErrProviderUnsupported) {
		t.Fatalf("setup err=%v", err)
	}
	if adapter.mpgURL() != sandboxMPGURL || adapter.Name() != "newebpay" {
		t.Fatalf("adapter endpoint=%q name=%q", adapter.mpgURL(), adapter.Name())
	}
}

func TestRefundUsesEncryptedCloseAPIAndValidatesBoundResponse(t *testing.T) {
	const tradeNo = "26081510300000001"
	client := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != sandboxCloseURL || request.Method != http.MethodPost {
			t.Fatalf("request=%s %s", request.Method, request.URL)
		}
		if err := request.ParseForm(); err != nil || request.Form.Get("MerchantID_") != "MS127874575" {
			t.Fatalf("form=%v err=%v", request.Form, err)
		}
		plain, err := DecryptTradeInfo(request.Form.Get("PostData_"), officialHashKey, officialHashIV)
		if err != nil {
			t.Fatal(err)
		}
		fields, _ := url.ParseQuery(plain)
		if fields.Get("Version") != "1.1" || fields.Get("IndexType") != "2" || fields.Get("CloseType") != "2" || fields.Get("TradeNo") != tradeNo || fields.Get("Amt") != "300" {
			t.Fatalf("fields=%v", fields)
		}
		body := `{"Status":"SUCCESS","Message":"success","Result":{"MerchantID":"MS127874575","Amt":300,"TradeNo":"` + tradeNo + `"}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	adapter, err := New(enabledConfig(client))
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Refund(context.Background(), payment.RefundRequest{AmountMinor: 300, Currency: payment.CurrencyTWD, ProviderTransactionReference: tradeNo})
	if err != nil || result.State != payment.PaymentIntentStateSucceeded || result.ProviderTransactionReference != tradeNo {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestHostedChargeBuildsEncryptedMPGFormWithoutCardData(t *testing.T) {
	adapter, err := New(enabledConfig(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") })))
	if err != nil {
		t.Fatal(err)
	}
	adapter.now = func() time.Time { return time.Unix(1_777_777_777, 0).UTC() }
	result, err := adapter.CreateHostedCharge(context.Background(), payment.HostedChargeRequest{
		IntentID: "intent-1", AmountMinor: 300, Currency: payment.CurrencyTWD,
		MerchantOrderReference: "rtk_01234567890123456789012345", NotifyURL: "https://billing.example/v1/payment-webhooks/newebpay",
		ReturnURL: "https://admin.example/console/billing/activity", ItemDescription: "RTK Cloud top-up",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EndpointURL != sandboxMPGURL || result.Fields["MerchantID"] != "MS127874575" || result.Fields["Version"] != "2.3" || !VerifyDigest(TradeSHA(result.Fields["TradeInfo"], officialHashKey, officialHashIV), result.Fields["TradeSha"]) {
		t.Fatalf("action=%+v", result)
	}
	plain, err := DecryptTradeInfo(result.Fields["TradeInfo"], officialHashKey, officialHashIV)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := url.ParseQuery(plain)
	if err != nil {
		t.Fatal(err)
	}
	if fields.Get("Amt") != "300" || fields.Get("CREDIT") != "1" || fields.Get("MerchantOrderNo") != "rtk_01234567890123456789012345" || fields.Get("NotifyURL") != "https://billing.example/v1/payment-webhooks/newebpay" || fields.Get("ReturnURL") != "https://admin.example/console/billing/activity" {
		t.Fatalf("fields=%v", fields)
	}
	for _, forbidden := range []string{"CardNo", "CVV", "CVC"} {
		if fields.Get(forbidden) != "" {
			t.Fatalf("hosted action contained %s", forbidden)
		}
	}
}

func TestHostedChargeRejectsInvalidInputsAndTruncatesDescription(t *testing.T) {
	disabled, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disabled.CreateHostedCharge(context.Background(), payment.HostedChargeRequest{}); !errors.Is(err, payment.ErrProviderUnsupported) {
		t.Fatalf("disabled hosted charge err=%v", err)
	}

	adapter, err := New(enabledConfig(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") })))
	if err != nil {
		t.Fatal(err)
	}
	valid := payment.HostedChargeRequest{
		AmountMinor: 300, Currency: payment.CurrencyTWD, MerchantOrderReference: "order_1",
		NotifyURL: "https://billing.example/callback", ReturnURL: "https://admin.example/activity", ItemDescription: "top up",
	}
	invalid := []payment.HostedChargeRequest{
		func() payment.HostedChargeRequest {
			value := valid
			value.MerchantOrderReference = "order-with-dash"
			return value
		}(),
		func() payment.HostedChargeRequest { value := valid; value.AmountMinor = 0; return value }(),
		func() payment.HostedChargeRequest {
			value := valid
			value.NotifyURL = "javascript:alert(1)"
			return value
		}(),
		func() payment.HostedChargeRequest { value := valid; value.ItemDescription = " "; return value }(),
	}
	for _, request := range invalid {
		if _, err := adapter.CreateHostedCharge(context.Background(), request); err == nil {
			t.Fatalf("invalid hosted request passed: %+v", request)
		}
	}

	valid.ItemDescription = strings.Repeat("界", 51)
	result, err := adapter.CreateHostedCharge(context.Background(), valid)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := DecryptTradeInfo(result.Fields["TradeInfo"], officialHashKey, officialHashIV)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := url.ParseQuery(plain)
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(fields.Get("ItemDesc"))); got != 50 {
		t.Fatalf("description runes=%d, want 50", got)
	}
}

func TestRefundRejectsInvalidInputsAndMapsProviderFailures(t *testing.T) {
	disabled, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	request := payment.RefundRequest{AmountMinor: 300, Currency: payment.CurrencyTWD, ProviderTransactionReference: "trade1"}
	if _, err := disabled.Refund(context.Background(), request); !errors.Is(err, payment.ErrProviderUnsupported) {
		t.Fatalf("disabled refund err=%v", err)
	}

	adapter, err := New(enabledConfig(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})))
	if err != nil {
		t.Fatal(err)
	}
	invalid := request
	invalid.ProviderTransactionReference = ""
	if _, err := adapter.Refund(context.Background(), invalid); err == nil {
		t.Fatal("invalid refund passed")
	}
	if _, err := adapter.Refund(context.Background(), request); err == nil {
		t.Fatal("refund transport failure passed")
	}

	adapter.httpClient = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("upstream error"))}, nil
	})
	if _, err := adapter.Refund(context.Background(), request); err == nil {
		t.Fatal("refund HTTP failure passed")
	}
	adapter.httpClient = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("not-json"))}, nil
	})
	if _, err := adapter.Refund(context.Background(), request); err == nil {
		t.Fatal("invalid refund response passed")
	}
}

func TestSimulatorEndpointOverrideIsSandboxOnlyAndPathExact(t *testing.T) {
	config := enabledConfig(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") }))
	config.EndpointBaseURL = "http://payment-simulator.test:8081"
	adapter, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.mpgURL() != "http://payment-simulator.test:8081/MPG/mpg_gateway" || adapter.queryURL() != "http://payment-simulator.test:8081/API/QueryTradeInfo" {
		t.Fatalf("mpg=%q query=%q", adapter.mpgURL(), adapter.queryURL())
	}
	config.Environment = EnvironmentProduction
	if _, err := New(config); err == nil || !strings.Contains(err.Error(), "override is forbidden") {
		t.Fatalf("production override err=%v", err)
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
