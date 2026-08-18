package paymentservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

type Store interface {
	ClaimPaymentJobs(context.Context, time.Time, time.Time, string, int) ([]payment.ReconciliationJob, error)
	BeginProviderAttempt(context.Context, paymentstore.BeginProviderAttemptInput) (paymentstore.ProviderAttemptWork, error)
	SetAttemptRequestDigest(context.Context, string, string) error
	FinalizeProviderAttempt(context.Context, paymentstore.FinalizeProviderAttemptInput) (paymentstore.FinalizeProviderAttemptResult, error)
	CompletePaymentJob(context.Context, string, string) error
	RetryPaymentJob(context.Context, string, string, time.Time, string) error
	GetPaymentIntent(context.Context, string) (payment.PaymentIntent, error)
	RecordWebhook(context.Context, paymentstore.RecordWebhookInput) (paymentstore.RecordWebhookResult, error)
}

type ReferenceResolver interface {
	ResolveMethodReference(context.Context, []byte) (string, error)
}

type Options struct {
	Store               Store
	Providers           []payment.PaymentProvider
	ReferenceResolver   ReferenceResolver
	LeaseOwner          string
	LeaseDuration       time.Duration
	ReconciliationDelay time.Duration
	BatchSize           int
	ChargeEnabled       map[string]bool
	Now                 func() time.Time
}

type Service struct {
	store               Store
	providers           map[string]payment.PaymentProvider
	resolver            ReferenceResolver
	leaseOwner          string
	leaseDuration       time.Duration
	reconciliationDelay time.Duration
	batchSize           int
	chargeEnabled       map[string]bool
	now                 func() time.Time
}

