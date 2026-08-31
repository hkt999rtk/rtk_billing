package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"

	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentservice"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

const maxPaymentWebhookBytes = 1 << 20

type paymentPersistence interface {
	paymentservice.Store
	EnsureCommercialAccount(context.Context, string, payment.Currency) (payment.CommercialAccount, bool, error)
	GetCommercialAccountByOrganization(context.Context, string, payment.Currency) (payment.CommercialAccount, error)
	ListLedgerEntriesPage(context.Context, string, int, int) (paymentstore.LedgerEntryPage, error)
	ListPaymentMethods(context.Context, string, int, int) (paymentstore.PaymentMethodPage, error)
	GetPaymentMethod(context.Context, string, string) (payment.PaymentMethod, error)
	RevokePaymentMethod(context.Context, paymentstore.RevokePaymentMethodInput) (paymentstore.RevokePaymentMethodResult, error)
	GetAutoTopUpPolicy(context.Context, string) (payment.AutoTopUpPolicy, error)
	CreateConsent(context.Context, paymentstore.CreateConsentInput) (payment.PaymentConsent, error)
	PutAutoTopUpPolicy(context.Context, paymentstore.PutAutoTopUpPolicyInput) (payment.AutoTopUpPolicy, error)
	DisableAutoTopUpPolicy(context.Context, paymentstore.DisableAutoTopUpPolicyInput) (payment.AutoTopUpPolicy, error)
	CreateManualTopUp(context.Context, paymentstore.CreateManualTopUpInput) (paymentstore.CreateManualTopUpResult, error)
	CreateHostedTopUp(context.Context, paymentstore.CreateHostedTopUpInput) (paymentstore.CreateManualTopUpResult, error)
	ListPaymentIntents(context.Context, string, int, int) (paymentstore.PaymentIntentPage, error)
	GetPaymentIntentForAccount(context.Context, string, string) (payment.PaymentIntent, error)
	ListPaymentAttempts(context.Context, string) ([]payment.PaymentAttempt, error)
	PostLedgerEntry(context.Context, paymentstore.PostLedgerEntryInput) (paymentstore.PostLedgerEntryResult, error)
	BeginPaymentMethodSetup(context.Context, paymentstore.BeginPaymentMethodSetupInput) (paymentstore.BeginPaymentMethodSetupResult, error)
	CompletePaymentMethodSetup(context.Context, paymentstore.CompletePaymentMethodSetupInput) (paymentstore.CompletePaymentMethodSetupResult, error)
}

type PaymentReferenceProtector interface {
	EncryptMethodReference(string) ([]byte, error)
}

type PaymentAPIOptions struct {
	Store                   paymentPersistence
	Providers               []payment.PaymentProvider
	ReferenceProtector      PaymentReferenceProtector
	BillingDebitToken       string
	BillingDebitSource      string
	SimulatorCallbackSecret string
	HostedChargeNotifyURL   string
	HostedChargeReturnURL   string
	Now                     func() time.Time
}

type paymentRuntime struct {
	store                   paymentPersistence
	providers               map[string]payment.PaymentProvider
	webhooks                *paymentservice.Service
	referenceProtector      PaymentReferenceProtector
	billingDebitToken       string
	billingDebitSource      string
	simulatorCallbackSecret []byte
	hostedChargeNotifyURL   string
	hostedChargeReturnURL   string
	now                     func() time.Time
}

type unavailablePaymentReferenceResolver struct{}

func (unavailablePaymentReferenceResolver) ResolveMethodReference(context.Context, []byte) (string, error) {
	return "", errors.New("payment reference resolver unavailable in HTTP process")
}

