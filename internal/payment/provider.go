package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type ProviderOperation string

const (
	ProviderOperationSetup  ProviderOperation = "setup"
	ProviderOperationCharge ProviderOperation = "charge"
	ProviderOperationQuery  ProviderOperation = "query"
	ProviderOperationCancel ProviderOperation = "cancel"
	ProviderOperationRefund ProviderOperation = "refund"
)

type ProviderErrorKind string

const (
	ProviderErrorDeclined       ProviderErrorKind = "declined"
	ProviderErrorTemporary      ProviderErrorKind = "temporary"
	ProviderErrorInvalidRequest ProviderErrorKind = "invalid_request"
	ProviderErrorAuthentication ProviderErrorKind = "authentication"
	ProviderErrorRequiresAction ProviderErrorKind = "requires_action"
	ProviderErrorUnknown        ProviderErrorKind = "unknown"
	ProviderErrorUnsupported    ProviderErrorKind = "unsupported"
)

var ErrProviderUnsupported = errors.New("payment provider operation is unsupported")

type ProviderError struct {
	Kind                 ProviderErrorKind
	Code                 string
	MayHaveReachedServer bool
	Cause                error
}

func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return fmt.Sprintf("payment provider %s (%s)", e.Kind, e.Code)
	}
	return fmt.Sprintf("payment provider %s", e.Kind)
}

func (e *ProviderError) Unwrap() error { return e.Cause }

func NewProviderError(kind ProviderErrorKind, code string, mayHaveReachedServer bool, cause error) error {
	return &ProviderError{
		Kind:                 kind,
		Code:                 NormalizeProviderCode(code),
		MayHaveReachedServer: mayHaveReachedServer,
		Cause:                cause,
	}
}

func NormalizeProviderCode(code string) string {
	code = strings.TrimSpace(code)
	if len(code) > 64 {
		code = code[:64]
	}
	for _, character := range code {
		if !(character == '-' || character == '_' || character == '.' || character >= '0' && character <= '9' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z') {
			return "redacted"
		}
	}
	return code
}

type SetupRequest struct {
	AccountID      string
	LocalSessionID string
	ReturnURL      string
	IdempotencyKey string
	CorrelationID  string
}

type SetupResult struct {
	State               PaymentIntentState
	HostedURL           string
	ProviderCustomerRef string
	ProviderMethodRef   string
	ProviderCode        string
	RequiresUserAction  bool
	CardBrand           string
	LastFour            string
	ExpiryMonth         *int
	ExpiryYear          *int
}

type ChargeRequest struct {
	IntentID               string
	AmountMinor            int64
	Currency               Currency
	OpaqueMethodReference  string
	MerchantOrderReference string
	IdempotencyKey         string
	CorrelationID          string
}

type QueryRequest struct {
	IntentID                     string
	AmountMinor                  int64
	Currency                     Currency
	MerchantOrderReference       string
	ProviderTransactionReference string
	CorrelationID                string
}

type RefundRequest struct {
	IntentID                     string
	AmountMinor                  int64
	Currency                     Currency
	ProviderTransactionReference string
	IdempotencyKey               string
	CorrelationID                string
}

type ProviderResult struct {
	State                        PaymentIntentState
	ProviderTransactionReference string
	ProviderCode                 string
	Evidence                     map[string]string
}

type WebhookRequest struct {
	Header http.Header
	Body   []byte
}

type WebhookEvent struct {
	ProviderEventReference       string
	MerchantOrderReference       string
	ProviderTransactionReference string
	AmountMinor                  int64
	Currency                     Currency
	State                        PaymentIntentState
	EventType                    string
	ProviderCode                 string
	SafeSummary                  map[string]string
}

type PaymentProvider interface {
	Name() string
	Capabilities(context.Context) ProviderCapabilities
	CreateSetup(context.Context, SetupRequest) (SetupResult, error)
	Charge(context.Context, ChargeRequest) (ProviderResult, error)
	Query(context.Context, QueryRequest) (ProviderResult, error)
	VerifyWebhook(context.Context, WebhookRequest) (WebhookEvent, error)
	Refund(context.Context, RefundRequest) (ProviderResult, error)
}

func ValidateProviderResult(result ProviderResult) error {
	switch result.State {
	case PaymentIntentStateAuthorized, PaymentIntentStateRequiresAction,
		PaymentIntentStateUnknown, PaymentIntentStateSucceeded,
		PaymentIntentStateFailed, PaymentIntentStateCanceled:
		return nil
	default:
		return ErrInvalidState
	}
}

func StateForProviderError(err error) PaymentIntentState {
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		return PaymentIntentStateUnknown
	}
	switch providerErr.Kind {
	case ProviderErrorDeclined, ProviderErrorInvalidRequest, ProviderErrorAuthentication:
		if !providerErr.MayHaveReachedServer {
			return PaymentIntentStateFailed
		}
	case ProviderErrorRequiresAction:
		return PaymentIntentStateRequiresAction
	}
	return PaymentIntentStateUnknown
}
