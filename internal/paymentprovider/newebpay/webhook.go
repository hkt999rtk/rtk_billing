package newebpay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func (a *Adapter) VerifyWebhook(_ context.Context, request payment.WebhookRequest) (payment.WebhookEvent, error) {
	if !a.enabled {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorUnsupported, "provider_disabled", false, payment.ErrProviderUnsupported)
	}
	if len(request.Body) == 0 || len(request.Body) > maxResponseBytes {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_webhook_size", false, nil)
	}
	outer, err := url.ParseQuery(string(request.Body))
	if err != nil {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_webhook_form", false, err)
	}
	tradeInfo := outer.Get("TradeInfo")
	if outer.Get("MerchantID") != a.merchantID || tradeInfo == "" || !VerifyDigest(TradeSHA(tradeInfo, a.hashKey, a.hashIV), outer.Get("TradeSha")) {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorAuthentication, "invalid_trade_sha", false, nil)
	}
	decrypted, err := DecryptTradeInfo(tradeInfo, a.hashKey, a.hashIV)
	if err != nil {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorAuthentication, "invalid_trade_info", false, err)
	}
	fields, innerStatus, err := parseWebhookTradeInfo(decrypted)
	if err != nil {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "invalid_trade_payload", false, err)
	}
	merchantOrderReference := fields.Get("MerchantOrderNo")
	tradeNumber := fields.Get("TradeNo")
	amountNTD, amountErr := strconv.ParseInt(fields.Get("Amt"), 10, 64)
	if fields.Get("MerchantID") != a.merchantID || !merchantOrderPattern.MatchString(merchantOrderReference) ||
		tradeNumber == "" || amountErr != nil || amountNTD <= 0 || amountNTD > 9_999_999_999 {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "webhook_result_mismatch", false, nil)
	}
	outerStatus := strings.TrimSpace(outer.Get("Status"))
	if innerStatus == "" || outerStatus == "" || innerStatus != outerStatus {
		return payment.WebhookEvent{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "webhook_status_mismatch", false, nil)
	}
	state := payment.PaymentIntentStateFailed
	eventType := "payment.failed"
	if innerStatus == "SUCCESS" {
		state = payment.PaymentIntentStateSucceeded
		eventType = "payment.succeeded"
	}
	providerCode := fields.Get("RespondCode")
	if providerCode == "" {
		providerCode = innerStatus
	}
	return payment.WebhookEvent{
		ProviderEventReference:       fmt.Sprintf("%s:%s", tradeNumber, innerStatus),
		MerchantOrderReference:       merchantOrderReference,
		ProviderTransactionReference: tradeNumber,
		AmountMinor:                  amountNTD,
		Currency:                     payment.CurrencyTWD,
		State:                        state,
		EventType:                    eventType,
		ProviderCode:                 payment.NormalizeProviderCode(providerCode),
		SafeSummary: map[string]string{
			"payment_type": payment.NormalizeProviderCode(fields.Get("PaymentType")),
		},
	}, nil
}

type webhookJSONPayload struct {
	Status string            `json:"Status"`
	Result webhookJSONResult `json:"Result"`
}

type webhookJSONResult struct {
	MerchantID      string        `json:"MerchantID"`
	Amt             numericString `json:"Amt"`
	TradeNo         string        `json:"TradeNo"`
	MerchantOrderNo string        `json:"MerchantOrderNo"`
	PaymentType     string        `json:"PaymentType"`
	RespondCode     string        `json:"RespondCode"`
}

func parseWebhookTradeInfo(decrypted string) (url.Values, string, error) {
	decrypted = strings.TrimSpace(decrypted)
	if strings.HasPrefix(decrypted, "{") {
		var payload webhookJSONPayload
		if err := json.Unmarshal([]byte(decrypted), &payload); err != nil {
			return nil, "", err
		}
		fields := url.Values{
			"MerchantID":      []string{payload.Result.MerchantID},
			"Amt":             []string{string(payload.Result.Amt)},
			"TradeNo":         []string{payload.Result.TradeNo},
			"MerchantOrderNo": []string{payload.Result.MerchantOrderNo},
			"PaymentType":     []string{payload.Result.PaymentType},
			"RespondCode":     []string{payload.Result.RespondCode},
		}
		return fields, strings.TrimSpace(payload.Status), nil
	}
	fields, err := url.ParseQuery(decrypted)
	if err != nil {
		return nil, "", err
	}
	return fields, strings.TrimSpace(fields.Get("Status")), nil
}
