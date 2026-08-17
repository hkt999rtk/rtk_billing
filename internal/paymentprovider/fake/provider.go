package fake

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

type Outcome struct {
	Result payment.ProviderResult
	Err    error
}

type SetupOutcome struct {
	Result payment.SetupResult
	Err    error
}

type Provider struct {
	mu sync.Mutex

	secret         []byte
	setupOutcomes  []SetupOutcome
	chargeOutcomes []Outcome
	queryOutcomes  []Outcome
	setupByKey     map[string]SetupOutcome
	chargeByOrder  map[string]Outcome
	setupCalls     []payment.SetupRequest
	chargeCalls    []payment.ChargeRequest
	queryCalls     []payment.QueryRequest
}

func New(secret string) *Provider {
	return &Provider{
		secret:        []byte(secret),
		setupByKey:    make(map[string]SetupOutcome),
		chargeByOrder: make(map[string]Outcome),
	}
}

func (p *Provider) Name() string { return "fake" }

func (p *Provider) Capabilities(context.Context) payment.ProviderCapabilities {
	return payment.ProviderCapabilities{
		HostedSetup:             true,
		VaultedMethod:           true,
		MerchantInitiatedCharge: true,
		StatusQuery:             true,
		Webhook:                 true,
	}
}

func (p *Provider) QueueSetup(outcomes ...SetupOutcome) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setupOutcomes = append(p.setupOutcomes, outcomes...)
}

func (p *Provider) QueueCharge(outcomes ...Outcome) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chargeOutcomes = append(p.chargeOutcomes, outcomes...)
}

func (p *Provider) QueueQuery(outcomes ...Outcome) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queryOutcomes = append(p.queryOutcomes, outcomes...)
}

func (p *Provider) Charge(_ context.Context, request payment.ChargeRequest) (payment.ProviderResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chargeCalls = append(p.chargeCalls, request)
	if outcome, exists := p.chargeByOrder[request.MerchantOrderReference]; exists {
		return outcome.Result, outcome.Err
	}
	outcome := pop(&p.chargeOutcomes)
	p.chargeByOrder[request.MerchantOrderReference] = outcome
	return outcome.Result, outcome.Err
}

func (p *Provider) Query(_ context.Context, request payment.QueryRequest) (payment.ProviderResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queryCalls = append(p.queryCalls, request)
	outcome := pop(&p.queryOutcomes)
	return outcome.Result, outcome.Err
}

func pop(outcomes *[]Outcome) Outcome {
	if len(*outcomes) == 0 {
		return Outcome{Err: payment.NewProviderError(payment.ProviderErrorTemporary, "no_fake_outcome", false, nil)}
	}
	outcome := (*outcomes)[0]
	*outcomes = (*outcomes)[1:]
	return outcome
}

func (p *Provider) CreateSetup(_ context.Context, request payment.SetupRequest) (payment.SetupResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setupCalls = append(p.setupCalls, request)
	if strings.TrimSpace(request.AccountID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.CorrelationID) == "" {
		return payment.SetupResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_fake_setup_request", false, nil)
	}
	setupKey := request.AccountID + "\x00" + request.IdempotencyKey
	if outcome, exists := p.setupByKey[setupKey]; exists {
		return outcome.Result, outcome.Err
	}
	outcome := SetupOutcome{Err: payment.NewProviderError(payment.ProviderErrorTemporary, "no_fake_setup_outcome", false, nil)}
	if len(p.setupOutcomes) > 0 {
		outcome = p.setupOutcomes[0]
		p.setupOutcomes = p.setupOutcomes[1:]
	}
	p.setupByKey[setupKey] = outcome
	return outcome.Result, outcome.Err
}

func (p *Provider) Refund(context.Context, payment.RefundRequest) (payment.ProviderResult, error) {
	return payment.ProviderResult{}, payment.ErrProviderUnsupported
}

type webhookPayload struct {
	EventReference               string                     `json:"event_reference"`
	MerchantOrderReference       string                     `json:"merchant_order_reference"`
	ProviderTransactionReference string                     `json:"provider_transaction_reference"`
	AmountMinor                  int64                      `json:"amount_minor"`
	Currency                     payment.Currency           `json:"currency"`
	State                        payment.PaymentIntentState `json:"state"`
	EventType                    string                     `json:"event_type"`
	ProviderCode                 string                     `json:"provider_code"`
}

func (p *Provider) VerifyWebhook(_ context.Context, request payment.WebhookRequest) (payment.WebhookEvent, error) {
	provided, err := hex.DecodeString(strings.TrimSpace(request.Header.Get("X-Fake-Signature")))
	if err != nil {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorAuthentication, "invalid_signature_encoding", false, err)
	}
	mac := hmac.New(sha256.New, p.secret)
	_, _ = mac.Write(request.Body)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorAuthentication, "invalid_signature", false, nil)
	}
	decoder := json.NewDecoder(strings.NewReader(string(request.Body)))
	decoder.DisallowUnknownFields()
	var payload webhookPayload
	if err := decoder.Decode(&payload); err != nil {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_payload", false, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_payload", false, errors.New("multiple webhook values"))
	}
	if payload.EventReference == "" || payload.MerchantOrderReference == "" || payload.EventType == "" ||
		payment.ValidateChargeAmount(payload.Currency, payload.AmountMinor) != nil ||
		payment.ValidateProviderResult(payment.ProviderResult{State: payload.State}) != nil {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_payload", false, errors.New("missing or invalid webhook fields"))
	}
	return payment.WebhookEvent{
		ProviderEventReference:       payload.EventReference,
		MerchantOrderReference:       payload.MerchantOrderReference,
		ProviderTransactionReference: payload.ProviderTransactionReference,
		AmountMinor:                  payload.AmountMinor,
		Currency:                     payload.Currency,
		State:                        payload.State,
		EventType:                    payload.EventType,
		ProviderCode:                 payload.ProviderCode,
	}, nil
}

func (p *Provider) ChargeCalls() []payment.ChargeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]payment.ChargeRequest(nil), p.chargeCalls...)
}

func (p *Provider) SetupCalls() []payment.SetupRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]payment.SetupRequest(nil), p.setupCalls...)
}

func (p *Provider) QueryCalls() []payment.QueryRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]payment.QueryRequest(nil), p.queryCalls...)
}

func SignWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func WebhookBody(event payment.WebhookEvent) ([]byte, error) {
	if event.ProviderEventReference == "" {
		return nil, fmt.Errorf("event reference is required")
	}
	return json.Marshal(webhookPayload{
		EventReference:               event.ProviderEventReference,
		MerchantOrderReference:       event.MerchantOrderReference,
		ProviderTransactionReference: event.ProviderTransactionReference,
		AmountMinor:                  event.AmountMinor,
		Currency:                     event.Currency,
		State:                        event.State,
		EventType:                    event.EventType,
		ProviderCode:                 event.ProviderCode,
	})
}
