package payment

import (
	"errors"
	"testing"
)

func TestValidateChargeAmountUsesZeroDecimalTWDUnits(t *testing.T) {
	for _, amount := range []int64{1, 301, 1000} {
		if err := ValidateChargeAmount(CurrencyTWD, amount); err != nil {
			t.Fatalf("amount %d: %v", amount, err)
		}
	}
	for _, amount := range []int64{-100, 0} {
		if !errors.Is(ValidateChargeAmount(CurrencyTWD, amount), ErrInvalidAmount) {
			t.Fatalf("amount %d should be invalid", amount)
		}
	}
	if !errors.Is(ValidateChargeAmount("USD", 100), ErrInvalidCurrency) {
		t.Fatal("unsupported currency should fail")
	}
}

func TestApplyBalanceAllowsNegativeBalanceAndRejectsOverflow(t *testing.T) {
	got, err := ApplyBalance(500, LedgerDirectionDebit, 700)
	if err != nil || got != -200 {
		t.Fatalf("debit got=%d err=%v", got, err)
	}
	got, err = ApplyBalance(-200, LedgerDirectionCredit, 1000)
	if err != nil || got != 800 {
		t.Fatalf("credit got=%d err=%v", got, err)
	}
	if !errors.Is(mustBalanceError(maxInt64, LedgerDirectionCredit, 1), ErrBalanceOverflow) {
		t.Fatal("credit overflow should fail")
	}
	if !errors.Is(mustBalanceError(minInt64, LedgerDirectionDebit, 1), ErrBalanceOverflow) {
		t.Fatal("debit overflow should fail")
	}
}

func mustBalanceError(current int64, direction LedgerDirection, amount int64) error {
	_, err := ApplyBalance(current, direction, amount)
	return err
}

func TestValidateLedgerReasonMatchesDirection(t *testing.T) {
	if err := ValidateLedgerReason(LedgerDirectionCredit, LedgerReasonPaymentTopUpCredit); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ValidateLedgerReason(LedgerDirectionDebit, LedgerReasonPaymentTopUpCredit), ErrInvalidReason) {
		t.Fatal("credit reason on debit should fail")
	}
	if !errors.Is(ValidateLedgerReason("sideways", LedgerReasonInvoiceDebit), ErrInvalidDirection) {
		t.Fatal("invalid direction should fail")
	}
	if !errors.Is(mustBalanceError(0, "sideways", 100), ErrInvalidDirection) {
		t.Fatal("invalid balance direction should fail")
	}
}

func TestNormalizeProviderUsesStableLowercaseKey(t *testing.T) {
	if got := NormalizeProvider("  NewebPay  "); got != "newebpay" {
		t.Fatalf("normalized provider=%q", got)
	}
}