func (s *Server) ConfigurePayments(options PaymentAPIOptions) error {
	if options.Store == nil {
		return fmt.Errorf("payment API store is required")
	}
	providers := make(map[string]payment.PaymentProvider, len(options.Providers))
	chargeEnabled := make(map[string]bool, len(options.Providers))
	for _, provider := range options.Providers {
		if provider == nil {
			return fmt.Errorf("nil payment API provider")
		}
		name := payment.NormalizeProvider(provider.Name())
		if name == "" {
			return fmt.Errorf("payment API provider name is required")
		}
		if _, exists := providers[name]; exists {
			return fmt.Errorf("duplicate payment API provider %q", name)
		}
		providers[name] = provider
		// HTTP routes never enable charge execution. The dedicated worker owns it.
		chargeEnabled[name] = false
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	options.BillingDebitToken = strings.TrimSpace(options.BillingDebitToken)
	options.BillingDebitSource = strings.TrimSpace(options.BillingDebitSource)
	options.SimulatorCallbackSecret = strings.TrimSpace(options.SimulatorCallbackSecret)
	options.HostedChargeNotifyURL = strings.TrimSpace(options.HostedChargeNotifyURL)
	options.HostedChargeReturnURL = strings.TrimSpace(options.HostedChargeReturnURL)
	if s.handoff != nil && (options.BillingDebitToken == s.handoff.token || options.SimulatorCallbackSecret == s.handoff.token) {
		return fmt.Errorf("handoff credential must be distinct from payment credentials")
	}
	if (options.BillingDebitToken == "") != (options.BillingDebitSource == "") {
		return fmt.Errorf("billing debit token and source must be configured together")
	}
	if options.BillingDebitToken != "" {
		if len(options.BillingDebitToken) < 32 {
			return fmt.Errorf("billing debit token must contain at least 32 characters")
		}
		if options.BillingDebitToken == s.serviceToken || options.BillingDebitToken == s.internalToken {
			return fmt.Errorf("billing debit token must be distinct from service and internal credentials")
		}
		if !validBillingDebitSource(options.BillingDebitSource) {
			return fmt.Errorf("billing debit source must use lowercase letters, digits, dots, underscores, or hyphens")
		}
	}
	if options.SimulatorCallbackSecret != "" && len(options.SimulatorCallbackSecret) < 32 {
		return fmt.Errorf("payment simulator callback secret must contain at least 32 characters")
	}
	webhooks, err := paymentservice.New(paymentservice.Options{
		Store: options.Store, Providers: options.Providers,
		ReferenceResolver: unavailablePaymentReferenceResolver{},
		LeaseOwner:        "payment-webhook-api", ChargeEnabled: chargeEnabled,
		Now: options.Now,
	})
	if err != nil {
		return err
	}
	s.payments = &paymentRuntime{
		store: options.Store, providers: providers, webhooks: webhooks,
		referenceProtector: options.ReferenceProtector,
		billingDebitToken:  options.BillingDebitToken, billingDebitSource: options.BillingDebitSource,
		simulatorCallbackSecret: []byte(options.SimulatorCallbackSecret),
		hostedChargeNotifyURL:   options.HostedChargeNotifyURL,
		hostedChargeReturnURL:   options.HostedChargeReturnURL,
		now:                     options.Now,
	}
	return nil
}

func validBillingDebitSource(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character == '.' || character == '_' || character == '-' ||
			character >= '0' && character <= '9' || character >= 'a' && character <= 'z' {
			continue
		}
		return false
	}
	return true
}

func (s *Server) requireBillingDebitToken(c *gin.Context) bool {
	if s.payments == nil || s.payments.billingDebitToken == "" || s.payments.billingDebitSource == "" {
		writeError(c, http.StatusServiceUnavailable, "BILLING_DEBIT_UNCONFIGURED", "Billing debit ingestion is not configured")
		return false
	}
	provided := strings.TrimSpace(c.GetHeader("Authorization"))
	expected := "Bearer " + s.payments.billingDebitToken
	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		writeError(c, http.StatusUnauthorized, "unauthorized", "Unauthorized")
		return false
	}
	return true
}

func (s *Server) paymentAccount(c *gin.Context) (payment.CommercialAccount, bool) {
	if s.payments == nil || s.payments.store == nil {
		writeError(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_NOT_CONFIGURED", "Payment service is not configured")
		return payment.CommercialAccount{}, false
	}
	account, _, err := s.payments.store.EnsureCommercialAccount(c.Request.Context(), c.Param("orgId"), payment.CurrencyTWD)
	if err != nil {
		writePaymentError(c, err)
		return payment.CommercialAccount{}, false
	}
	return account, true
}

func (s *Server) getBillingAccount(c *gin.Context) {
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	var policy any
	storedPolicy, err := s.payments.store.GetAutoTopUpPolicy(c.Request.Context(), account.ID)
	if err == nil {
		policy = autoTopUpResponse(storedPolicy, s.payments.now())
	} else if !errors.Is(err, paymentstore.ErrNotFound) {
		writePaymentError(c, err)
		return
	}
	providerNames := make([]string, 0, len(s.payments.providers))
	for name := range s.payments.providers {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)
	providers := make([]gin.H, 0, len(providerNames))
	for _, name := range providerNames {
		provider := s.payments.providers[name]
		environment := "external"
		if name == "simulator" {
			environment = "simulated"
		}
		providers = append(providers, gin.H{"name": name, "environment": environment, "capabilities": provider.Capabilities(c.Request.Context())})
	}
	c.JSON(http.StatusOK, gin.H{"account": account, "auto_topup": policy, "payment_providers": providers})
}

func (s *Server) listBillingLedger(c *gin.Context) {
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	limit, offset := pagination(c)
	page, err := s.payments.store.ListLedgerEntriesPage(c.Request.Context(), account.ID, limit, offset)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	entries := make([]gin.H, 0, len(page.Entries))
	for _, entry := range page.Entries {
		entries = append(entries, gin.H{
			"id": entry.ID, "direction": entry.Direction, "amount_minor": entry.AmountMinor,
			"currency": entry.Currency, "reason": entry.Reason,
			"balance_after_minor": entry.BalanceAfterMinor, "created_at": entry.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"ledger_entries": entries, "pagination": gin.H{"limit": limit, "offset": offset, "total": page.Total}})
}

func (s *Server) listPaymentMethods(c *gin.Context) {
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	limit, offset := pagination(c)
	page, err := s.payments.store.ListPaymentMethods(c.Request.Context(), account.ID, limit, offset)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"payment_methods": page.Methods, "pagination": gin.H{"limit": limit, "offset": offset, "total": page.Total}})
}

type paymentMethodSetupRequest struct {
	Provider string         `json:"provider" binding:"required"`
	Consent  consentRequest `json:"consent" binding:"required"`
}

