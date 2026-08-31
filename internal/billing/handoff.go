package billing

// FinancialEvidence is a Billing-side snapshot, never browser input. The
// completeness flags must be backed by reconciled, persisted checkpoints for
// the operation's cutoff. Zero rows or elapsed time are not completeness proof.
// Authority, ownership version and participant confirmation are checked by the
// handoff coordinator in addition to these financial conditions.
type FinancialEvidence struct {
	BalanceKnown                 bool
	Currency                     Currency
	BalanceMinor                 int64
	UsageSettled                 bool
	InvoicesReconciled           bool
	ProviderWorkReconciled       bool
	UnpaidInvoiceCount           int64
	DebtMinor                    int64
	PendingPaymentCount          int64
	PendingRefundCount           int64
	OpenDisputeCount             int64
	PendingSetupCount            int64
	UnresolvedProviderEventCount int64
}

// OwnershipTransferBlockers deliberately accepts zero credit. A positive
// balance never suppresses another blocker. The stable order supports both
// machine checks and a consistent UI; an empty result is not ownership authority.
func OwnershipTransferBlockers(e FinancialEvidence) []string {
	blockers := make([]string, 0)
	if !e.BalanceKnown {
		blockers = append(blockers, "balance_evidence_missing")
	} else {
		if e.Currency != CurrencyTWD {
			blockers = append(blockers, "currency_unsupported")
		}
		if e.BalanceMinor < 0 {
			blockers = append(blockers, "balance_negative")
		}
	}
	if !e.UsageSettled {
		blockers = append(blockers, "usage_unsettled")
	}
	if !e.InvoicesReconciled {
		blockers = append(blockers, "invoice_evidence_missing")
	}
	if !e.ProviderWorkReconciled {
		blockers = append(blockers, "provider_evidence_missing")
	}
	// Negative counts/debt are malformed evidence, not an alternate encoding of
	// "none". In particular, a negative debt amount is not spendable credit.
	if e.UnpaidInvoiceCount < 0 || e.DebtMinor < 0 || e.PendingPaymentCount < 0 || e.PendingRefundCount < 0 || e.OpenDisputeCount < 0 || e.PendingSetupCount < 0 || e.UnresolvedProviderEventCount < 0 {
		blockers = append(blockers, "financial_evidence_invalid")
	}
	for _, check := range []struct {
		count int64
		code  string
	}{
		{e.UnpaidInvoiceCount, "unpaid_invoices"},
		{e.DebtMinor, "outstanding_debt"},
		{e.PendingPaymentCount, "payments_pending"},
		{e.PendingRefundCount, "refunds_pending"},
		{e.OpenDisputeCount, "disputes_open"},
		{e.PendingSetupCount, "payment_setups_pending"},
		{e.UnresolvedProviderEventCount, "provider_events_unresolved"},
	} {
		if check.count > 0 {
			blockers = append(blockers, check.code)
		}
	}
	return blockers
}

// CloudClosureBlockers keeps deletion stricter than transfer: positive credit
// must be resolved before closure. It cannot reuse >= 0 as its balance predicate.
func CloudClosureBlockers(e FinancialEvidence) []string {
	blockers := OwnershipTransferBlockers(e)
	if e.BalanceKnown && e.BalanceMinor > 0 {
		blockers = append(blockers, "balance_positive")
	}
	return blockers
}
