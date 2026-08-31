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
	t.Setenv("BILLING_HANDOFF_TOKEN", "")
	t.Setenv("BILLING_DEBIT_TOKEN", strings.Repeat("d", 32))
	t.Setenv("BILLING_DEBIT_SOURCE", "pricing-service")
	t.Setenv("ENVIRONMENT", "staging")
	t.Setenv("PAYMENT_SIMULATOR_ENABLED", "true")
	t.Setenv("PAYMENT_SIMULATOR_BASE_URL", "http://payment-simulator.invalid")
	t.Setenv("PAYMENT_SIMULATOR_SHARED_SECRET", strings.Repeat("h", 32))
	t.Setenv("PAYMENT_SIMULATOR_CALLBACK_SECRET", strings.Repeat("c", 32))
	t.Setenv("PAYMENT_REFERENCE_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

func TestHandoffCredentialIsOptionalButCannotReuseOtherBoundaries(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("NEWEBPAY_HASH_KEY", strings.Repeat("k", 32))
	for _, token := range []string{"short", strings.Repeat("o", 32) + " injected", strings.Repeat("s", 32), strings.Repeat("i", 32), strings.Repeat("d", 32), strings.Repeat("h", 32), strings.Repeat("c", 32), strings.Repeat("k", 32), base64.StdEncoding.EncodeToString(make([]byte, 32))} {
		t.Setenv("BILLING_HANDOFF_TOKEN", token)
		if _, err := Load(); err == nil {
			t.Fatal("weak or reused handoff credential accepted")
		}
	}
	t.Setenv("BILLING_HANDOFF_TOKEN", strings.Repeat("o", 32))
	if cfg, err := Load(); err != nil || cfg.HandoffToken != strings.Repeat("o", 32) {
		t.Fatalf("valid optional handoff credential: %v", err)
	}
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

func TestLoadNewebPayRequiresFixedCheckoutCallbacks(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("NEWEBPAY_ENABLED", "true")
	t.Setenv("NEWEBPAY_MERCHANT_ID", "MS127874575")
	t.Setenv("NEWEBPAY_HASH_KEY", strings.Repeat("k", 32))
	t.Setenv("NEWEBPAY_HASH_IV", strings.Repeat("i", 16))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "notify and return URLs") {
		t.Fatalf("missing callbacks err=%v", err)
	}
	t.Setenv("NEWEBPAY_NOTIFY_URL", "https://billing.example/v1/payment-webhooks/newebpay")
	t.Setenv("NEWEBPAY_RETURN_URL", "https://admin.example/console/billing/activity")
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
