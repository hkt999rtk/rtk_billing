package config

import (
	"errors"
	"os"
	"strings"
	"time"
)

type Config struct {
	Port                          string
	DatabaseURL                   string
	ServiceToken                  string
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
		Port: env("PORT", "8080"), DatabaseURL: strings.TrimSpace(os.Getenv("DATABASE_URL")),
		ServiceToken:                  strings.TrimSpace(os.Getenv("BILLING_SERVICE_TOKEN")),
		PaymentReferenceEncryptionKey: strings.TrimSpace(os.Getenv("PAYMENT_REFERENCE_ENCRYPTION_KEY")),
		SimulatorEnabled:              strings.EqualFold(env("PAYMENT_SIMULATOR_ENABLED", "false"), "true"),
		SimulatorBaseURL:              strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_BASE_URL")),
		SimulatorSharedSecret:         strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_SHARED_SECRET")),
		SimulatorCallbackSecret:       strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_CALLBACK_SECRET")),
		SimulatorRunID:                env("PAYMENT_SIMULATOR_RUN_ID", "local"), SimulatorScenario: env("PAYMENT_SIMULATOR_SCENARIO", "success"),
		RequestTimeout: 15 * time.Second,
	}
	if cfg.DatabaseURL == "" || len(cfg.ServiceToken) < 32 {
		return Config{}, errors.New("DATABASE_URL and a BILLING_SERVICE_TOKEN of at least 32 characters are required")
	}
	if cfg.SimulatorEnabled && (cfg.SimulatorBaseURL == "" || len(cfg.SimulatorSharedSecret) < 32 || len(cfg.SimulatorCallbackSecret) < 32) {
		return Config{}, errors.New("payment simulator requires base URL and 32-character shared and callback secrets")
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