func (s *Server) setupPaymentMethod(c *gin.Context) {
	var request paymentMethodSetupRequest
	if !bindPaymentStrict(c, &request) {
		return
	}
	idempotencyKey, ok := requiredIdempotencyKey(c)
	if !ok {
		return
	}
	if !request.Consent.Accepted {
		writeError(c, http.StatusBadRequest, "PAYMENT_CONSENT_REQUIRED", "Explicit payment-method consent is required")
		return
	}
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	providerName := payment.NormalizeProvider(request.Provider)
	provider := s.payments.providers[providerName]
	if provider == nil {
		writeError(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_NOT_CONFIGURED", "Payment provider is not configured")
		return
	}
	if !provider.Capabilities(c.Request.Context()).HostedSetup {
		writeError(c, http.StatusConflict, "PAYMENT_CAPABILITY_UNSUPPORTED", "Provider-hosted payment method setup is not available")
		return
	}
	if s.payments.referenceProtector == nil {
		writeError(c, http.StatusServiceUnavailable, "PAYMENT_REFERENCE_PROTECTION_UNCONFIGURED", "Payment method setup is not configured")
		return
	}
	requestSHA256, err := paymentSetupRequestSHA256(account.ID, providerName, request.Consent, paymentActorType(c), paymentActorID(c))
	if err != nil {
		writePaymentError(c, err)
		return
	}
	correlationID := paymentRequestID(c, idempotencyKey)
	begin, err := s.payments.store.BeginPaymentMethodSetup(c.Request.Context(), paymentstore.BeginPaymentMethodSetupInput{
		AccountID: account.ID, Provider: providerName, IdempotencyKey: idempotencyKey,
		RequestSHA256: requestSHA256, CorrelationID: correlationID,
		Capabilities: provider.Capabilities(c.Request.Context()),
		Consent: paymentstore.CreateConsentInput{
			AccountID: account.ID, ConsentType: "payment_method", TextVersion: request.Consent.TextVersion,
			TextSHA256: strings.ToLower(request.Consent.TextSHA256), AcceptedActorType: paymentActorType(c),
			AcceptedActorID: paymentActorID(c), Locale: request.Consent.Locale, Source: "cloud_admin_or_api",
		},
		Now: s.payments.now(),
	})
	if err != nil {
		writePaymentSetupError(c, err)
		return
	}
	setup, err := provider.CreateSetup(c.Request.Context(), payment.SetupRequest{
		AccountID: account.ID, LocalSessionID: begin.Session.ID,
		IdempotencyKey: idempotencyKey, CorrelationID: correlationID,
	})
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_UNAVAILABLE", "Payment provider setup is temporarily unavailable")
		return
	}
	if (setup.State != payment.PaymentIntentStateSucceeded && setup.State != payment.PaymentIntentStateRequiresAction) ||
		(setup.State == payment.PaymentIntentStateRequiresAction && !setup.RequiresUserAction) || !validHostedPaymentURL(setup.HostedURL) {
		writeError(c, http.StatusBadGateway, "PAYMENT_PROVIDER_RESPONSE_INVALID", "Payment provider returned an invalid setup response")
		return
	}
	hostedURLDigest := sha256.Sum256([]byte(setup.HostedURL))
	completeInput := paymentstore.CompletePaymentMethodSetupInput{
		AccountID: account.ID, SessionID: begin.Session.ID, State: setup.State,
		ProviderCode: setup.ProviderCode, HostedURLSHA256: hex.EncodeToString(hostedURLDigest[:]),
		CardBrand: setup.CardBrand, LastFour: setup.LastFour,
		ExpiryMonth: setup.ExpiryMonth, ExpiryYear: setup.ExpiryYear, Now: s.payments.now(),
	}
	if setup.State == payment.PaymentIntentStateSucceeded {
		if !validOpaqueProviderReference(setup.ProviderCustomerRef) || !validOpaqueProviderReference(setup.ProviderMethodRef) {
			writeError(c, http.StatusBadGateway, "PAYMENT_PROVIDER_RESPONSE_INVALID", "Payment provider returned an invalid setup response")
			return
		}
		completeInput.ProviderCustomerRefCiphertext, err = s.payments.referenceProtector.EncryptMethodReference(setup.ProviderCustomerRef)
		if err == nil {
			completeInput.ProviderMethodRefCiphertext, err = s.payments.referenceProtector.EncryptMethodReference(setup.ProviderMethodRef)
		}
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, "PAYMENT_REFERENCE_PROTECTION_FAILED", "Payment method setup could not be persisted safely")
			return
		}
		methodDigest := sha256.Sum256([]byte(setup.ProviderMethodRef))
		completeInput.ProviderMethodRefSHA256 = hex.EncodeToString(methodDigest[:])
	}
	// This is the validated adapter response for the original persisted session,
	// not browser input. Preserve reconciliation even if ownership changed while
	// waiting for the provider; invalidated sessions can never reactivate a method.
	completed, err := s.payments.store.CompletePaymentMethodSetup(billingidentity.ForProviderReconciliation(c.Request.Context()), completeInput)
	if err != nil {
		writePaymentSetupError(c, err)
		return
	}
	duplicate := begin.Duplicate || completed.Duplicate
	if !s.writePaymentAudit(c, "payment_method_setup_created", "payment_method", completed.Method.ID, gin.H{
		"provider": providerName, "state": completed.Method.Status, "duplicate": duplicate,
		"consent_text_version": begin.Consent.TextVersion, "consent_text_sha256": begin.Consent.TextSHA256,
	}) {
		return
	}
	if !s.revalidateOwnerResponse(c) {
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"payment_method": completed.Method, "hosted_url": setup.HostedURL, "duplicate": duplicate})
}

