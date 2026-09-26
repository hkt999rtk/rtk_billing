package billing

import (
	"errors"
	"slices"
	"testing"
)

func reviewedOTARates() []PricingRate {
	rates := ProposedOTARates()
	category := "finance-reviewed"
	for i := range rates {
		rates[i].TaxCategory = &category
		rates[i].TaxRateBasisPoints = 500
	}
	return rates
}

func TestReviewOTACandidateRatesRequiresCompleteUnchangedCard(t *testing.T) {
	whole := 0
	category := "finance-reviewed"
	base := []PricingRate{
		{ServiceCode: "mqtt", MetricCode: "publish_count", Description: "Accepted publish", Unit: "requests",
			UnitPriceMinor: 32, UnitPriceScale: 6, QuantityScale: &whole, RoundingMode: RoundingHalfUp,
			TaxCategory: &category, TaxRateBasisPoints: 500},
		{ServiceCode: "video", MetricCode: "relay_minutes", Description: "Relay minutes", Unit: "minutes",
			UnitPriceMinor: 9, UnitPriceScale: 2, QuantityScale: &whole, RoundingMode: RoundingDown,
			TaxCategory: &category, TaxRateBasisPoints: 500},
	}
	candidate := append(slices.Clone(base), reviewedOTARates()...)
	review, err := ReviewOTACandidateRates("base-version", base, candidate)
	if err != nil || review.BaseVersionID != "base-version" || len(review.AddedOTARates) != 4 || len(review.RateSetSHA256) != 64 {
		t.Fatalf("complete card review=%+v err=%v", review, err)
	}
	reversed := slices.Clone(candidate)
	slices.Reverse(reversed)
	other, err := ReviewOTACandidateRates("base-version", base, reversed)
	if err != nil || other.RateSetSHA256 != review.RateSetSHA256 {
		t.Fatalf("rate order must not change review digest: %+v err=%v", other, err)
	}
	changedTax := slices.Clone(candidate)
	changedTax[2].TaxRateBasisPoints = 0
	changedTaxReview, err := ReviewOTACandidateRates("base-version", base, changedTax)
	if err != nil || changedTaxReview.RateSetSHA256 == review.RateSetSHA256 {
		t.Fatalf("tax choice must change the review digest: %+v err=%v", changedTaxReview, err)
	}

	cases := map[string]func([]PricingRate) []PricingRate{
		"lost existing rate":         func(r []PricingRate) []PricingRate { return r[1:] },
		"changed existing price":     func(r []PricingRate) []PricingRate { r[0].UnitPriceMinor++; return r },
		"changed existing tax":       func(r []PricingRate) []PricingRate { r[0].TaxRateBasisPoints = 0; return r },
		"duplicate existing rate":    func(r []PricingRate) []PricingRate { r[1] = r[0]; return r },
		"changed approved OTA price": func(r []PricingRate) []PricingRate { r[2].UnitPriceMinor++; return r },
		"missing OTA tax metadata":   func(r []PricingRate) []PricingRate { r[2].TaxCategory = nil; return r },
		"missing OTA precision":      func(r []PricingRate) []PricingRate { r[3].QuantityScale = nil; return r },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			proposed := mutate(slices.Clone(candidate))
			if _, err := ReviewOTACandidateRates("base-version", base, proposed); !errors.Is(err, ErrOTACardReview) {
				t.Fatalf("unapproved card must fail closed: %v", err)
			}
		})
	}
	unknownBaseTax := slices.Clone(base)
	unknownBaseTax[0].TaxCategory = nil
	if _, err := ReviewOTACandidateRates("base-version", unknownBaseTax, candidate); !errors.Is(err, ErrOTACardReview) {
		t.Fatalf("historical unknown tax cannot pass as approved: %v", err)
	}
}
