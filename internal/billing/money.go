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
	if rate.TaxRateBasisPoints < 0 || rate.TaxRateBasisPoints > 10000 {
		return 0, 0, 0, ErrInvalidAmount
	}
	subtotal, err := PriceSubtotal(rate, quantity, quantityScale)
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

func PriceSubtotal(rate PricingRate, quantity int64, quantityScale int) (int64, error) {
	if quantity < 0 || rate.UnitPriceMinor < 0 {
		return 0, ErrInvalidAmount
	}
	if quantityScale < 0 || quantityScale > 9 || rate.UnitPriceScale < 0 || rate.UnitPriceScale > 9 ||
		(rate.QuantityScale != nil && *rate.QuantityScale != quantityScale) {
		return 0, ErrInvalidScale
	}
	denominator := pow10(quantityScale + rate.UnitPriceScale)
	product := new(big.Int).Mul(big.NewInt(quantity), big.NewInt(rate.UnitPriceMinor))
	return roundedInt64(product, denominator, rate.RoundingMode)
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
		lineTotal := new(big.Int).Add(big.NewInt(line.SubtotalMinor), big.NewInt(line.TaxMinor))
		if line.SubtotalMinor < 0 || line.TaxMinor < 0 || line.TotalMinor < 0 || !lineTotal.IsInt64() || lineTotal.Int64() != line.TotalMinor {
			return ErrInvoiceMismatch
		}
		subtotal.Add(&subtotal, big.NewInt(line.SubtotalMinor))
		tax.Add(&tax, big.NewInt(line.TaxMinor))
	}
	if !subtotal.IsInt64() || !tax.IsInt64() {
		return ErrOverflow
	}
	invoiceTotal := new(big.Int).Add(big.NewInt(invoice.SubtotalMinor), big.NewInt(invoice.TaxMinor))
	settlementTotal := new(big.Int).Add(big.NewInt(invoice.AmountSettledMinor), big.NewInt(invoice.AmountDueMinor))
	if subtotal.Int64() != invoice.SubtotalMinor || tax.Int64() != invoice.TaxMinor ||
		!invoiceTotal.IsInt64() || invoiceTotal.Int64() != invoice.TotalMinor ||
		invoice.AmountSettledMinor < 0 || invoice.AmountDueMinor < 0 ||
		!settlementTotal.IsInt64() || settlementTotal.Int64() != invoice.TotalMinor {
		return ErrInvoiceMismatch
	}
	if invoice.TaxMode == TaxModeInvoiceTotal {
		if invoice.InvoiceTaxRateBasisPoints == nil || *invoice.InvoiceTaxRateBasisPoints < 0 || *invoice.InvoiceTaxRateBasisPoints > 10000 ||
			invoice.InvoiceTaxCategory == "" || !validRoundingMode(invoice.InvoiceTaxRoundingMode) {
			return ErrInvoiceMismatch
		}
		expectedTax, err := roundedInt64(new(big.Int).Mul(big.NewInt(invoice.SubtotalMinor), big.NewInt(*invoice.InvoiceTaxRateBasisPoints)), big.NewInt(10000), invoice.InvoiceTaxRoundingMode)
		if err != nil || expectedTax != invoice.TaxMinor {
			return ErrInvoiceMismatch
		}
	} else {
		if invoice.TaxMode != "" && invoice.TaxMode != TaxModeLine ||
			invoice.InvoiceTaxRateBasisPoints != nil || invoice.InvoiceTaxRoundingMode != "" || invoice.InvoiceTaxCategory != "" {
			return ErrInvoiceMismatch
		}
	}
	return nil
}