func paymentSetupRequestSHA256(accountID, provider string, consent consentRequest, actorType, actorID string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"account_id": accountID, "provider": provider, "consent_accepted": consent.Accepted,
		"consent_text_version": strings.TrimSpace(consent.TextVersion),
		"consent_text_sha256":  strings.ToLower(strings.TrimSpace(consent.TextSHA256)),
		"consent_locale":       strings.TrimSpace(consent.Locale), "actor_type": actorType, "actor_id": actorID,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func validHostedPaymentURL(value string) bool {
	if value != strings.TrimSpace(value) {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func validOpaqueProviderReference(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 1024
}

func writePaymentSetupError(c *gin.Context, err error) {
	if errors.Is(err, paymentstore.ErrIdempotencyConflict) {
		writeError(c, http.StatusConflict, "PAYMENT_METHOD_SETUP_CONFLICT", "Idempotency key conflicts with an existing payment method setup")
		return
	}
	writePaymentError(c, err)
}

type revokePaymentMethodRequest struct {
	Reason string `json:"reason" binding:"required,min=3,max=500"`
}

func (s *Server) revokePaymentMethod(c *gin.Context) {
	var request revokePaymentMethodRequest
	if !bindPaymentStrict(c, &request) {
		return
	}
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	result, err := s.payments.store.RevokePaymentMethod(c.Request.Context(), paymentstore.RevokePaymentMethodInput{
		AccountID: account.ID, MethodID: c.Param("methodId"), ActorID: paymentActorID(c),
		Reason: request.Reason, Now: s.payments.now(),
	})
	if err != nil {
		writePaymentError(c, err)
		return
	}
	if !s.writePaymentAudit(c, "payment_method_revoked", "payment_method", result.Method.ID, gin.H{
		"provider": result.Method.Provider, "policy_disabled": result.PolicyDisabled, "duplicate": result.Duplicate,
	}) {
		return
	}
	c.JSON(http.StatusOK, result)
}

func (s *Server) getAutoTopUpPolicy(c *gin.Context) {
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	policy, err := s.payments.store.GetAutoTopUpPolicy(c.Request.Context(), account.ID)
	if errors.Is(err, paymentstore.ErrNotFound) {
		c.JSON(http.StatusOK, gin.H{"auto_topup": nil})
		return
	}
	if err != nil {
		writePaymentError(c, err)
		return
	}
	c.Header("ETag", fmt.Sprintf("\"%d\"", policy.Version))
	c.JSON(http.StatusOK, gin.H{"auto_topup": autoTopUpResponse(policy, s.payments.now())})
}

type putAutoTopUpRequest struct {
	Enabled               bool             `json:"enabled"`
	ThresholdMinor        int64            `json:"threshold_minor" binding:"required"`
	TopUpAmountMinor      int64            `json:"top_up_amount_minor" binding:"required"`
	Currency              payment.Currency `json:"currency" binding:"required"`
	PaymentMethodID       string           `json:"payment_method_id" binding:"required"`
	DailyAttemptLimit     int              `json:"daily_attempt_limit" binding:"required"`
	DailyAmountLimitMinor int64            `json:"daily_amount_limit_minor" binding:"required"`
	CooldownSeconds       int64            `json:"cooldown_seconds" binding:"required"`
	Consent               consentRequest   `json:"consent" binding:"required"`
}

type consentRequest struct {
	Accepted    bool   `json:"accepted"`
	TextVersion string `json:"text_version" binding:"required,max=128"`
	TextSHA256  string `json:"text_sha256" binding:"required,len=64,hexadecimal"`
	Locale      string `json:"locale" binding:"required,min=2,max=35"`
}

func (s *Server) putAutoTopUpPolicy(c *gin.Context) {
	var request putAutoTopUpRequest
	if !bindPaymentStrict(c, &request) {
		return
	}
	if !request.Consent.Accepted {
		writeError(c, http.StatusBadRequest, "PAYMENT_CONSENT_REQUIRED", "Explicit automatic top-up consent is required")
		return
	}
	expectedVersion, ok := requiredVersion(c)
	if !ok {
		return
	}
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	consent, err := s.payments.store.CreateConsent(c.Request.Context(), paymentstore.CreateConsentInput{
		AccountID: account.ID, ConsentType: "auto_topup", TextVersion: request.Consent.TextVersion,
		TextSHA256: request.Consent.TextSHA256, AcceptedActorType: paymentActorType(c),
		AcceptedActorID: paymentActorID(c), AcceptedAt: s.payments.now(), Locale: request.Consent.Locale,
		Source: "cloud_admin_or_api",
	})
	if err != nil {
		writePaymentError(c, err)
		return
	}
	policy, err := s.payments.store.PutAutoTopUpPolicy(c.Request.Context(), paymentstore.PutAutoTopUpPolicyInput{
		AccountID: account.ID, Enabled: request.Enabled, ThresholdMinor: request.ThresholdMinor,
		TopUpAmountMinor: request.TopUpAmountMinor, Currency: request.Currency,
		PaymentMethodID: request.PaymentMethodID, DailyAttemptLimit: request.DailyAttemptLimit,
		DailyAmountLimitMinor: request.DailyAmountLimitMinor, CooldownSeconds: request.CooldownSeconds,
		ConsentID: consent.ID, ActorID: paymentActorID(c), ExpectedVersion: expectedVersion,
	})
	if err != nil {
		writePaymentError(c, err)
		return
	}
	if !s.writePaymentAudit(c, "auto_topup_policy_replaced", "auto_topup_policy", policy.ID, gin.H{
		"enabled": policy.Enabled, "threshold_minor": policy.ThresholdMinor,
		"top_up_amount_minor": policy.TopUpAmountMinor, "currency": policy.Currency,
		"daily_attempt_limit":      policy.DailyAttemptLimit,
		"daily_amount_limit_minor": policy.DailyAmountLimitMinor,
		"cooldown_seconds":         policy.CooldownSeconds, "generation": policy.Generation, "version": policy.Version,
		"consent_text_version": consent.TextVersion, "consent_text_sha256": consent.TextSHA256,
	}) {
		return
	}
	c.Header("ETag", fmt.Sprintf("\"%d\"", policy.Version))
	c.JSON(http.StatusOK, gin.H{"auto_topup": autoTopUpResponse(policy, s.payments.now())})
}

type disableAutoTopUpRequest struct {
	Reason string `json:"reason" binding:"required,min=3,max=500"`
}

func (s *Server) disableAutoTopUpPolicy(c *gin.Context) {
	var request disableAutoTopUpRequest
	if !bindPaymentStrict(c, &request) {
		return
	}
	expectedVersion, ok := requiredVersion(c)
	if !ok {
		return
	}
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	policy, err := s.payments.store.DisableAutoTopUpPolicy(c.Request.Context(), paymentstore.DisableAutoTopUpPolicyInput{
		AccountID: account.ID, ActorID: paymentActorID(c), ExpectedVersion: expectedVersion,
	})
	if err != nil {
		writePaymentError(c, err)
		return
	}
	if !s.writePaymentAudit(c, "auto_topup_policy_disabled", "auto_topup_policy", policy.ID, gin.H{
		"reason": strings.TrimSpace(request.Reason), "generation": policy.Generation, "version": policy.Version,
	}) {
		return
	}
	c.Header("ETag", fmt.Sprintf("\"%d\"", policy.Version))
	c.JSON(http.StatusOK, gin.H{"auto_topup": autoTopUpResponse(policy, s.payments.now())})
}

type manualTopUpRequest struct {
	AmountMinor     int64            `json:"amount_minor" binding:"required"`
	Currency        payment.Currency `json:"currency" binding:"required"`
	PaymentMethodID string           `json:"payment_method_id" binding:"required"`
}

func (s *Server) createManualTopUp(c *gin.Context) {
	var request manualTopUpRequest
	if !bindPaymentStrict(c, &request) {
		return
	}
	idempotencyKey, ok := requiredIdempotencyKey(c)
	if !ok {
		return
	}
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	result, err := s.payments.store.CreateManualTopUp(c.Request.Context(), paymentstore.CreateManualTopUpInput{
		AccountID: account.ID, AmountMinor: request.AmountMinor, Currency: request.Currency,
		PaymentMethodID: request.PaymentMethodID, IdempotencyKey: idempotencyKey,
		CorrelationID: paymentRequestID(c, idempotencyKey), Now: s.payments.now(),
	})
	if err != nil {
		writePaymentError(c, err)
		return
	}
	if !s.writePaymentAudit(c, "manual_topup_intent_created", "payment_intent", result.Intent.ID, gin.H{
		"amount_minor": result.Intent.AmountMinor, "currency": result.Intent.Currency,
		"state": result.Intent.State, "duplicate": result.Duplicate,
	}) {
		return
	}
	status := http.StatusAccepted
	if result.Duplicate {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{"payment_intent": paymentIntentResponse(result.Intent), "duplicate": result.Duplicate})
}

type hostedTopUpRequest struct {
	AmountMinor int64            `json:"amount_minor" binding:"required"`
	Currency    payment.Currency `json:"currency" binding:"required"`
	Provider    string           `json:"provider" binding:"required"`
}

func (s *Server) createHostedTopUp(c *gin.Context) {
	var request hostedTopUpRequest
	if !bindPaymentStrict(c, &request) {
		return
	}
	idempotencyKey, ok := requiredIdempotencyKey(c)
	if !ok {
		return
	}
	providerName := payment.NormalizeProvider(request.Provider)
	provider, exists := s.payments.providers[providerName]
	hosted, hostedOK := provider.(payment.HostedChargeProvider)
	if !exists || !hostedOK || !provider.Capabilities(c.Request.Context()).HostedCharge || s.payments.hostedChargeNotifyURL == "" || s.payments.hostedChargeReturnURL == "" {
		writeError(c, http.StatusConflict, "PAYMENT_CAPABILITY_UNSUPPORTED", "Hosted card checkout is unavailable")
		return
	}
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	result, err := s.payments.store.CreateHostedTopUp(c.Request.Context(), paymentstore.CreateHostedTopUpInput{
		AccountID: account.ID, Provider: providerName, AmountMinor: request.AmountMinor, Currency: request.Currency,
		IdempotencyKey: idempotencyKey, CorrelationID: paymentRequestID(c, idempotencyKey), Now: s.payments.now(),
	})
	if err != nil {
		writePaymentError(c, err)
		return
	}
	action, err := hosted.CreateHostedCharge(c.Request.Context(), payment.HostedChargeRequest{
		IntentID: result.Intent.ID, AmountMinor: result.Intent.AmountMinor, Currency: result.Intent.Currency,
		MerchantOrderReference: result.Intent.MerchantOrderReference, NotifyURL: s.payments.hostedChargeNotifyURL,
		ReturnURL: s.payments.hostedChargeReturnURL, ItemDescription: "RTK Cloud account top-up",
	})
	if err != nil {
		writePaymentError(c, err)
		return
	}
	if !validHostedChargeAction(action) {
		writeError(c, http.StatusBadGateway, "PAYMENT_PROVIDER_RESPONSE_INVALID", "Payment provider returned an invalid hosted action")
		return
	}
	if !s.writePaymentAudit(c, "hosted_topup_intent_created", "payment_intent", result.Intent.ID, gin.H{
		"amount_minor": result.Intent.AmountMinor, "currency": result.Intent.Currency, "provider": providerName, "state": result.Intent.State, "duplicate": result.Duplicate,
	}) {
		return
	}
	status := http.StatusAccepted
	if result.Duplicate {
		status = http.StatusOK
	}
	if !s.revalidateOwnerResponse(c) {
		return
	}
	c.JSON(status, gin.H{"payment_intent": paymentIntentResponse(result.Intent), "duplicate": result.Duplicate, "payment_action": gin.H{"method": "POST", "url": action.EndpointURL, "fields": action.Fields}})
}

func validHostedChargeAction(action payment.HostedChargeResult) bool {
	parsed, err := url.Parse(action.EndpointURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") || len(action.Fields) == 0 || len(action.Fields) > 20 {
		return false
	}
	for name, value := range action.Fields {
		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "_", ""), "-", ""))
		if name == "" || len(name) > 64 || len(value) > 1<<20 || strings.Contains(normalized, "cardnumber") || normalized == "pan" || strings.Contains(normalized, "cvv") || strings.Contains(normalized, "cvc") || strings.Contains(normalized, "expiry") {
			return false
		}
	}
	return true
}

