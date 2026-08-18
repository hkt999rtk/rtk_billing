package simulator

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

const ProviderName = "simulator"

type Config struct {
	BaseURL      string
	SharedSecret string
	RunID        string
	Scenario     string
	Timeout      time.Duration
	HTTPClient   *http.Client
}

type Client struct {
	baseURL  string
	secret   []byte
	runID    string
	scenario string
	http     *http.Client
	timeout  time.Duration
}

type providerResponse struct {
	State                        payment.PaymentIntentState `json:"state"`
	HostedURL                    string                     `json:"hosted_url"`
	ProviderCustomerRef          string                     `json:"provider_customer_ref"`
	ProviderMethodRef            string                     `json:"provider_method_ref"`
	ProviderTransactionReference string                     `json:"provider_transaction_reference"`
	ProviderCode                 string                     `json:"provider_code"`
	RequiresUserAction           bool                       `json:"requires_user_action"`
	CardBrand                    string                     `json:"card_brand"`
	LastFour                     string                     `json:"last_four"`
	ExpiryMonth                  *int                       `json:"expiry_month"`
	ExpiryYear                   *int                       `json:"expiry_year"`
	Evidence                     map[string]string          `json:"evidence"`
}

func New(config Config) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("payment simulator base URL must be an absolute HTTP(S) URL")
	}
	if len(strings.TrimSpace(config.SharedSecret)) < 32 {
		return nil, fmt.Errorf("payment simulator shared secret must contain at least 32 characters")
	}
	if !validRunID(config.RunID) {
		return nil, fmt.Errorf("payment simulator run ID is required and must use letters, digits, dot, underscore, or hyphen")
	}
	if config.Timeout <= 0 {
		config.Timeout = 10 * time.Second
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	return &Client{
		baseURL: strings.TrimRight(parsed.String(), "/"), secret: []byte(strings.TrimSpace(config.SharedSecret)), runID: strings.TrimSpace(config.RunID),
		scenario: normalizeScenario(config.Scenario), http: config.HTTPClient, timeout: config.Timeout,
	}, nil
}

func (c *Client) Name() string { return ProviderName }

func (c *Client) Capabilities(context.Context) payment.ProviderCapabilities {
	return payment.ProviderCapabilities{HostedSetup: true, VaultedMethod: true, MerchantInitiatedCharge: true, StatusQuery: true, Refund: true}
}

func (c *Client) CreateSetup(ctx context.Context, request payment.SetupRequest) (payment.SetupResult, error) {
	if strings.TrimSpace(request.AccountID) == "" || strings.TrimSpace(request.LocalSessionID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.CorrelationID) == "" {
		return payment.SetupResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_setup_request", false, nil)
	}
	var response providerResponse
	err := c.post(ctx, "/internal/v1/setup-sessions", map[string]any{
		"run_id":     c.runID,
		"account_id": request.AccountID, "setup_session_id": request.LocalSessionID,
		"idempotency_key": request.IdempotencyKey, "correlation_id": request.CorrelationID, "scenario": c.scenario,
	}, &response)
	if err != nil {
		return payment.SetupResult{}, err
	}
	return payment.SetupResult{
		State: response.State, HostedURL: response.HostedURL, ProviderCustomerRef: response.ProviderCustomerRef,
		ProviderMethodRef: response.ProviderMethodRef, ProviderCode: response.ProviderCode,
		RequiresUserAction: response.RequiresUserAction, CardBrand: response.CardBrand, LastFour: response.LastFour,
		ExpiryMonth: response.ExpiryMonth, ExpiryYear: response.ExpiryYear,
	}, nil
}

func (c *Client) Charge(ctx context.Context, request payment.ChargeRequest) (payment.ProviderResult, error) {
	var response providerResponse
	err := c.post(ctx, "/internal/v1/charges", map[string]any{
		"run_id":    c.runID,
		"intent_id": request.IntentID, "amount_minor": request.AmountMinor, "currency": request.Currency,
		"opaque_method_reference": request.OpaqueMethodReference, "merchant_order_reference": request.MerchantOrderReference,
		"idempotency_key": request.IdempotencyKey, "correlation_id": request.CorrelationID, "scenario": c.scenario,
	}, &response)
	return providerResult(response), err
}

func (c *Client) Query(ctx context.Context, request payment.QueryRequest) (payment.ProviderResult, error) {
	var response providerResponse
	err := c.post(ctx, "/internal/v1/queries", map[string]any{
		"run_id":    c.runID,
		"intent_id": request.IntentID, "amount_minor": request.AmountMinor, "currency": request.Currency,
		"merchant_order_reference":       request.MerchantOrderReference,
		"provider_transaction_reference": request.ProviderTransactionReference,
		"correlation_id":                 request.CorrelationID, "scenario": c.scenario,
	}, &response)
	return providerResult(response), err
}

func (c *Client) Refund(ctx context.Context, request payment.RefundRequest) (payment.ProviderResult, error) {
	var response providerResponse
	err := c.post(ctx, "/internal/v1/refunds", map[string]any{
		"run_id":    c.runID,
		"intent_id": request.IntentID, "amount_minor": request.AmountMinor, "currency": request.Currency,
		"provider_transaction_reference": request.ProviderTransactionReference,
		"idempotency_key":                request.IdempotencyKey, "correlation_id": request.CorrelationID, "scenario": c.scenario,
	}, &response)
	return providerResult(response), err
}

func (c *Client) VerifyWebhook(context.Context, payment.WebhookRequest) (payment.WebhookEvent, error) {
	return payment.WebhookEvent{}, payment.ErrProviderUnsupported
}

func providerResult(response providerResponse) payment.ProviderResult {
	return payment.ProviderResult{
		State: response.State, ProviderTransactionReference: response.ProviderTransactionReference,
		ProviderCode: response.ProviderCode, Evidence: response.Evidence,
	}
}

func (c *Client) post(ctx context.Context, path string, payload any, destination any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return payment.NewProviderError(payment.ProviderErrorInvalidRequest, "encode_failed", false, err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return payment.NewProviderError(payment.ProviderErrorInvalidRequest, "request_build_failed", false, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Payment-Simulator-Signature", sign(c.secret, body))
	response, err := c.http.Do(request)
	if err != nil {
		return payment.NewProviderError(payment.ProviderErrorTemporary, "simulator_unavailable", false, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 128*1024))
	if err != nil {
		return payment.NewProviderError(payment.ProviderErrorUnknown, "response_read_failed", true, err)
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		kind := payment.ProviderErrorTemporary
		if response.StatusCode == http.StatusUnauthorized {
			kind = payment.ProviderErrorAuthentication
		} else if response.StatusCode >= 400 && response.StatusCode < 500 {
			kind = payment.ProviderErrorInvalidRequest
		}
		return payment.NewProviderError(kind, fmt.Sprintf("simulator_http_%d", response.StatusCode), response.StatusCode >= 500, nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return payment.NewProviderError(payment.ProviderErrorUnknown, "invalid_simulator_response", true, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return payment.NewProviderError(payment.ProviderErrorUnknown, "invalid_simulator_response", true, err)
	}
	return nil
}

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func normalizeScenario(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "success"
	}
	return value
}

func validRunID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || index > 0 && (character == '.' || character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}
