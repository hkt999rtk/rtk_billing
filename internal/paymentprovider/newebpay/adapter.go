package newebpay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

const (
	EnvironmentSandbox    = "sandbox"
	EnvironmentProduction = "production"

	sandboxMPGURL      = "https://ccore.newebpay.com/MPG/mpg_gateway"
	productionMPGURL   = "https://core.newebpay.com/MPG/mpg_gateway"
	sandboxQueryURL    = "https://ccore.newebpay.com/API/QueryTradeInfo"
	productionQueryURL = "https://core.newebpay.com/API/QueryTradeInfo"
	sandboxCloseURL    = "https://ccore.newebpay.com/API/CreditCard/Close"
	productionCloseURL = "https://core.newebpay.com/API/CreditCard/Close"
	maxResponseBytes   = 1 << 20
)

var merchantOrderPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,30}$`)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Config struct {
	Enabled         bool
	Environment     string
	MerchantID      string
	HashKey         string
	HashIV          string
	HTTPClient      HTTPDoer
	Timeout         time.Duration
	EndpointBaseURL string
}

type Adapter struct {
	enabled         bool
	environment     string
	merchantID      string
	hashKey         string
	hashIV          string
	httpClient      HTTPDoer
	timeout         time.Duration
	now             func() time.Time
	endpointBaseURL string
}

func New(config Config) (*Adapter, error) {
	environment := strings.ToLower(strings.TrimSpace(config.Environment))
	if environment == "" {
		environment = EnvironmentSandbox
	}
	if environment != EnvironmentSandbox && environment != EnvironmentProduction {
		return nil, fmt.Errorf("NewebPay environment must be sandbox or production")
	}
	if config.Timeout <= 0 {
		config.Timeout = 10 * time.Second
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: config.Timeout}
	}
	adapter := &Adapter{
		enabled: config.Enabled, environment: environment,
		merchantID: strings.TrimSpace(config.MerchantID), hashKey: strings.TrimSpace(config.HashKey),
		hashIV: strings.TrimSpace(config.HashIV), httpClient: config.HTTPClient,
		timeout: config.Timeout, now: func() time.Time { return time.Now().UTC() }, endpointBaseURL: strings.TrimRight(strings.TrimSpace(config.EndpointBaseURL), "/"),
	}
	if adapter.endpointBaseURL != "" {
		parsed, parseErr := url.Parse(adapter.endpointBaseURL)
		if parseErr != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("NewebPay endpoint base URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
		if environment == EnvironmentProduction {
			return nil, fmt.Errorf("NewebPay production endpoint override is forbidden")
		}
	}
	if !adapter.enabled {
		return adapter, nil
	}
	if adapter.merchantID == "" || len(adapter.merchantID) > 15 ||
		len(adapter.hashKey) != 32 || len(adapter.hashIV) != 16 {
		return nil, fmt.Errorf("enabled NewebPay requires MerchantID (max 15), 32-byte HashKey, and 16-byte HashIV")
	}
	return adapter, nil
}

func (a *Adapter) Name() string { return "newebpay" }

func (a *Adapter) Capabilities(context.Context) payment.ProviderCapabilities {
	if !a.enabled {
		return payment.ProviderCapabilities{}
	}
	return payment.ProviderCapabilities{
		HostedCharge: true,
		StatusQuery:  true,
		Webhook:      true,
		Refund:       true,
		// Public NDNF-1.2.4 documents MPG and remembered-card UI flows, but
		// does not establish unattended, variable-time merchant-initiated
		// charging. Keep this false until the merchant capability is approved.
		MerchantInitiatedCharge: false,
	}
}

func (a *Adapter) CreateHostedCharge(_ context.Context, request payment.HostedChargeRequest) (payment.HostedChargeResult, error) {
	if !a.enabled {
		return payment.HostedChargeResult{}, payment.NewProviderError(payment.ProviderErrorUnsupported, "provider_disabled", false, payment.ErrProviderUnsupported)
	}
	if !merchantOrderPattern.MatchString(request.MerchantOrderReference) || payment.ValidateChargeAmount(request.Currency, request.AmountMinor) != nil || request.AmountMinor > 9_999_999_999 {
		return payment.HostedChargeResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_hosted_charge", false, nil)
	}
	if !validCallbackURL(request.NotifyURL) || !validCallbackURL(request.ReturnURL) || strings.TrimSpace(request.ItemDescription) == "" {
		return payment.HostedChargeResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_callback_url", false, nil)
	}
	description := []rune(strings.TrimSpace(request.ItemDescription))
	if len(description) > 50 {
		description = description[:50]
	}
	trade := url.Values{
		"MerchantID": {a.merchantID}, "RespondType": {"JSON"}, "TimeStamp": {strconv.FormatInt(a.now().Unix(), 10)},
		"Version": {"2.3"}, "LangType": {"zh-tw"}, "MerchantOrderNo": {request.MerchantOrderReference},
		"Amt": {strconv.FormatInt(request.AmountMinor, 10)}, "ItemDesc": {string(description)}, "NotifyURL": {request.NotifyURL},
		"ReturnURL": {request.ReturnURL}, "CREDIT": {"1"},
	}
	encrypted, err := EncryptTradeInfo(trade.Encode(), a.hashKey, a.hashIV)
	if err != nil {
		return payment.HostedChargeResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "encryption_failed", false, err)
	}
	return payment.HostedChargeResult{EndpointURL: a.mpgURL(), Fields: map[string]string{
		"MerchantID": a.merchantID, "TradeInfo": encrypted, "TradeSha": TradeSHA(encrypted, a.hashKey, a.hashIV), "Version": "2.3",
	}}, nil
}

func validCallbackURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func (a *Adapter) CreateSetup(context.Context, payment.SetupRequest) (payment.SetupResult, error) {
	return payment.SetupResult{}, payment.ErrProviderUnsupported
}

func (a *Adapter) Charge(context.Context, payment.ChargeRequest) (payment.ProviderResult, error) {
	return payment.ProviderResult{}, payment.ErrProviderUnsupported
}

func (a *Adapter) Refund(ctx context.Context, request payment.RefundRequest) (payment.ProviderResult, error) {
	if !a.enabled {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnsupported, "provider_disabled", false, payment.ErrProviderUnsupported)
	}
	tradeNo := strings.TrimSpace(request.ProviderTransactionReference)
	if payment.ValidateChargeAmount(request.Currency, request.AmountMinor) != nil || request.AmountMinor > 9_999_999_999 || tradeNo == "" || len(tradeNo) > 30 {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_refund", false, nil)
	}
	postData, err := EncryptTradeInfo(url.Values{
		"RespondType": {"JSON"}, "Version": {"1.1"}, "Amt": {strconv.FormatInt(request.AmountMinor, 10)},
		"TimeStamp": {strconv.FormatInt(a.now().Unix(), 10)}, "IndexType": {"2"}, "TradeNo": {tradeNo}, "CloseType": {"2"},
	}.Encode(), a.hashKey, a.hashIV)
	if err != nil {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "encryption_failed", false, err)
	}
	form := url.Values{"MerchantID_": {a.merchantID}, "PostData_": {postData}}
	requestContext, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, a.closeURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "request_build_failed", false, err)
	}
	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := a.httpClient.Do(httpRequest)
	if err != nil {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorTemporary, "refund_transport", false, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes || response.StatusCode < 200 || response.StatusCode >= 300 {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorTemporary, "refund_response", false, err)
	}
	var decoded struct {
		Status string `json:"Status"`
		Result struct {
			MerchantID string        `json:"MerchantID"`
			Amt        numericString `json:"Amt"`
			TradeNo    string        `json:"TradeNo"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, "invalid_refund_response", false, err)
	}
	amount, amountErr := strconv.ParseInt(string(decoded.Result.Amt), 10, 64)
	if decoded.Status != "SUCCESS" || decoded.Result.MerchantID != a.merchantID || decoded.Result.TradeNo != tradeNo || amountErr != nil || amount != request.AmountMinor {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, payment.NormalizeProviderCode(decoded.Status), false, nil)
	}
	return payment.ProviderResult{State: payment.PaymentIntentStateSucceeded, ProviderTransactionReference: tradeNo, ProviderCode: "refund_succeeded"}, nil
}

