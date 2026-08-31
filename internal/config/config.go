package config

import (
	"errors"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	Port                          string
	Environment                   string
	DatabaseURL                   string
	ServiceToken                  string
	InternalToken                 string
	HandoffToken                  string
	BillingDebitToken             string
	BillingDebitSource            string
	PaymentReferenceEncryptionKey string
	SimulatorEnabled              bool
	SimulatorBaseURL              string
	SimulatorSharedSecret         string
	SimulatorCallbackSecret       string
	SimulatorRunID                string
	SimulatorScenario             string
	NewebPayEnabled               bool
	NewebPayEnvironment           string
	NewebPayMerchantID            string
	NewebPayHashKey               string
	NewebPayHashIV                string
	NewebPayEndpointBaseURL       string
	NewebPayNotifyURL             string
	NewebPayReturnURL             string
	RequestTimeout                time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Port: env("PORT", "8080"), Environment: strings.ToLower(env("ENVIRONMENT", "development")), DatabaseURL: strings.TrimSpace(os.Getenv("DATABASE_URL")),
		ServiceToken:                  strings.TrimSpace(os.Getenv("BILLING_SERVICE_TOKEN")),
		InternalToken:                 strings.TrimSpace(os.Getenv("BILLING_INTERNAL_TOKEN")),
		HandoffToken:                  strings.TrimSpace(os.Getenv("BILLING_HANDOFF_TOKEN")),
		BillingDebitToken:             strings.TrimSpace(os.Getenv("BILLING_DEBIT_TOKEN")),
		BillingDebitSource:            strings.TrimSpace(os.Getenv("BILLING_DEBIT_SOURCE")),
		PaymentReferenceEncryptionKey: strings.TrimSpace(os.Getenv("PAYMENT_REFERENCE_ENCRYPTION_KEY")),
		SimulatorEnabled:              strings.EqualFold(env("PAYMENT_SIMULATOR_ENABLED", "false"), "true"),
		SimulatorBaseURL:              strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_BASE_URL")),
		SimulatorSharedSecret:         strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_SHARED_SECRET")),
		SimulatorCallbackSecret:       strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_CALLBACK_SECRET")),
		SimulatorRunID:                env("PAYMENT_SIMULATOR_RUN_ID", "local"), SimulatorScenario: env("PAYMENT_SIMULATOR_SCENARIO", "success"),
		NewebPayEnabled: strings.EqualFold(env("NEWEBPAY_ENABLED", "false"), "true"), NewebPayEnvironment: env("NEWEBPAY_ENVIRONMENT", "sandbox"),
		NewebPayMerchantID: strings.TrimSpace(os.Getenv("NEWEBPAY_MERCHANT_ID")), NewebPayHashKey: os.Getenv("NEWEBPAY_HASH_KEY"), NewebPayHashIV: os.Getenv("NEWEBPAY_HASH_IV"),
		NewebPayEndpointBaseURL: strings.TrimSpace(os.Getenv("NEWEBPAY_SIMULATOR_BASE_URL")),
		NewebPayNotifyURL:       strings.TrimSpace(os.Getenv("NEWEBPAY_NOTIFY_URL")),
		NewebPayReturnURL:       strings.TrimSpace(os.Getenv("NEWEBPAY_RETURN_URL")),
		RequestTimeout:          15 * time.Second,
	}
	if cfg.DatabaseURL == "" || len(cfg.ServiceToken) < 32 || len(cfg.InternalToken) < 32 {
		return Config{}, errors.New("DATABASE_URL plus BILLING_SERVICE_TOKEN and BILLING_INTERNAL_TOKEN of at least 32 characters are required")
	}
	if credentialReuse(cfg.ServiceToken, cfg.InternalToken, cfg.BillingDebitToken) {
		return Config{}, errors.New("billing service, internal, and debit credentials must be distinct")
	}
	if cfg.HandoffToken != "" {
		if len(cfg.HandoffToken) < 32 || strings.ContainsAny(cfg.HandoffToken, " \t\r\n") || credentialReuse(cfg.HandoffToken, cfg.ServiceToken) ||
			credentialReuse(cfg.HandoffToken, cfg.InternalToken) || credentialReuse(cfg.HandoffToken, cfg.BillingDebitToken) ||
			credentialReuse(cfg.HandoffToken, cfg.SimulatorSharedSecret) || credentialReuse(cfg.HandoffToken, cfg.SimulatorCallbackSecret) ||
			credentialReuse(cfg.HandoffToken, cfg.NewebPayHashKey) || credentialReuse(cfg.HandoffToken, cfg.PaymentReferenceEncryptionKey) {
			return Config{}, errors.New("BILLING_HANDOFF_TOKEN must contain at least 32 characters and be distinct from other service credentials")
		}
	}
	if (cfg.BillingDebitToken == "") != (cfg.BillingDebitSource == "") {
		return Config{}, errors.New("BILLING_DEBIT_TOKEN and BILLING_DEBIT_SOURCE must be configured together")
	}
	if cfg.BillingDebitToken != "" && len(cfg.BillingDebitToken) < 32 {
		return Config{}, errors.New("BILLING_DEBIT_TOKEN must contain at least 32 characters")
	}
	if cfg.SimulatorEnabled {
		if err := ValidateSimulatorEnvironment(cfg.Environment); err != nil {
			return Config{}, err
		}
		if cfg.SimulatorBaseURL == "" || len(cfg.SimulatorSharedSecret) < 32 || len(cfg.SimulatorCallbackSecret) < 32 || cfg.PaymentReferenceEncryptionKey == "" {
			return Config{}, errors.New("payment simulator requires base URL, reference encryption key, and 32-character shared and callback secrets")
		}
	}
	if cfg.NewebPayEnabled {
		if cfg.NewebPayMerchantID == "" || len(cfg.NewebPayHashKey) != 32 || len(cfg.NewebPayHashIV) != 16 {
			return Config{}, errors.New("enabled NewebPay requires MerchantID, 32-byte HashKey, and 16-byte HashIV")
		}
		if cfg.NewebPayEndpointBaseURL != "" {
			if err := ValidateSimulatorEnvironment(cfg.Environment); err != nil {
				return Config{}, err
			}
			if strings.EqualFold(cfg.NewebPayEnvironment, "production") {
				return Config{}, errors.New("NewebPay production endpoint override is forbidden")
			}
		}
		if !validPaymentURL(cfg.NewebPayNotifyURL, cfg.Environment) || !validPaymentURL(cfg.NewebPayReturnURL, cfg.Environment) {
			return Config{}, errors.New("enabled NewebPay requires fixed absolute notify and return URLs; production URLs must use HTTPS")
		}
	}
	return cfg, nil
}

func validPaymentURL(value, environment string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return !strings.EqualFold(environment, "production") && !strings.EqualFold(environment, "prod") || parsed.Scheme == "https"
}

func ValidateSimulatorEnvironment(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "development", "dev", "test", "staging":
		return nil
	case "production", "prod":
		return errors.New("payment simulator is forbidden in production")
	default:
		return errors.New("payment simulator environment must be development, test, or staging")
	}
}

func credentialReuse(values ...string) bool {
	for i, left := range values {
		left = strings.TrimSpace(left)
		if left == "" {
			continue
		}
		for _, right := range values[i+1:] {
			if left == strings.TrimSpace(right) {
				return true
			}
		}
	}
	return false
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