func (s *Server) listPaymentIntents(c *gin.Context) {
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	limit, offset := pagination(c)
	page, err := s.payments.store.ListPaymentIntents(c.Request.Context(), account.ID, limit, offset)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	intents := make([]gin.H, 0, len(page.Intents))
	for _, intent := range page.Intents {
		intents = append(intents, paymentIntentResponse(intent))
	}
	c.JSON(http.StatusOK, gin.H{"payment_intents": intents, "pagination": gin.H{"limit": limit, "offset": offset, "total": page.Total}})
}

func (s *Server) getPaymentIntent(c *gin.Context) {
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	intent, err := s.payments.store.GetPaymentIntentForAccount(c.Request.Context(), account.ID, c.Param("intentId"))
	if err != nil {
		writePaymentError(c, err)
		return
	}
	attempts, err := s.payments.store.ListPaymentAttempts(c.Request.Context(), intent.ID)
	if err != nil {
		writePaymentError(c, err)
		return
	}
	safeAttempts := make([]gin.H, 0, len(attempts))
	for _, attempt := range attempts {
		safeAttempts = append(safeAttempts, gin.H{
			"id": attempt.ID, "operation": attempt.Operation, "attempt_number": attempt.AttemptNumber,
			"started_at": attempt.StartedAt, "completed_at": attempt.CompletedAt,
			"status": attempt.NormalizedResult, "provider_code": attempt.ProviderCode,
			"next_reconciliation_at": attempt.NextReconciliationAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"payment_intent": paymentIntentResponse(intent), "attempts": safeAttempts})
}

func (s *Server) handlePaymentWebhook(c *gin.Context) {
	if s.payments == nil || s.payments.webhooks == nil {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	provider := payment.NormalizeProvider(c.Param("provider"))
	if s.payments.providers[provider] == nil {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxPaymentWebhookBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxPaymentWebhookBytes {
		writeError(c, http.StatusBadRequest, "invalid_webhook", "Invalid webhook body")
		return
	}
	result, err := s.payments.webhooks.HandleWebhook(c.Request.Context(), provider, c.Request.Header, body)
	if err != nil {
		// Authentication and mapping failures intentionally reveal no intent facts.
		writeError(c, http.StatusUnauthorized, "invalid_webhook", "Invalid webhook")
		return
	}
	c.JSON(http.StatusOK, gin.H{"accepted": result.Verified, "duplicate": result.Duplicate})
}

type simulatorSetupCallback struct {
	AccountID           string                     `json:"account_id"`
	SetupSessionID      string                     `json:"setup_session_id"`
	State               payment.PaymentIntentState `json:"state"`
	ProviderCode        string                     `json:"provider_code"`
	HostedURL           string                     `json:"hosted_url"`
	ProviderCustomerRef string                     `json:"provider_customer_ref"`
	ProviderMethodRef   string                     `json:"provider_method_ref"`
	CardBrand           string                     `json:"card_brand"`
	LastFour            string                     `json:"last_four"`
	ExpiryMonth         *int                       `json:"expiry_month"`
	ExpiryYear          *int                       `json:"expiry_year"`
}

func (s *Server) handlePaymentSimulatorSetupCallback(c *gin.Context) {
	if s.payments == nil || len(s.payments.simulatorCallbackSecret) == 0 || s.payments.referenceProtector == nil {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 64*1024))
	if err != nil || len(body) == 0 || !validPaymentSimulatorSignature(
		s.payments.simulatorCallbackSecret, body, c.GetHeader("X-Payment-Simulator-Signature"),
	) {
		writeError(c, http.StatusUnauthorized, "invalid_callback", "Invalid callback")
		return
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var callback simulatorSetupCallback
	if err := decoder.Decode(&callback); err != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) ||
		(callback.State != payment.PaymentIntentStateSucceeded && callback.State != payment.PaymentIntentStateFailed) ||
		!validHostedPaymentURL(callback.HostedURL) {
		writeError(c, http.StatusBadRequest, "invalid_callback", "Invalid callback")
		return
	}
	hostedDigest := sha256.Sum256([]byte(callback.HostedURL))
	input := paymentstore.CompletePaymentMethodSetupInput{
		AccountID: callback.AccountID, SessionID: callback.SetupSessionID, State: callback.State,
		ProviderCode: callback.ProviderCode, HostedURLSHA256: hex.EncodeToString(hostedDigest[:]),
		CardBrand: callback.CardBrand, LastFour: callback.LastFour,
		ExpiryMonth: callback.ExpiryMonth, ExpiryYear: callback.ExpiryYear, Now: s.payments.now(),
	}
	if callback.State == payment.PaymentIntentStateSucceeded {
		if !validOpaqueProviderReference(callback.ProviderCustomerRef) || !validOpaqueProviderReference(callback.ProviderMethodRef) {
			writeError(c, http.StatusBadRequest, "invalid_callback", "Invalid callback")
			return
		}
		input.ProviderCustomerRefCiphertext, err = s.payments.referenceProtector.EncryptMethodReference(callback.ProviderCustomerRef)
		if err == nil {
			input.ProviderMethodRefCiphertext, err = s.payments.referenceProtector.EncryptMethodReference(callback.ProviderMethodRef)
		}
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, "callback_persistence_failed", "Callback could not be persisted")
			return
		}
		methodDigest := sha256.Sum256([]byte(callback.ProviderMethodRef))
		input.ProviderMethodRefSHA256 = hex.EncodeToString(methodDigest[:])
	}
	result, err := s.payments.store.CompletePaymentMethodSetup(c.Request.Context(), input)
	if errors.Is(err, paymentstore.ErrSetupInvalidated) {
		// The authenticated provider result was durably recorded without activating
		// a method. Acknowledge it so the provider need not retry forever. Browser
		// setup requests still receive the explicit unusable-setup conflict.
		c.JSON(http.StatusOK, gin.H{"accepted": true, "duplicate": result.Duplicate})
		return
	}
	if err != nil {
		writePaymentSetupError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"accepted": true, "duplicate": result.Duplicate})
}

