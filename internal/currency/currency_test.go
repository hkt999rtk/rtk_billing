package currency

import "testing"

func TestCurrencyRepresentationDoesNotEnableSettlement(t *testing.T) {
	for _, tc := range []struct {
		code   Code
		digits int
	}{
		{TWD, 0}, {USD, 2}, {CNY, 2},
	} {
		digits, ok := MinorDigits(tc.code)
		if !ok || digits != tc.digits {
			t.Fatalf("%s digits: %d %t", tc.code, digits, ok)
		}
		if CanSettle(tc.code) != (tc.code == TWD) {
			t.Fatalf("unexpected settlement policy for %s", tc.code)
		}
	}
	if _, ok := MinorDigits("RMB"); ok {
		t.Fatal("RMB is a display name, not the ISO currency code")
	}
}
