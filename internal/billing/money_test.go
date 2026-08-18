package billing

import (
	"errors"
	"math"
	"testing"
)

func TestPriceUsageUsesFixedPrecisionAndHalfUpRounding(t *testing.T) {
	rate := PricingRate{UnitPriceMinor: 28, UnitPriceScale: 1, TaxRateBasisPoints: 500, RoundingMode: RoundingHalfUp}
	subtotal, tax, total, err := PriceUsage(rate, 1862, 1)
	if err != nil {
		t.Fatal(err)
	}
	if subtotal != 521 || tax != 26 || total != 547 {
		t.Fatalf("subtotal=%d tax=%d total=%d", subtotal, tax, total)
	}
}

func TestPriceUsageHonorsRoundingModes(t *testing.T) {
	for _, tc := range []struct {
		mode RoundingMode
		want int64
	}{{RoundingDown, 1}, {RoundingHalfUp, 2}, {RoundingUp, 2}} {
		rate := PricingRate{UnitPriceMinor: 15, UnitPriceScale: 1, RoundingMode: tc.mode}
		got, _, _, err := PriceUsage(rate, 1, 0)
		if err != nil || got != tc.want {
			t.Fatalf("mode=%s got=%d want=%d err=%v", tc.mode, got, tc.want, err)
		}
	}
}

func TestPriceUsageRejectsOverflowAndInvalidScale(t *testing.T) {
	_, _, _, err := PriceUsage(PricingRate{UnitPriceMinor: math.MaxInt64, RoundingMode: RoundingHalfUp}, math.MaxInt64, 0)
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflow err=%v", err)
	}
	_, _, _, err = PriceUsage(PricingRate{UnitPriceMinor: 1, UnitPriceScale: 10, RoundingMode: RoundingHalfUp}, 1, 0)
	if !errors.Is(err, ErrInvalidScale) {
		t.Fatalf("scale err=%v", err)
	}
}

func TestValidateInvoiceTotalsRejectsMismatch(t *testing.T) {
	invoice := Invoice{SubtotalMinor: 100, TaxMinor: 5, TotalMinor: 105, AmountDueMinor: 105, Lines: []InvoiceLine{{SubtotalMinor: 99, TaxMinor: 5, TotalMinor: 104}}}
	if !errors.Is(ValidateInvoiceTotals(invoice), ErrInvoiceMismatch) {
		t.Fatal("expected invoice mismatch")
	}
}