func validPaymentSimulatorSignature(secret, body []byte, provided string) bool {
	decoded, err := hex.DecodeString(strings.TrimSpace(provided))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(decoded, mac.Sum(nil))
}

func autoTopUpResponse(policy payment.AutoTopUpPolicy, now time.Time) gin.H {
	_, resetAt := payment.DailyLimitWindow(now)
	return gin.H{
		"id": policy.ID, "enabled": policy.Enabled, "threshold_minor": policy.ThresholdMinor,
		"top_up_amount_minor": policy.TopUpAmountMinor, "currency": policy.Currency,
		"payment_method_id": policy.PaymentMethodID, "daily_attempt_limit": policy.DailyAttemptLimit,
		"daily_amount_limit_minor": policy.DailyAmountLimitMinor, "cooldown_seconds": policy.CooldownSeconds,
		"generation": policy.Generation, "version": policy.Version, "armed": policy.Armed,
		"consecutive_failure_count": policy.ConsecutiveFailureCount,
		"last_triggered_at":         policy.LastTriggeredAt, "last_succeeded_at": policy.LastSucceededAt,
		"limit_timezone": payment.DailyLimitTimezone, "limit_reset_at": resetAt, "created_at": policy.CreatedAt, "updated_at": policy.UpdatedAt,
	}
}

