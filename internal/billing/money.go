package billing

import (
	"errors"
	"math/big"
)

var (
	ErrInvalidAmount   = errors.New("invalid billing amount")
	ErrInvalidScale    = errors.New("invalid billing scale")
	ErrInvalidRounding = errors.New("invalid billing rounding mode")
	ErrOverflow        = errors.New("billing arithmetic overflow")
	ErrInvoiceMismatch = errors.New("invoice totals do not reconcile")
)

func PriceUsage(rate PricingRate, quantity int64, quantityScale int) (subtotalMinor, taxMinor, totalMinor int64, err error) {
	if quantity < 0 || rate.UnitPriceMinor < 0 || rate.TaxRateBasisPoints < 0 || rate.TaxRateBasisPoints > 10000 {
		return 0, 0, 0, ErrInvalidAmount
	}
	if quantityScale < 0 || quantityScale > 9 || rate.UnitPriceScale < 0 || rate.UnitPriceScale > 9 {
		return 0, 0, 0, ErrInvalidScale
	}
	denominator := pow10(quantityScale + rate.UnitPriceScale)
	product := new(big.Int).Mul(big.NewInt(quantity), big.NewInt(rate.UnitPriceMinor))
	subtotal, err := roundedInt64(product, denominator, rate.RoundingMode)
	if err != nil {
		return 0, 0, 0, err
	}
	taxNumerator := new(big.Int).Mul(big.NewInt(subtotal), big.NewInt(rate.TaxRateBasisPoints))
	tax, err := roundedInt64(taxNumerator, big.NewInt(10000), rate.RoundingMode)
	if err != nil {
		return 0, 0, 0, err
	}
	total := new(big.Int).Add(big.NewInt(subtotal), big.NewInt(tax))
	if !total.IsInt64() {
		return 0, 0, 0, ErrOverflow
	}
	return subtotal, tax, total.Int64(), nil
}

func roundedInt64(numerator, denominator *big.Int, mode RoundingMode) (int64, error) {
	if numerator.Sign() < 0 || denominator.Sign() <= 0 {
		return 0, ErrInvalidAmount
	}
	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if remainder.Sign() != 0 {
		switch mode {
		case RoundingDown:
		case RoundingUp:
			quotient.Add(quotient, big.NewInt(1))
		case RoundingHalfUp:
			doubled := new(big.Int).Lsh(new(big.Int).Set(remainder), 1)
			if doubled.Cmp(denominator) >= 0 {
				quotient.Add(quotient, big.NewInt(1))
			}
		default:
			return 0, ErrInvalidRounding
		}
	} else if mode != RoundingDown && mode != RoundingUp && mode != RoundingHalfUp {
		return 0, ErrInvalidRounding
	}
	if !quotient.IsInt64() {
		return 0, ErrOverflow
	}
	return quotient.Int64(), nil
}

func pow10(scale int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
}

func ValidateInvoiceTotals(invoice Invoice) error {
	var subtotal, tax big.Int
	for _, line := range invoice.Lines {
		if line.SubtotalMinor < 0 || line.TaxMinor < 0 || line.TotalMinor < 0 || line.SubtotalMinor+line.TaxMinor != line.TotalMinor {
			return ErrInvoiceMismatch
		}
		subtotal.Add(&subtotal, big.NewInt(line.SubtotalMinor))
		tax.Add(&tax, big.NewInt(line.TaxMinor))
	}
	if !subtotal.IsInt64() || !tax.IsInt64() {
		return ErrOverflow
	}
	if subtotal.Int64() != invoice.SubtotalMinor || tax.Int64() != invoice.TaxMinor ||
		invoice.SubtotalMinor+invoice.TaxMinor != invoice.TotalMinor ||
		invoice.AmountSettledMinor < 0 || invoice.AmountDueMinor < 0 ||
		invoice.AmountSettledMinor+invoice.AmountDueMinor != invoice.TotalMinor {
		return ErrInvoiceMismatch
	}
	return nil
}
