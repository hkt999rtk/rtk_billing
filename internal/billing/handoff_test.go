package billing

import (
	"math"
	"slices"
	"testing"
)

func settledHandoffEvidence(balance int64) FinancialEvidence {
	return FinancialEvidence{BalanceKnown: true, Currency: CurrencyTWD, BalanceMinor: balance, UsageSettled: true, InvoicesReconciled: true, ProviderWorkReconciled: true}
}

func TestOwnershipTransferNonnegativeBalanceAndClosureZero(t *testing.T) {
	for _, amount := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
		e := settledHandoffEvidence(amount)
		transfer, closure := OwnershipTransferBlockers(e), CloudClosureBlockers(e)
		if (len(transfer) == 0) != (amount >= 0) {
			t.Fatalf("transfer amount=%d blockers=%v", amount, transfer)
		}
		if (len(closure) == 0) != (amount == 0) {
			t.Fatalf("closure amount=%d blockers=%v", amount, closure)
		}
		if amount < 0 && !slices.Contains(transfer, "balance_negative") {
			t.Fatalf("negative amount lost reason: %v", transfer)
		}
		if amount > 0 && !slices.Contains(closure, "balance_positive") {
			t.Fatalf("positive closure lost reason: %v", closure)
		}
	}
}

func TestOwnershipTransferCreditDoesNotOverrideIndependentBlockers(t *testing.T) {
	for _, test := range []struct {
		code   string
		mutate func(*FinancialEvidence)
	}{
		{"balance_evidence_missing", func(e *FinancialEvidence) { e.BalanceKnown = false }},
		{"currency_unsupported", func(e *FinancialEvidence) { e.Currency = "USD" }},
		{"usage_unsettled", func(e *FinancialEvidence) { e.UsageSettled = false }},
		{"invoice_evidence_missing", func(e *FinancialEvidence) { e.InvoicesReconciled = false }},
		{"provider_evidence_missing", func(e *FinancialEvidence) { e.ProviderWorkReconciled = false }},
		{"unpaid_invoices", func(e *FinancialEvidence) { e.UnpaidInvoiceCount = 1 }},
		{"outstanding_debt", func(e *FinancialEvidence) { e.DebtMinor = 1 }},
		{"payments_pending", func(e *FinancialEvidence) { e.PendingPaymentCount = 1 }},
		{"refunds_pending", func(e *FinancialEvidence) { e.PendingRefundCount = 1 }},
		{"disputes_open", func(e *FinancialEvidence) { e.OpenDisputeCount = 1 }},
		{"payment_setups_pending", func(e *FinancialEvidence) { e.PendingSetupCount = 1 }},
		{"provider_events_unresolved", func(e *FinancialEvidence) { e.UnresolvedProviderEventCount = 1 }},
	} {
		t.Run(test.code, func(t *testing.T) {
			for _, amount := range []int64{0, 1, math.MaxInt64} {
				e := settledHandoffEvidence(amount)
				test.mutate(&e)
				got := OwnershipTransferBlockers(e)
				if !slices.Equal(got, []string{test.code}) {
					t.Fatalf("amount=%d blockers=%v", amount, got)
				}
			}
		})
	}
}

func TestOwnershipTransferUnknownAndMalformedEvidenceFailClosed(t *testing.T) {
	want := []string{"balance_evidence_missing", "usage_unsettled", "invoice_evidence_missing", "provider_evidence_missing"}
	if got := OwnershipTransferBlockers(FinancialEvidence{}); !slices.Equal(got, want) {
		t.Fatalf("empty evidence=%v", got)
	}
	for _, mutate := range []func(*FinancialEvidence){
		func(e *FinancialEvidence) { e.UnpaidInvoiceCount = -1 }, func(e *FinancialEvidence) { e.DebtMinor = -1 },
		func(e *FinancialEvidence) { e.PendingPaymentCount = -1 }, func(e *FinancialEvidence) { e.PendingRefundCount = -1 },
		func(e *FinancialEvidence) { e.OpenDisputeCount = -1 }, func(e *FinancialEvidence) { e.PendingSetupCount = -1 },
		func(e *FinancialEvidence) { e.UnresolvedProviderEventCount = -1 },
	} {
		e := settledHandoffEvidence(1)
		mutate(&e)
		if got := OwnershipTransferBlockers(e); !slices.Equal(got, []string{"financial_evidence_invalid"}) {
			t.Fatalf("malformed evidence=%v", got)
		}
	}
	e := settledHandoffEvidence(-1)
	e.PendingPaymentCount, e.PendingSetupCount, e.OpenDisputeCount = 1, 1, 1
	want = []string{"balance_negative", "payments_pending", "disputes_open", "payment_setups_pending"}
	if got := OwnershipTransferBlockers(e); !slices.Equal(got, want) {
		t.Fatalf("blockers lost or reordered: %v", got)
	}
}