func paymentIntentResponse(intent payment.PaymentIntent) gin.H {
	return gin.H{
		"id": intent.ID, "amount_minor": intent.AmountMinor, "currency": intent.Currency,
		"reason": intent.Reason, "provider": intent.Provider, "payment_method_id": intent.PaymentMethodID,
		"state": intent.State, "requires_customer_action": intent.State == payment.PaymentIntentStateRequiresAction,
		"correlation_id": intent.CorrelationID, "created_at": intent.CreatedAt,
		"updated_at": intent.UpdatedAt, "completed_at": intent.CompletedAt,
	}
}

func requiredIdempotencyKey(c *gin.Context) (string, bool) {
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(c, http.StatusBadRequest, "idempotency_key_required", "A valid Idempotency-Key header is required")
		return "", false
	}
	return key, true
}

func bindPaymentStrict(c *gin.Context, destination any) bool {
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request", "Invalid payment request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(c, http.StatusBadRequest, "invalid_request", "Invalid payment request")
		return false
	}
	if err := binding.Validator.ValidateStruct(destination); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request", "Invalid payment request")
		return false
	}
	return true
}

func requiredVersion(c *gin.Context) (int64, bool) {
	value := strings.Trim(strings.TrimSpace(c.GetHeader("If-Match")), "\"")
	version, err := strconv.ParseInt(value, 10, 64)
	if err != nil || version < 0 {
		writeError(c, http.StatusPreconditionRequired, "AUTO_TOPUP_POLICY_CONFLICT", "A numeric If-Match policy version is required")
		return 0, false
	}
	return version, true
}

