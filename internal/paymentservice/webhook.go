package paymentservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

type WebhookResult struct {
	Receipt   payment.WebhookReceipt `json:"receipt"`
	Duplicate bool                   `json:"duplicate"`
	Verified  bool                   `json:"verified"`
}

func (s *Service) HandleWebhook(ctx context.Context, providerName string, header http.Header, body []byte) (WebhookResult, error) {
	providerName = payment.NormalizeProvider(providerName)
	provider := s.providers[providerName]
	if provider == nil {
		return WebhookResult{}, payment.NewProviderError(payment.ProviderErrorInvalidRequest, "provider_unavailable", false, nil)
	}
	digest := sha256.Sum256(body)
	payloadSHA256 := hex.EncodeToString(digest[:])
	receivedAt := s.now().UTC()
	event, verifyErr := provider.VerifyWebhook(ctx, payment.WebhookRequest{Header: header.Clone(), Body: append([]byte(nil), body...)})
	verification := "verified"
	if verifyErr != nil {
		verification = "rejected"
	}
	recorded, recordErr := s.store.RecordWebhook(ctx, paymentstore.RecordWebhookInput{
		Provider: providerName, PayloadSHA256: payloadSHA256,
		VerificationResult: verification, Event: event, ReceivedAt: receivedAt,
	})
	if recordErr != nil {
		return WebhookResult{}, recordErr
	}
	result := WebhookResult{
		Receipt: recorded.Receipt, Duplicate: recorded.Duplicate, Verified: verification == "verified",
	}
	if verifyErr != nil {
		var providerErr *payment.ProviderError
		if errors.As(verifyErr, &providerErr) {
			return result, providerErr
		}
		return result, payment.NewProviderError(payment.ProviderErrorAuthentication, "webhook_verification_failed", false, verifyErr)
	}
	return result, nil
}
