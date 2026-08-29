package main

import (
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/config"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func providerNames(providers []payment.PaymentProvider) []string {
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, provider.Name())
	}
	return names
}

func TestBuildPaymentProvidersSupportsNewebPayWithoutLegacySimulator(t *testing.T) {
	providers, chargeEnabled, err := buildPaymentProviders(config.Config{
		NewebPayEnabled:         true,
		NewebPayEnvironment:     "sandbox",
		NewebPayMerchantID:      "MS123456789",
		NewebPayHashKey:         "12345678901234567890123456789012",
		NewebPayHashIV:          "1234567890123456",
		NewebPayEndpointBaseURL: "http://payment-simulator.invalid",
		RequestTimeout:          time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].Name() != "newebpay" {
		t.Fatalf("providers=%v", providerNames(providers))
	}
	if chargeEnabled["newebpay"] {
		t.Fatal("hosted NewebPay must not enable unattended merchant-initiated charge")
	}
}

func TestBuildPaymentProvidersKeepsSimulatorChargeAndAddsNewebPayQuery(t *testing.T) {
	providers, chargeEnabled, err := buildPaymentProviders(config.Config{
		Environment:             "test",
		SimulatorEnabled:        true,
		SimulatorBaseURL:        "http://legacy-simulator.invalid",
		SimulatorSharedSecret:   "12345678901234567890123456789012",
		SimulatorRunID:          "worker-test",
		SimulatorScenario:       "success",
		NewebPayEnabled:         true,
		NewebPayEnvironment:     "sandbox",
		NewebPayMerchantID:      "MS123456789",
		NewebPayHashKey:         "12345678901234567890123456789012",
		NewebPayHashIV:          "1234567890123456",
		NewebPayEndpointBaseURL: "http://payment-simulator.invalid",
		RequestTimeout:          time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := providerNames(providers); len(got) != 2 || got[0] != "simulator" || got[1] != "newebpay" {
		t.Fatalf("providers=%v", got)
	}
	if !chargeEnabled["simulator"] || chargeEnabled["newebpay"] {
		t.Fatalf("charge enabled=%v", chargeEnabled)
	}
}
