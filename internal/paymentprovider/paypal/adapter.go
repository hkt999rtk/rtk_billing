package paypal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

const maxResponseBytes = 1 << 20

var orderReference = regexp.MustCompile(`^rtk_[0-9a-f]{26}$`)
var paypalID = regexp.MustCompile(`^[A-Z0-9]{10,32}$`)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Config struct {
	Environment     string
	ClientID        string
	ClientSecret    string
	WebhookID       string
	ReturnURL       string
	CancelURL       string
	HTTPClient      HTTPDoer
	EndpointBaseURL string // Local contract tests only; never configure in production.
}

type Adapter struct {
	baseURL, clientID, clientSecret, webhookID, returnURL, cancelURL string
	client                                                           HTTPDoer
}

func New(cfg Config) (*Adapter, error) {
	env := strings.ToLower(strings.TrimSpace(cfg.Environment))
	base := "https://api-m.sandbox.paypal.com"
	if env == "production" {
		base = "https://api-m.paypal.com"
	} else if env != "sandbox" {
		return nil, errors.New("PayPal environment must be sandbox or production")
	}
	if cfg.EndpointBaseURL != "" {
		if env == "production" {
			return nil, errors.New("PayPal production endpoint override is forbidden")
		}
		parsed, err := url.Parse(cfg.EndpointBaseURL)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, errors.New("invalid PayPal test endpoint")
		}
		base = strings.TrimRight(cfg.EndpointBaseURL, "/")
	}
	if strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.ClientSecret) == "" || strings.TrimSpace(cfg.WebhookID) == "" || !validURL(cfg.ReturnURL, env) || !validURL(cfg.CancelURL, env) {
		return nil, errors.New("PayPal requires client credentials, webhook ID, and fixed return/cancel URLs")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Adapter{baseURL: base, clientID: cfg.ClientID, clientSecret: cfg.ClientSecret, webhookID: cfg.WebhookID, returnURL: cfg.ReturnURL, cancelURL: cfg.CancelURL, client: client}, nil
}

func validURL(raw, env string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Scheme == "https" || env == "sandbox" && u.Scheme == "http")
}

func (a *Adapter) Name() string { return "paypal" }
func (a *Adapter) Capabilities(context.Context) payment.ProviderCapabilities {
	return payment.ProviderCapabilities{HostedCharge: true, StatusQuery: true, Webhook: true}
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

type amount struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}
type capture struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	Amount            amount `json:"amount"`
	CustomID          string `json:"custom_id"`
	SupplementaryData struct {
		RelatedIDs struct {
			OrderID string `json:"order_id"`
		} `json:"related_ids"`
	} `json:"supplementary_data"`
}
type purchaseUnit struct {
	CustomID string `json:"custom_id"`
	Amount   amount `json:"amount"`
	Payments struct {
		Captures []capture `json:"captures"`
	} `json:"payments"`
}
type link struct {
	Href string `json:"href"`
	Rel  string `json:"rel"`
}
type order struct {
	ID            string         `json:"id"`
	Status        string         `json:"status"`
	PurchaseUnits []purchaseUnit `json:"purchase_units"`
	Links         []link         `json:"links"`
}

func (a *Adapter) CreateHostedCharge(ctx context.Context, in payment.HostedChargeRequest) (payment.HostedChargeResult, error) {
	if !orderReference.MatchString(in.MerchantOrderReference) || in.Currency != payment.CurrencyTWD || payment.ValidateChargeAmount(in.Currency, in.AmountMinor) != nil {
		return payment.HostedChargeResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_hosted_charge", false, nil)
	}
	body := map[string]any{
		"intent":         "CAPTURE",
		"purchase_units": []any{map[string]any{"custom_id": in.MerchantOrderReference, "invoice_id": in.MerchantOrderReference, "description": "RTK Cloud account top-up", "amount": amount{CurrencyCode: "TWD", Value: strconv.FormatInt(in.AmountMinor, 10)}}},
		"payment_source": map[string]any{"paypal": map[string]any{"experience_context": map[string]any{"return_url": a.returnURL, "cancel_url": a.cancelURL, "shipping_preference": "NO_SHIPPING", "user_action": "PAY_NOW"}}},
	}
	var out order
	if err := a.call(ctx, http.MethodPost, "/v2/checkout/orders", in.MerchantOrderReference, body, &out); err != nil {
		return payment.HostedChargeResult{}, err
	}
	if !paypalID.MatchString(out.ID) || len(out.PurchaseUnits) != 1 || !matchesUnit(out.PurchaseUnits[0], in.MerchantOrderReference, in.AmountMinor, in.Currency) {
		return payment.HostedChargeResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, "invalid_order_response", true, nil)
	}
	for _, l := range out.Links {
		if l.Rel == "payer-action" || l.Rel == "approve" {
			u, e := url.Parse(l.Href)
			if e == nil && u.Scheme == "https" && (u.Host == "www.paypal.com" || u.Host == "www.sandbox.paypal.com") {
				return payment.HostedChargeResult{Method: "GET", EndpointURL: l.Href, ProviderTransactionReference: out.ID}, nil
			}
		}
	}
	return payment.HostedChargeResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, "approval_url_missing", true, nil)
}