func New(options Options) (*Service, error) {
	if options.Store == nil || options.ReferenceResolver == nil || strings.TrimSpace(options.LeaseOwner) == "" {
		return nil, fmt.Errorf("payment service store, reference resolver, and lease owner are required")
	}
	providers := make(map[string]payment.PaymentProvider, len(options.Providers))
	for _, provider := range options.Providers {
		if provider == nil {
			return nil, fmt.Errorf("nil payment provider")
		}
		name := payment.NormalizeProvider(provider.Name())
		if name == "" {
			return nil, fmt.Errorf("payment provider name is required")
		}
		if _, exists := providers[name]; exists {
			return nil, fmt.Errorf("duplicate payment provider %q", name)
		}
		providers[name] = provider
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = 30 * time.Second
	}
	if options.ReconciliationDelay <= 0 {
		options.ReconciliationDelay = time.Minute
	}
	if options.BatchSize <= 0 || options.BatchSize > 100 {
		options.BatchSize = 20
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	chargeEnabled := make(map[string]bool, len(options.ChargeEnabled))
	for provider, enabled := range options.ChargeEnabled {
		chargeEnabled[payment.NormalizeProvider(provider)] = enabled
	}
	return &Service{
		store: options.Store, providers: providers, resolver: options.ReferenceResolver,
		leaseOwner: strings.TrimSpace(options.LeaseOwner), leaseDuration: options.LeaseDuration,
		reconciliationDelay: options.ReconciliationDelay, batchSize: options.BatchSize,
		chargeEnabled: chargeEnabled, now: options.Now,
	}, nil
}

func (s *Service) RunOnce(ctx context.Context) (int, error) {
	now := s.now().UTC()
	jobs, err := s.store.ClaimPaymentJobs(ctx, now, now.Add(-s.leaseDuration), s.leaseOwner, s.batchSize)
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, job := range jobs {
		if err := s.ProcessJob(ctx, job); err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

func (s *Service) Run(ctx context.Context, pollInterval time.Duration) error {
	if pollInterval <= 0 {
		return fmt.Errorf("payment worker poll interval must be positive")
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		processed, err := s.RunOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if processed > 0 {
			continue
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

func (s *Service) ProcessJob(ctx context.Context, job payment.ReconciliationJob) error {
	operation := payment.ProviderOperationQuery
	if job.Reason == payment.ReconciliationReasonCharge {
		operation = payment.ProviderOperationCharge
	}
	now := s.now().UTC()
	work, err := s.store.BeginProviderAttempt(ctx, paymentstore.BeginProviderAttemptInput{
		JobID: job.ID, LeaseOwner: s.leaseOwner, Operation: operation, Now: now,
	})
	if errors.Is(err, paymentstore.ErrJobNotActionable) {
		return s.store.CompletePaymentJob(ctx, job.ID, s.leaseOwner)
	}
	if err != nil {
		return err
	}
	if work.RecoverIncompleteAttempt {
		if work.Attempt.RequestSHA256 == "" {
			if err := s.setRequestDigest(ctx, work.Attempt.ID, map[string]any{
				"operation": work.Attempt.Operation, "intent_id": work.Intent.ID,
				"recovered_before_external_call": true,
			}); err != nil {
				return err
			}
		}
		return s.finalize(ctx, work, payment.ProviderResult{
			State: payment.PaymentIntentStateUnknown, ProviderCode: "recovered_incomplete_attempt",
		})
	}

	provider := s.providers[payment.NormalizeProvider(work.Intent.Provider)]
	if provider == nil {
		return s.finalize(ctx, work, payment.ProviderResult{
			State: payment.PaymentIntentStateFailed, ProviderCode: "provider_unavailable",
		})
	}

	var result payment.ProviderResult
	if operation == payment.ProviderOperationCharge {
		result, err = s.charge(ctx, provider, work)
	} else {
		result, err = s.query(ctx, provider, work)
	}
	if err != nil {
		result = payment.ProviderResult{
			State:        payment.StateForProviderError(err),
			ProviderCode: providerErrorCode(err),
		}
	}
	return s.finalize(ctx, work, result)
}

func (s *Service) charge(ctx context.Context, provider payment.PaymentProvider, work paymentstore.ProviderAttemptWork) (payment.ProviderResult, error) {
	if !s.chargeEnabled[payment.NormalizeProvider(provider.Name())] {
		result := payment.ProviderResult{State: payment.PaymentIntentStateFailed, ProviderCode: "provider_disabled"}
		if err := s.setRequestDigest(ctx, work.Attempt.ID, map[string]any{
			"operation": "charge", "intent_id": work.Intent.ID, "disabled": true,
		}); err != nil {
			return payment.ProviderResult{}, err
		}
		return result, nil
	}
	methodReference, err := s.resolver.ResolveMethodReference(ctx, work.ProviderMethodRefCiphertext)
	if err != nil || strings.TrimSpace(methodReference) == "" {
		return payment.ProviderResult{}, payment.NewProviderError(payment.ProviderErrorAuthentication, "method_reference_unavailable", false, err)
	}
	request := payment.ChargeRequest{
		IntentID:               work.Intent.ID,
		AmountMinor:            work.Intent.AmountMinor,
		Currency:               work.Intent.Currency,
		OpaqueMethodReference:  methodReference,
		MerchantOrderReference: work.Intent.MerchantOrderReference,
		IdempotencyKey:         work.Intent.IdempotencyKey,
		CorrelationID:          work.Intent.CorrelationID,
	}
	methodDigest := sha256.Sum256([]byte(methodReference))
	if err := s.setRequestDigest(ctx, work.Attempt.ID, map[string]any{
		"operation": "charge", "intent_id": request.IntentID, "amount_minor": request.AmountMinor,
		"currency": request.Currency, "merchant_order_reference": request.MerchantOrderReference,
		"idempotency_key": request.IdempotencyKey, "correlation_id": request.CorrelationID,
		"method_reference_sha256": hex.EncodeToString(methodDigest[:]),
	}); err != nil {
		return payment.ProviderResult{}, err
	}
	return provider.Charge(ctx, request)
}

func (s *Service) query(ctx context.Context, provider payment.PaymentProvider, work paymentstore.ProviderAttemptWork) (payment.ProviderResult, error) {
	request := payment.QueryRequest{
		IntentID: work.Intent.ID, AmountMinor: work.Intent.AmountMinor, Currency: work.Intent.Currency,
		MerchantOrderReference:       work.Intent.MerchantOrderReference,
		ProviderTransactionReference: work.Intent.ProviderTransactionReference,
		CorrelationID:                work.Intent.CorrelationID,
	}
	if err := s.setRequestDigest(ctx, work.Attempt.ID, map[string]any{
		"operation": "query", "intent_id": request.IntentID,
		"amount_minor": request.AmountMinor, "currency": request.Currency,
		"merchant_order_reference":       request.MerchantOrderReference,
		"provider_transaction_reference": request.ProviderTransactionReference,
		"correlation_id":                 request.CorrelationID,
	}); err != nil {
		return payment.ProviderResult{}, err
	}
	return provider.Query(ctx, request)
}

func (s *Service) setRequestDigest(ctx context.Context, attemptID string, safeRequest any) error {
	digest, err := digestJSON(safeRequest)
	if err != nil {
		return err
	}
	return s.store.SetAttemptRequestDigest(ctx, attemptID, digest)
}

func (s *Service) finalize(ctx context.Context, work paymentstore.ProviderAttemptWork, result payment.ProviderResult) error {
	if err := payment.ValidateProviderResult(result); err != nil {
		result = payment.ProviderResult{State: payment.PaymentIntentStateUnknown, ProviderCode: "invalid_provider_result"}
	}
	responseDigest, err := digestJSON(map[string]any{
		"state":                          result.State,
		"provider_transaction_reference": result.ProviderTransactionReference,
		"provider_code":                  payment.NormalizeProviderCode(result.ProviderCode),
	})
	if err != nil {
		return err
	}
	now := s.now().UTC()
	_, err = s.store.FinalizeProviderAttempt(ctx, paymentstore.FinalizeProviderAttemptInput{
		JobID: work.Job.ID, LeaseOwner: s.leaseOwner, AttemptID: work.Attempt.ID,
		Result: result, ResponseSHA256: responseDigest, Now: now,
		NextReconciliationAt: now.Add(s.reconciliationDelay),
	})
	return err
}

func providerErrorCode(err error) string {
	var providerErr *payment.ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.Code != "" {
			return providerErr.Code
		}
		return string(providerErr.Kind)
	}
	return "adapter_error"
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
