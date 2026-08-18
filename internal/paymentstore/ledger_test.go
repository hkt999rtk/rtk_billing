package paymentstore

import (
	"errors"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func validLedgerInput() PostLedgerEntryInput {
	return PostLedgerEntryInput{
		AccountID:        "account-1",
		Direction:        payment.LedgerDirectionDebit,
		AmountMinor:      100,
		Currency:         payment.CurrencyTWD,
		Reason:           payment.LedgerReasonInvoiceDebit,
		IdempotencyScope: "invoice",
		IdempotencyKey:   "invoice-1",
	}
}

func TestValidateLedgerInputRequiresStablePairsAndValidMoney(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PostLedgerEntryInput)
		want   error
	}{
		{name: "missing account", mutate: func(in *PostLedgerEntryInput) { in.AccountID = "" }, want: ErrConflict},
		{name: "partial external reference", mutate: func(in *PostLedgerEntryInput) { in.ExternalType = "invoice" }, want: ErrConflict},
		{name: "partial actor", mutate: func(in *PostLedgerEntryInput) { in.ActorID = "user-1" }, want: ErrConflict},
		{name: "zero amount", mutate: func(in *PostLedgerEntryInput) { in.AmountMinor = 0 }, want: payment.ErrInvalidAmount},
		{name: "wrong reason", mutate: func(in *PostLedgerEntryInput) { in.Reason = payment.LedgerReasonPaymentTopUpCredit }, want: payment.ErrInvalidReason},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validLedgerInput()
			tc.mutate(&in)
			if err := validateLedgerInput(in); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if err := validateLedgerInput(validLedgerInput()); err != nil {
		t.Fatal(err)
	}
}

func TestSettlementReversalNeverTriggersAutomaticTopUp(t *testing.T) {
	for _, reason := range []payment.LedgerReason{payment.LedgerReasonRefundDebit, payment.LedgerReasonChargebackDebit} {
		if !isSettlementReversal(reason) {
			t.Fatalf("%s must be treated as a settlement reversal", reason)
		}
	}
	for _, reason := range []payment.LedgerReason{
		payment.LedgerReasonInvoiceDebit,
		payment.LedgerReasonUsageAdjustmentDebit,
		payment.LedgerReasonManualAdjustmentDebit,
	} {
		if isSettlementReversal(reason) {
			t.Fatalf("%s must remain an ordinary debit", reason)
		}
	}
}
