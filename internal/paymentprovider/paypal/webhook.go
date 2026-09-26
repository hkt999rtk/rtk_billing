package paypal

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func (a *Adapter) VerifyWebhook(ctx context.Context, request payment.WebhookRequest) (payment.WebhookEvent, error) {
	if len(request.Body) == 0 || len(request.Body) > maxResponseBytes {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_webhook_size", false, nil)
	}
	var envelope struct {
		ID        string  `json:"id"`
		EventType string  `json:"event_type"`
		Resource  capture `json:"resource"`
	}
	if json.Unmarshal(request.Body, &envelope) != nil || envelope.ID == "" {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_webhook", false, nil)
	}
	if envelope.EventType != "PAYMENT.CAPTURE.COMPLETED" {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorUnsupported, "unsupported_webhook_event", false, nil)
	}
	orderID := envelope.Resource.SupplementaryData.RelatedIDs.OrderID
	if !paypalID.MatchString(orderID) || envelope.Resource.Status != "COMPLETED" || envelope.Resource.Amount.CurrencyCode != "TWD" {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "webhook_capture_mismatch", false, nil)
	}
	minor, validAmount := paypalTWDWholeAmount(envelope.Resource.Amount.Value)
	if !validAmount || envelope.Resource.ID == "" {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "webhook_amount_invalid", false, nil)
	}
	for _, name := range []string{"Paypal-Transmission-Id", "Paypal-Transmission-Time", "Paypal-Cert-Url", "Paypal-Auth-Algo", "Paypal-Transmission-Sig"} {
		if strings.TrimSpace(request.Header.Get(name)) == "" {
			return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorAuthentication, "webhook_header_missing", false, nil)
		}
	}
	verify := map[string]any{
		"transmission_id":   request.Header.Get("Paypal-Transmission-Id"),
		"transmission_time": request.Header.Get("Paypal-Transmission-Time"),
		"cert_url":          request.Header.Get("Paypal-Cert-Url"),
		"auth_algo":         request.Header.Get("Paypal-Auth-Algo"),
		"transmission_sig":  request.Header.Get("Paypal-Transmission-Sig"),
		"webhook_id":        a.webhookID,
		"webhook_event":     json.RawMessage(request.Body),
	}
	var result struct {
		VerificationStatus string `json:"verification_status"`
	}
	if err := a.call(ctx, http.MethodPost, "/v1/notifications/verify-webhook-signature", "", verify, &result); err != nil {
		return payment.WebhookEvent{}, err
	}
	if result.VerificationStatus != "SUCCESS" {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorAuthentication, "invalid_webhook_signature", false, nil)
	}
	canonical, err := a.getOrder(ctx, orderID)
	if err != nil {
		return payment.WebhookEvent{}, err
	}
	if canonical.ID != orderID || canonical.Status != "COMPLETED" || len(canonical.PurchaseUnits) != 1 {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorUnknown, "webhook_order_mismatch", true, nil)
	}
	unit := canonical.PurchaseUnits[0]
	if !orderReference.MatchString(unit.CustomID) || !matchesUnit(unit, unit.CustomID, minor, payment.CurrencyTWD) ||
		len(unit.Payments.Captures) != 1 || unit.Payments.Captures[0].ID != envelope.Resource.ID ||
		unit.Payments.Captures[0].Status != "COMPLETED" ||
		unit.Payments.Captures[0].Amount.CurrencyCode != envelope.Resource.Amount.CurrencyCode ||
		!paypalAmountMatches(unit.Payments.Captures[0].Amount.Value, minor) ||
		(unit.Payments.Captures[0].CustomID != "" && unit.Payments.Captures[0].CustomID != unit.CustomID) ||
		(envelope.Resource.CustomID != "" && envelope.Resource.CustomID != unit.CustomID) {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorUnknown, "webhook_capture_mismatch", true, nil)
	}
	return payment.WebhookEvent{
		ProviderEventReference: envelope.ID,
		MerchantOrderReference: unit.CustomID,
		AmountMinor:            minor,
		Currency:               payment.CurrencyTWD,
		State:                  payment.PaymentIntentStateSucceeded,
		EventType:              "payment.succeeded",
		ProviderCode:           "completed",
	}, nil
}
