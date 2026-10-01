package billing

import (
	"errors"
	"slices"
	"testing"
)

func loggerAdditionTestRates() []PricingRate {
	scale := 9
	standard := "standard"
	return []PricingRate{
		{ServiceCode: ServiceLogger, MetricCode: MetricLoggerIngest, Description: "Device and application log ingest", Unit: "GiB", UnitPriceMinor: 2880, UnitPriceScale: 2, QuantityScale: &scale, RoundingMode: RoundingHalfUp, TaxCategory: &standard},
		{ServiceCode: ServiceLogger, MetricCode: MetricLoggerRetained, Description: "Device and application log retention", Unit: "GiB-month", UnitPriceMinor: 131, UnitPriceScale: 2, QuantityScale: &scale, RoundingMode: RoundingHalfUp, TaxCategory: &standard},
	}
}

func TestReviewOTALoggerAdditionsRequiresApprovedExactPairAndUnchangedBase(t *testing.T) {
	whole := 0
	standard := "standard"
	base := []PricingRate{{ServiceCode: "mqtt", MetricCode: "publish_count", Description: "Accepted publishes", Unit: "requests", UnitPriceMinor: 32, UnitPriceScale: 6, QuantityScale: &whole, RoundingMode: RoundingHalfUp, TaxCategory: &standard, TaxRateBasisPoints: 500}}
	otaOnly := append(slices.Clone(base), reviewedOTARates()...)
	candidate := append(slices.Clone(otaOnly), loggerAdditionTestRates()...)
	const reference = "finance/2026-10-02/logger-reference-rates"
	review, err := ReviewOTACandidateRatesWithLogger("base", base, candidate, reference)
	if err != nil || review.LoggerApprovalReference != reference || len(review.LoggerRateSetSHA256) != 64 || len(review.AddedLoggerRates) != 2 || len(review.AddedOTARates) != 4 {
		t.Fatalf("reviewed pair=%+v err=%v", review, err)
	}
	reversed := slices.Clone(candidate)
	slices.Reverse(reversed)
	other, err := ReviewOTACandidateRatesWithLogger("base", base, reversed, reference)
	if err != nil || other.RateSetSHA256 != review.RateSetSHA256 || other.LoggerRateSetSHA256 != review.LoggerRateSetSHA256 {
		t.Fatalf("order changed digest: %+v %v", other, err)
	}
	changedDescription := slices.Clone(candidate)
	changedDescription[len(candidate)-1].Description = "Reworded Logger retention"
	other, err = ReviewOTACandidateRatesWithLogger("base", base, changedDescription, reference)
	if err != nil || other.RateSetSHA256 == review.RateSetSHA256 || other.LoggerRateSetSHA256 == review.LoggerRateSetSHA256 {
		t.Fatalf("Logger terms absent from both digests: %+v %v", other, err)
	}
	if _, err := ReviewOTACandidateRates("base", base, candidate); !errors.Is(err, ErrOTACardReview) {
		t.Fatalf("old OTA-only review silently allowed Logger: %v", err)
	}
	if _, err := ReviewOTACandidateRatesWithLogger("base", base, otaOnly, reference); !errors.Is(err, ErrOTACardReview) {
		t.Fatalf("unrelated approval accepted: %v", err)
	}
	for _, reference := range []string{"", " ", " padded "} {
		if _, err := ReviewOTACandidateRatesWithLogger("base", base, candidate, reference); !errors.Is(err, ErrOTACardReview) {
			t.Fatalf("invalid approval %q accepted: %v", reference, err)
		}
	}
	last := len(candidate) - 1
	cases := map[string]func([]PricingRate) []PricingRate{
		"partial Logger":                func(r []PricingRate) []PricingRate { return r[:last] },
		"changed base price":            func(r []PricingRate) []PricingRate { r[0].UnitPriceMinor++; return r },
		"changed base tax":              func(r []PricingRate) []PricingRate { r[0].TaxRateBasisPoints = 0; return r },
		"lost base":                     func(r []PricingRate) []PricingRate { return r[1:] },
		"unapproved OTA":                func(r []PricingRate) []PricingRate { r[1].UnitPriceMinor++; return r },
		"other service addition":        func(r []PricingRate) []PricingRate { r[last].ServiceCode = "storage"; return r },
		"legacy metric addition":        func(r []PricingRate) []PricingRate { r[last].MetricCode = "retention_gib_month"; return r },
		"duplicate Logger":              func(r []PricingRate) []PricingRate { r[last] = r[last-1]; return r },
		"changed Logger price":          func(r []PricingRate) []PricingRate { r[last].UnitPriceMinor++; return r },
		"changed ingest price":          func(r []PricingRate) []PricingRate { r[last-1].UnitPriceMinor++; return r },
		"changed Logger unit":           func(r []PricingRate) []PricingRate { r[last].Unit = "GB-month"; return r },
		"changed Logger quantity scale": func(r []PricingRate) []PricingRate { scale := 8; r[last].QuantityScale = &scale; return r },
		"changed Logger price scale":    func(r []PricingRate) []PricingRate { r[last].UnitPriceScale = 3; return r },
		"changed Logger rounding":       func(r []PricingRate) []PricingRate { r[last].RoundingMode = RoundingDown; return r },
		"added row tax":                 func(r []PricingRate) []PricingRate { r[last].TaxRateBasisPoints = 500; return r },
		"tax exempt Logger":             func(r []PricingRate) []PricingRate { category := "exempt"; r[last].TaxCategory = &category; return r },
		"extra non-OTA": func(r []PricingRate) []PricingRate {
			extra := r[0]
			extra.ServiceCode = "shadow"
			return append(r, extra)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReviewOTACandidateRatesWithLogger("base", base, mutate(slices.Clone(candidate)), reference); !errors.Is(err, ErrOTACardReview) {
				t.Fatalf("unapproved terms accepted: %v", err)
			}
		})
	}
	baseWithLogger := append(slices.Clone(base), loggerAdditionTestRates()...)
	unchanged := append(slices.Clone(baseWithLogger), reviewedOTARates()...)
	if review, err := ReviewOTACandidateRates("base-with-logger", baseWithLogger, unchanged); err != nil || review.LoggerRateSetSHA256 != "" {
		t.Fatalf("existing Logger pair must stay unchanged without new approval: %+v %v", review, err)
	}
	if _, err := ReviewOTACandidateRatesWithLogger("base-with-logger", baseWithLogger, append(unchanged, loggerAdditionTestRates()...), reference); !errors.Is(err, ErrOTACardReview) {
		t.Fatalf("duplicate pair supplemented existing Logger: %v", err)
	}
}