func paymentRequestID(c *gin.Context, fallback string) string {
	requestID := strings.TrimSpace(c.GetHeader("X-Request-Id"))
	if requestID == "" {
		requestID = fallback
	}
	if len(requestID) > 128 {
		requestID = requestID[:128]
	}
	return requestID
}

func paymentActorType(c *gin.Context) string {
	if value := strings.TrimSpace(c.GetHeader("X-Billing-Actor-Type")); value != "" {
		return value
	}
	return "service"
}

func paymentActorID(c *gin.Context) string {
	if value := strings.TrimSpace(c.GetHeader("X-Billing-Actor-ID")); value != "" {
		return value
	}
	return "unknown"
}

func (s *Server) writePaymentAudit(c *gin.Context, eventType, subjectType, subjectID string, payload map[string]any) bool {
	orgID := c.Param("orgId")
	payload["actor_type"] = paymentActorType(c)
	payload["actor_id"] = paymentActorID(c)
	payload["request_id"] = paymentRequestID(c, "payment-api")
	if err := s.audit.CreateAuditEvent(c.Request.Context(), AuditEventInput{
		EventType: eventType, OrganizationID: orgID, ActorType: paymentActorType(c), ActorID: paymentActorID(c),
		SubjectType: subjectType, SubjectID: subjectID, RequestID: paymentRequestID(c, "payment-api"), Payload: payload,
	}); err != nil {
		writeError(c, http.StatusInternalServerError, "BILLING_AUDIT_FAILED", "Billing audit could not be persisted")
		return false
	}
	return true
}

func writePaymentError(c *gin.Context, err error) {
	if writeOwnershipError(c, err) {
		return
	}
	switch {
	case errors.Is(err, paymentstore.ErrHandoffFenced):
		writeError(c, http.StatusConflict, "BILLING_OWNERSHIP_HANDOFF_FENCED", "Payment changes are paused during ownership handoff")
	case errors.Is(err, paymentstore.ErrSetupInvalidated):
		writeError(c, http.StatusConflict, "PAYMENT_SETUP_INVALIDATED", "Payment setup is no longer usable")
	case errors.Is(err, paymentstore.ErrNotFound):
		writeError(c, http.StatusNotFound, "not_found", "Payment resource not found")
	case errors.Is(err, payment.ErrInvalidAmount):
		writeError(c, http.StatusBadRequest, "PAYMENT_AMOUNT_INVALID", "Payment amount is invalid")
	case errors.Is(err, payment.ErrInvalidCurrency):
		writeError(c, http.StatusBadRequest, "PAYMENT_CURRENCY_UNSUPPORTED", "Payment currency is unsupported")
	case errors.Is(err, payment.ErrPaymentMethodInactive):
		writeError(c, http.StatusConflict, "PAYMENT_METHOD_INACTIVE", "Payment method is inactive")
	case errors.Is(err, payment.ErrCapabilityUnsupported), errors.Is(err, payment.ErrProviderUnsupported):
		writeError(c, http.StatusConflict, "PAYMENT_CAPABILITY_UNSUPPORTED", "Payment capability is unsupported")
	case errors.Is(err, paymentstore.ErrIdempotencyConflict):
		writeError(c, http.StatusConflict, "PAYMENT_INTENT_CONFLICT", "Idempotency key conflicts with an existing request")
	case errors.Is(err, paymentstore.ErrAccountClosed):
		writeError(c, http.StatusConflict, "BILLING_ACCOUNT_SUSPENDED", "Billing account cannot create payments")
	case errors.Is(err, paymentstore.ErrConflict), errors.Is(err, payment.ErrInvalidPolicy):
		writeError(c, http.StatusConflict, "AUTO_TOPUP_POLICY_CONFLICT", "Payment request conflicts with current state")
	default:
		writeError(c, http.StatusInternalServerError, "PAYMENT_INTERNAL_ERROR", "Payment operation failed")
	}
}