func (a *Adapter) Query(ctx context.Context, request payment.QueryRequest) (payment.ProviderResult, error) {
	return a.queryWithAmount(ctx, request, request.AmountMinor, request.Currency)
}

func (a *Adapter) queryWithAmount(ctx context.Context, request payment.QueryRequest, amountMinor int64, currency payment.Currency) (payment.ProviderResult, error) {
	if !a.enabled {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnsupported, "provider_disabled", false, payment.ErrProviderUnsupported)
	}
	if !merchantOrderPattern.MatchString(request.MerchantOrderReference) ||
		payment.ValidateChargeAmount(currency, amountMinor) != nil || amountMinor > 9_999_999_999 {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_query", false, nil)
	}
	amountNTD := amountMinor
	form := url.Values{
		"MerchantID":      []string{a.merchantID},
		"Version":         []string{"1.3"},
		"RespondType":     []string{"JSON"},
		"CheckValue":      []string{QueryCheckValue(amountNTD, a.merchantID, request.MerchantOrderReference, a.hashKey, a.hashIV)},
		"TimeStamp":       []string{strconv.FormatInt(a.now().Unix(), 10)},
		"MerchantOrderNo": []string{request.MerchantOrderReference},
		"Amt":             []string{strconv.FormatInt(amountNTD, 10)},
	}
	endpoint := a.queryURL()
	requestContext, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "request_build_failed", false, err)
	}
	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := a.httpClient.Do(httpRequest)
	if err != nil {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorTemporary, "query_transport", false, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorTemporary, "query_read", false, err)
	}
	if len(body) > maxResponseBytes {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "response_too_large", false, nil)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorTemporary, "http_"+strconv.Itoa(response.StatusCode), false, nil)
	}
	return a.parseQueryResponse(body, request, amountNTD)
}

