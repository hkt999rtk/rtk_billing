package config

import (
	"errors"
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
	BillingDebitToken             string
	BillingDebitSource            string
	PaymentReferenceEncryptionKey string
	SimulatorEnabled              bool
	SimulatorBaseURL              string
	SimulatorSharedSecret         string
	SimulatorCallbackSecret       string
	SimulatorRunID                string
	SimulatorScenario             string
	RequestTimeout                time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Port: env("PORT", "8080"), Environment: strings.ToLower(env("ENVIRONMENT", "development")), DatabaseURL: strings.TrimSpace(os.Getenv("DATABASE_URL")),
		ServiceToken:                  strings.TrimSpace(os.Getenv("BILLING_SERVICE_TOKEN")),
		InternalToken:                 strings.TrimSpace(os.Getenv("BILLING_INTERNAL_TOKEN")),
		BillingDebitToken:             strings.TrimSpace(os.Getenv("BILLING_DEBIT_TOKEN")),
		BillingDebitSource:            strings.TrimSpace(os.Getenv("BILLING_DEBIT_SOURCE")),
		PaymentReferenceEncryptionKey: strings.TrimSpace(os.Getenv("PAYMENT_REFERENCE_ENCRYPTION_KEY")),
		SimulatorEnabled:              strings.EqualFold(env("PAYMENT_SIMULATOR_ENABLED", "false"), "true"),
		SimulatorBaseURL:              strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_BASE_URL")),
		SimulatorSharedSecret:         strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_SHARED_SECRET")),
		SimulatorCallbackSecret:       strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_CALLBACK_SECRET")),
		SimulatorRunID:                env("PAYMENT_SIMULATOR_RUN_ID", "local"), SimulatorScenario: env("PAYMENT_SIMULATOR_SCENARIO", "success"),
		RequestTimeout: 15 * time.Second,
	}
	if cfg.DatabaseURL == "" || len(cfg.ServiceToken) < 32 || len(cfg.InternalToken) < 32 {
		return Config{}, errors.New("DATABASE_URL plus BILLING_SERVICE_TOKEN and BILLING_INTERNAL_TOKEN of at least 32 characters are required")
	}
	if credentialReuse(cfg.ServiceToken, cfg.InternalToken, cfg.BillingDebitToken) {
		return Config{}, errors.New("billing service, internal, and debit credentials must be distinct")
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
	return cfg, nil
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
