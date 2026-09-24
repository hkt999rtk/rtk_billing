// Package currency defines the monetary representation used by Billing.
// Enabling a settlement currency also requires an account, tax, and payment
// qualification; knowing its minor-unit scale does not enable transactions.
package currency

type Code string

const (
	TWD Code = "TWD"
	USD Code = "USD"
	CNY Code = "CNY"

	Settlement Code = TWD
)

func MinorDigits(code Code) (int, bool) {
	switch code {
	case TWD:
		return 0, true
	case USD, CNY:
		return 2, true
	default:
		return 0, false
	}
}

func CanSettle(code Code) bool { return code == Settlement }
