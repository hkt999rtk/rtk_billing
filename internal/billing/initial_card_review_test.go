package billing

import "testing"

func TestReviewInitialCandidateRates(t *testing.T) {
	zero := 0
	category := "standard"
	rate := PricingRate{ServiceCode: "mqtt", MetricCode: "publish_count", Description: "MQTT publishes", Unit: "requests",
		UnitPriceMinor: 48, UnitPriceScale: 6, QuantityScale: &zero, RoundingMode: RoundingHalfUp, TaxCategory: &category}
	second := rate
	second.MetricCode = "delivery_count"
	digest, err := ReviewInitialCandidateRates([]PricingRate{rate, second})
	if err != nil || len(digest) != 64 {
		t.Fatalf("review initial rates: digest=%q err=%v", digest, err)
	}
	reversed, err := ReviewInitialCandidateRates([]PricingRate{second, rate})
	if err != nil || reversed != digest {
		t.Fatalf("digest must be order-independent: %q %q err=%v", digest, reversed, err)
	}
	for name, rates := range map[string][]PricingRate{
		"empty":     nil,
		"duplicate": {rate, rate},
		"ota": {{ServiceCode: ServiceOTA, MetricCode: MetricOTADeviceTask, Description: "OTA", Unit: "tasks",
			UnitPriceMinor: 96, UnitPriceScale: 3, QuantityScale: &zero, RoundingMode: RoundingHalfUp, TaxCategory: &category}},
		"unknown precision": {func() PricingRate { r := rate; r.QuantityScale = nil; return r }()},
		"line tax":          {func() PricingRate { r := rate; r.TaxRateBasisPoints = 500; return r }()},
	} {
		if _, err := ReviewInitialCandidateRates(rates); err == nil {
			t.Errorf("%s: unresolved card accepted", name)
		}
	}
}