func matchesUnit(unit purchaseUnit, ref string, minor int64, currency payment.Currency) bool {
	return unit.CustomID == ref && unit.Amount.CurrencyCode == string(currency) && unit.Amount.Value == strconv.FormatInt(minor, 10)
}

func (a *Adapter) Query(ctx context.Context, in payment.QueryRequest) (payment.ProviderResult, error) {
	if !paypalID.MatchString(in.ProviderTransactionReference) {
		return payment.ProviderResult{State: payment.PaymentIntentStateUnknown, ProviderCode: "order_reference_missing"}, nil
	}
	out, err := a.getOrder(ctx, in.ProviderTransactionReference)
	if err != nil {
		return payment.ProviderResult{}, err
	}
	if out.ID != in.ProviderTransactionReference || len(out.PurchaseUnits) != 1 || !matchesUnit(out.PurchaseUnits[0], in.MerchantOrderReference, in.AmountMinor, in.Currency) {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, "order_mismatch", true, nil)
	}
	result := payment.ProviderResult{State: payment.PaymentIntentStateRequiresAction, ProviderTransactionReference: out.ID, ProviderCode: strings.ToLower(out.Status)}
	switch out.Status {
	case "COMPLETED":
		if len(out.PurchaseUnits[0].Payments.Captures) != 1 {
			return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, "capture_mismatch", true, nil)
		}
		captured := out.PurchaseUnits[0].Payments.Captures[0]
		if captured.Amount.CurrencyCode != string(in.Currency) || captured.Amount.Value != strconv.FormatInt(in.AmountMinor, 10) || (captured.CustomID != "" && captured.CustomID != in.MerchantOrderReference) {
			return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorUnknown, "capture_mismatch", true, nil)
		}
		if captured.Status == "COMPLETED" {
			result.State = payment.PaymentIntentStateSucceeded
		} else {
			result.State = payment.PaymentIntentStateUnknown
		}
	case "VOIDED":
		result.State = payment.PaymentIntentStateCanceled
	case "APPROVED":
		result.State = payment.PaymentIntentStateRequiresAction
	case "CREATED", "PAYER_ACTION_REQUIRED":
		result.State = payment.PaymentIntentStateRequiresAction
	default:
		result.State = payment.PaymentIntentStateUnknown
	}
	return result, nil
}

func (a *Adapter) getOrder(ctx context.Context, id string) (order, error) {
	var out order
	err := a.call(ctx, http.MethodGet, "/v2/checkout/orders/"+id, "", nil, &out)
	return out, err
}

// Capture is called only after PayPal returns an approved buyer to the fixed return URL.
func (a *Adapter) Capture(ctx context.Context, in payment.QueryRequest) (payment.ProviderResult, error) {
	current, err := a.Query(ctx, in)
	if err != nil || current.State == payment.PaymentIntentStateSucceeded {
		return current, err
	}
	if current.ProviderCode != "approved" {
		return current, nil
	}
	var out order
	if err := a.call(ctx, http.MethodPost, "/v2/checkout/orders/"+in.ProviderTransactionReference+"/capture", "capture-"+in.MerchantOrderReference, map[string]any{}, &out); err != nil {
		return payment.ProviderResult{}, err
	}
	// Re-read the canonical order so the credited amount and custom ID are checked.
	return a.Query(ctx, in)
}

func (a *Adapter) token(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/oauth2/token", strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(a.clientID, a.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := a.do(req, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", payment.NewProviderError(payment.ProviderErrorAuthentication, "token_missing", false, nil)
	}
	return out.AccessToken, nil
}

func (a *Adapter) call(ctx context.Context, method, path, requestID string, body any, out any) error {
	token, err := a.token(ctx)
	if err != nil {
		return err
	}
	var content io.Reader
	if body != nil {
		data, e := json.Marshal(body)
		if e != nil {
			return e
		}
		content = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, content)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Prefer", "return=representation")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if requestID != "" {
		req.Header.Set("PayPal-Request-Id", requestID)
	}
	return a.do(req, out)
}

func (a *Adapter) do(req *http.Request, out any) error {
	response, err := a.client.Do(req)
	if err != nil {
		return payment.NewProviderError(payment.ProviderErrorUnknown, "transport_error", true, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return payment.NewProviderError(payment.ProviderErrorUnknown, "invalid_response", true, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return payment.NewProviderError(payment.ProviderErrorUnknown, fmt.Sprintf("http_%d", response.StatusCode), response.StatusCode >= 500, nil)
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return payment.NewProviderError(payment.ProviderErrorUnknown, "invalid_json", true, nil)
	}
	return nil
}