type queryResponse struct {
	Status  string          `json:"Status"`
	Message string          `json:"Message"`
	Result  json.RawMessage `json:"Result"`
}

type queryResult struct {
	MerchantID      string        `json:"MerchantID"`
	Amt             numericString `json:"Amt"`
	TradeNo         string        `json:"TradeNo"`
	MerchantOrderNo string        `json:"MerchantOrderNo"`
	TradeStatus     string        `json:"TradeStatus"`
	CheckCode       string        `json:"CheckCode"`
}

type numericString string

func (value *numericString) UnmarshalJSON(encoded []byte) error {
	raw := strings.TrimSpace(string(encoded))
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		var decoded string
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return err
		}
		raw = decoded
	}
	if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
		return err
	}
	*value = numericString(raw)
	return nil
}

func (a *Adapter) parseQueryResponse(body []byte, request payment.QueryRequest, amountNTD int64) (payment.ProviderResult, error) {
	var response queryResponse
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, "invalid_query_response", false, err)
	}
	if response.Status != "SUCCESS" {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, response.Status, false, nil)
	}
	var result queryResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, "invalid_query_result", false, err)
	}
	parsedAmount, err := strconv.ParseInt(string(result.Amt), 10, 64)
	if err != nil || result.MerchantID != a.merchantID || result.MerchantOrderNo != request.MerchantOrderReference || parsedAmount != amountNTD || result.TradeNo == "" {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, "query_result_mismatch", false, nil)
	}
	expectedCheckCode := ResponseCheckCode(map[string]string{
		"Amt": strconv.FormatInt(parsedAmount, 10), "MerchantID": result.MerchantID,
		"MerchantOrderNo": result.MerchantOrderNo, "TradeNo": result.TradeNo,
	}, a.hashKey, a.hashIV)
	if !VerifyDigest(expectedCheckCode, result.CheckCode) {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorAuthentication, "invalid_check_code", false, nil)
	}
	state := payment.PaymentIntentStateUnknown
	switch result.TradeStatus {
	case "1":
		state = payment.PaymentIntentStateSucceeded
	case "2":
		state = payment.PaymentIntentStateFailed
	case "3", "6":
		state = payment.PaymentIntentStateCanceled
	case "0":
		state = payment.PaymentIntentStateUnknown
	default:
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, "unknown_trade_status", false, nil)
	}
	return payment.ProviderResult{
		State: state, ProviderTransactionReference: result.TradeNo,
		ProviderCode: "trade_status_" + result.TradeStatus,
	}, nil
}

func (a *Adapter) mpgURL() string {
	if a.endpointBaseURL != "" {
		return a.endpointBaseURL + "/MPG/mpg_gateway"
	}
	if a.environment == EnvironmentProduction {
		return productionMPGURL
	}
	return sandboxMPGURL
}

func (a *Adapter) queryURL() string {
	if a.endpointBaseURL != "" {
		return a.endpointBaseURL + "/API/QueryTradeInfo"
	}
	if a.environment == EnvironmentProduction {
		return productionQueryURL
	}
	return sandboxQueryURL
}

func (a *Adapter) closeURL() string {
	if a.endpointBaseURL != "" {
		return a.endpointBaseURL + "/API/CreditCard/Close"
	}
	if a.environment == EnvironmentProduction {
		return productionCloseURL
	}
	return sandboxCloseURL
}
