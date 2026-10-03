package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func setRawRetentionEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("BILLING_RAW_RETENTION_ENABLED", "true")
	for key, char := range map[string]string{"FINANCIAL_TOKEN": "f", "RECOVERY_TOKEN": "r", "CONTROLLER_TOKEN": "x", "AUTHORITY_READ_TOKEN": "a", "LOGGER_TOKEN": "l", "CONSUMER_TOKEN": "v"} {
		t.Setenv("BILLING_RAW_RETENTION_"+key, strings.Repeat(char, 32))
	}
	t.Setenv("BILLING_RAW_RETENTION_LOGGER_BASE_URL", "https://logger.example")
	t.Setenv("BILLING_RAW_RETENTION_CONSUMER_BASE_URL", "https://meter.example")
	t.Setenv("BILLING_RAW_RETENTION_CONSUMER_ID", "video-cloud-mqttusage/staging")
	t.Setenv("BILLING_RAW_RETENTION_VERIFIER_KEY_ID", "verifier-1")
	t.Setenv("BILLING_RAW_RETENTION_VERIFIER_PUBLIC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

func TestRawRetentionDisabledByDefaultAndCompleteConfigurationRequired(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("BILLING_RAW_RETENTION_ENABLED", "")
	if cfg, err := Load(); err != nil || cfg.RawRetention.Enabled {
		t.Fatalf("unexpected default raw retention: %v", err)
	}
	t.Setenv("BILLING_RAW_RETENTION_ENABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("incomplete authority enabled")
	}
	setRawRetentionEnvironment(t)
	if cfg, err := Load(); err != nil || !cfg.RawRetention.Enabled || cfg.RawRetention.ListenAddr != ":8081" {
		t.Fatalf("valid raw authority rejected: %v", err)
	}
	for _, tc := range []struct{ key, value string }{{"AUTHORITY_READ_TOKEN", strings.Repeat("x", 32)}, {"CONTROLLER_TOKEN", strings.Repeat("i", 32)}, {"LOGGER_BASE_URL", "http://logger.example"}, {"VERIFIER_PUBLIC_KEY", "not-base64"}} {
		t.Run(tc.key, func(t *testing.T) {
			setRawRetentionEnvironment(t)
			t.Setenv("BILLING_RAW_RETENTION_"+tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestRawRetentionListenerAddressMustRemainSeparate(t *testing.T) {
	setValidEnvironment(t)
	setRawRetentionEnvironment(t)
	for _, addr := range []string{":0", ":65536", "example.test:8081", "not-an-address", ":8080"} {
		t.Setenv("BILLING_RAW_RETENTION_LISTEN_ADDR", addr)
		t.Setenv("PORT", "8080")
		if _, err := Load(); err == nil {
			t.Fatalf("invalid or shared retention listener accepted: %s", addr)
		}
	}
	t.Setenv("BILLING_RAW_RETENTION_LISTEN_ADDR", "127.0.0.1:8082")
	if cfg, err := Load(); err != nil || cfg.RawRetention.ListenAddr != "127.0.0.1:8082" {
		t.Fatalf("dedicated listener rejected: %v", err)
	}
}

func TestRawRetentionScopeOverrideDoesNotChangeLegacyEnvironment(t *testing.T) {
	setValidEnvironment(t)
	setRawRetentionEnvironment(t)
	t.Setenv("BILLING_RAW_RETENTION_ENVIRONMENT", "dev")
	cfg, err := Load()
	if err != nil || cfg.Environment != "staging" || cfg.RawRetention.Environment != "dev" {
		t.Fatalf("retention scope override disturbed existing state: %v", err)
	}
	t.Setenv("BILLING_RAW_RETENTION_ENVIRONMENT", "unknown")
	if _, err := Load(); err == nil {
		t.Fatal("unregistered retention environment accepted")
	}
}
