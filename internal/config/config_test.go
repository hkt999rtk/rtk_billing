package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func setValidEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://billing.invalid/rtk_billing")
	t.Setenv("BILLING_SERVICE_TOKEN", strings.Repeat("s", 32))
	t.Setenv("BILLING_INTERNAL_TOKEN", strings.Repeat("i", 32))
	t.Setenv("BILLING_DEBIT_TOKEN", strings.Repeat("d", 32))
	t.Setenv("BILLING_DEBIT_SOURCE", "pricing-service")
	t.Setenv("ENVIRONMENT", "staging")
	t.Setenv("PAYMENT_SIMULATOR_ENABLED", "true")
	t.Setenv("PAYMENT_SIMULATOR_BASE_URL", "http://payment-simulator.invalid")
	t.Setenv("PAYMENT_SIMULATOR_SHARED_SECRET", strings.Repeat("h", 32))
	t.Setenv("PAYMENT_SIMULATOR_CALLBACK_SECRET", strings.Repeat("c", 32))
	t.Setenv("PAYMENT_REFERENCE_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

func TestLoadRequiresDistinctCredentialBoundaries(t *testing.T) {
	setValidEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceToken == cfg.InternalToken || cfg.ServiceToken == cfg.BillingDebitToken || cfg.InternalToken == cfg.BillingDebitToken {
		t.Fatal("credential boundaries collapsed")
	}

	t.Setenv("BILLING_DEBIT_TOKEN", strings.Repeat("s", 32))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("credential reuse error = %v", err)
	}
}

func TestLoadSimulatorFailsClosedInProductionOrWithoutProtection(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("ENVIRONMENT", "production")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "forbidden in production") {
		t.Fatalf("production simulator error = %v", err)
	}

	t.Setenv("ENVIRONMENT", "staging")
	t.Setenv("PAYMENT_REFERENCE_ENCRYPTION_KEY", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "reference encryption key") {
		t.Fatalf("missing reference protection error = %v", err)
	}
}

func TestLoadAllowsProviderDisabledServiceWithOptionalDebitBoundary(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("PAYMENT_SIMULATOR_ENABLED", "false")
	t.Setenv("PAYMENT_SIMULATOR_BASE_URL", "")
	t.Setenv("PAYMENT_SIMULATOR_SHARED_SECRET", "")
	t.Setenv("PAYMENT_SIMULATOR_CALLBACK_SECRET", "")
	t.Setenv("PAYMENT_REFERENCE_ENCRYPTION_KEY", "")
	t.Setenv("BILLING_DEBIT_TOKEN", "")
	t.Setenv("BILLING_DEBIT_SOURCE", "")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSimulatorEnvironmentRejectsUnknownValues(t *testing.T) {
	for _, environment := range []string{"development", "dev", "test", "staging"} {
		if err := ValidateSimulatorEnvironment(environment); err != nil {
			t.Fatalf("%s: %v", environment, err)
		}
	}
	for _, environment := range []string{"", "production", "prod", "qa"} {
		if err := ValidateSimulatorEnvironment(environment); err == nil {
			t.Fatalf("environment %q passed", environment)
		}
	}
}
