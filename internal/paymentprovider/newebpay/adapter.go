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
	maxResponseBytes   = 1 << 20
)

var merchantOrderPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,30}$`)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Config struct {
	Enabled     bool
	Environment string
	MerchantID  string
	HashKey     string
	HashIV      string
	HTTPClient  HTTPDoer
	Timeout     time.Duration
}

type Adapter struct {
	enabled     bool
	environment string
	merchantID  string
	hashKey     string
	hashIV      string
	httpClient  HTTPDoer
	timeout     time.Duration
	now         func() time.Time
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
		timeout: config.Timeout, now: func() time.Time { return time.Now().UTC() },
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
		StatusQuery: true,
		Webhook:     true,
		// Public NDNF-1.2.3 documents MPG and remembered-card UI flows, but
		// does not establish unattended, variable-time merchant-initiated
		// charging. Keep this false until the merchant capability is approved.
		MerchantInitiatedCharge: false,
	}
}

func (a *Adapter) CreateSetup(context.Context, payment.SetupRequest) (payment.SetupResult, error) {
	return payment.SetupResult{}, payment.ErrProviderUnsupported
}

func (a *Adapter) Charge(context.Context, payment.ChargeRequest) (payment.ProviderResult, error) {
	return payment.ProviderResult{}, payment.ErrProviderUnsupported
}

func (a *Adapter) Refund(context.Context, payment.RefundRequest) (payment.ProviderResult, error) {
	return payment.ProviderResult{}, payment.ErrProviderUnsupported
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
	endpoint := sandboxQueryURL
	if a.environment == EnvironmentProduction {
		endpoint = productionQueryURL
	}
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
	if a.environment == EnvironmentProduction {
		return productionMPGURL
	}
	return sandboxMPGURL
}
