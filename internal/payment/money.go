package payment

import "strings"

const (
	maxInt64 = int64(^uint64(0) >> 1)
	minInt64 = -maxInt64 - 1
)

func ValidateCurrency(currency Currency) error {
	if currency != CurrencyTWD {
		return ErrInvalidCurrency
	}
	return nil
}

// ValidateChargeAmount validates positive currency-unit amounts. TWD is a
// zero-decimal currency, so one amount_minor unit represents NT$1.
func ValidateChargeAmount(currency Currency, amountMinor int64) error {
	if err := ValidateCurrency(currency); err != nil {
		return err
	}
	if amountMinor <= 0 {
		return ErrInvalidAmount
	}
	return nil
}

func ValidateLedgerReason(direction LedgerDirection, reason LedgerReason) error {
	valid := false
	switch direction {
	case LedgerDirectionCredit:
		valid = reason == LedgerReasonPaymentTopUpCredit || reason == LedgerReasonManualAdjustmentCredit
	case LedgerDirectionDebit:
		valid = reason == LedgerReasonInvoiceDebit || reason == LedgerReasonUsageAdjustmentDebit ||
			reason == LedgerReasonManualAdjustmentDebit || reason == LedgerReasonRefundDebit ||
			reason == LedgerReasonChargebackDebit
	default:
		return ErrInvalidDirection
	}
	if !valid {
		return ErrInvalidReason
	}
	return nil
}

func ApplyBalance(current int64, direction LedgerDirection, amountMinor int64) (int64, error) {
	if amountMinor <= 0 {
		return 0, ErrInvalidAmount
	}
	switch direction {
	case LedgerDirectionCredit:
		if current > maxInt64-amountMinor {
			return 0, ErrBalanceOverflow
		}
		return current + amountMinor, nil
	case LedgerDirectionDebit:
		if current < minInt64+amountMinor {
			return 0, ErrBalanceOverflow
		}
		return current - amountMinor, nil
	default:
		return 0, ErrInvalidDirection
	}
}

func NormalizeProvider(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
