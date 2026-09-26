package billing

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/currency"
)

var (
	ErrInvalidInvoice               = errors.New("invalid invoice")
	ErrInvoiceIssued                = errors.New("issued invoice is immutable")
	ErrRateNotFound                 = errors.New("pricing rate not found")
	ErrProfileConfigurationRequired = errors.New("billing profile configuration is required")
)

func BuildDraftInvoice(invoice Invoice, facts []UsageFact, rates []PricingRate) (Invoice, error) {
	if strings.TrimSpace(invoice.OrganizationID) == "" || strings.TrimSpace(invoice.PricingVersionID) == "" ||
		!currency.CanSettle(invoice.Currency) || !invoice.PeriodEnd.After(invoice.PeriodStart) {
		return Invoice{}, ErrInvalidInvoice
	}
	if invoice.State != "" && invoice.State != InvoiceStateDraft {
		return Invoice{}, ErrInvoiceIssued
	}
	if invoice.TaxMode == "" {
		invoice.TaxMode = TaxModeLine
	}
	if invoice.TaxMode != TaxModeLine && invoice.TaxMode != TaxModeInvoiceTotal {
		return Invoice{}, ErrInvalidInvoice
	}
	if invoice.TaxMode == TaxModeLine && (invoice.InvoiceTaxRateBasisPoints != nil || invoice.InvoiceTaxRoundingMode != "" || invoice.InvoiceTaxCategory != "") {
		return Invoice{}, ErrInvalidInvoice
	}
	if invoice.TaxMode == TaxModeInvoiceTotal {
		if invoice.InvoiceTaxRateBasisPoints == nil || *invoice.InvoiceTaxRateBasisPoints < 0 || *invoice.InvoiceTaxRateBasisPoints > 10000 ||
			strings.TrimSpace(invoice.InvoiceTaxCategory) == "" || !validRoundingMode(invoice.InvoiceTaxRoundingMode) {
			return Invoice{}, ErrInvalidInvoice
		}
	}
	rateByMetric := make(map[string]PricingRate, len(rates))
	for _, rate := range rates {
		if rate.PricingVersionID != "" && rate.PricingVersionID != invoice.PricingVersionID {
			return Invoice{}, ErrInvalidInvoice
		}
		key := rate.ServiceCode + "\x00" + rate.MetricCode + "\x00" + rate.Unit
		if _, exists := rateByMetric[key]; exists {
			return Invoice{}, ErrInvalidInvoice
		}
		rateByMetric[key] = rate
	}
	for _, fact := range facts {
		if fact.OrganizationID != invoice.OrganizationID || fact.WindowStart.Before(invoice.PeriodStart) || fact.WindowEnd.After(invoice.PeriodEnd) || !fact.WindowEnd.After(fact.WindowStart) {
			return Invoice{}, ErrInvalidInvoice
		}
	}
	var err error
	facts, err = BillableUsageFacts(facts, rates)
	if err != nil {
		return Invoice{}, err
	}
	type aggregate struct {
		quantity int64
		scale    int
		refs     []string
	}
	aggregates := make(map[string]aggregate)
	for _, fact := range facts {
		rateKey := fact.ServiceCode + "\x00" + fact.MetricCode + "\x00" + fact.Unit
		if _, ok := rateByMetric[rateKey]; !ok {
			return Invoice{}, fmt.Errorf("%w: %s/%s/%s", ErrRateNotFound, fact.ServiceCode, fact.MetricCode, fact.Unit)
		}
		key := rateKey + "\x00" + fact.ProductID
		agg := aggregates[key]
		if len(agg.refs) > 0 && agg.scale != fact.QuantityScale {
			return Invoice{}, ErrInvalidScale
		}
		if fact.Quantity < 0 || (fact.Quantity > 0 && agg.quantity > int64(^uint64(0)>>1)-fact.Quantity) {
			return Invoice{}, ErrOverflow
		}
		agg.quantity += fact.Quantity
		agg.scale = fact.QuantityScale
		agg.refs = append(agg.refs, fact.UsageID)
		aggregates[key] = agg
	}
	keys := make([]string, 0, len(aggregates))
	for key := range aggregates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	invoice.State = InvoiceStateDraft
	invoice.Lines = make([]InvoiceLine, 0, len(keys))
	invoice.SubtotalMinor = 0
	invoice.TaxMinor = 0
	invoice.TotalMinor = 0
	for _, key := range keys {
		parts := strings.Split(key, "\x00")
		rate := rateByMetric[strings.Join(parts[:3], "\x00")]
		agg := aggregates[key]
		var subtotal, tax, total int64
		if invoice.TaxMode == TaxModeInvoiceTotal {
			subtotal, err = PriceSubtotal(rate, agg.quantity, agg.scale)
			total = subtotal
		} else {
			subtotal, tax, total, err = PriceUsage(rate, agg.quantity, agg.scale)
		}
		if err != nil {
			return Invoice{}, err
		}
		sort.Strings(agg.refs)
		line := InvoiceLine{
			ProductID:     parts[3],
			PricingRateID: rate.ID, ServiceCode: rate.ServiceCode, MetricCode: rate.MetricCode,
			Description: rate.Description, Quantity: agg.quantity, QuantityScale: agg.scale, Unit: rate.Unit,
			UnitPriceMinor: rate.UnitPriceMinor, UnitPriceScale: rate.UnitPriceScale,
			SubtotalMinor: subtotal, TaxMinor: tax, TotalMinor: total, RoundingMode: rate.RoundingMode,
			UsageFactRefs: agg.refs,
		}
		invoice.Lines = append(invoice.Lines, line)
		if subtotal > int64(^uint64(0)>>1)-invoice.SubtotalMinor || tax > int64(^uint64(0)>>1)-invoice.TaxMinor || total > int64(^uint64(0)>>1)-invoice.TotalMinor {
			return Invoice{}, ErrOverflow
		}
		invoice.SubtotalMinor += subtotal
		invoice.TaxMinor += tax
		invoice.TotalMinor += total
	}
	if invoice.TaxMode == TaxModeInvoiceTotal {
		if err := allocateInvoiceTax(&invoice); err != nil {
			return Invoice{}, err
		}
	}
	invoice.AmountSettledMinor = 0
	invoice.AmountDueMinor = invoice.TotalMinor
	if invoice.Version == 0 {
		invoice.Version = 1
	}
	if err := ValidateInvoiceTotals(invoice); err != nil {
		return Invoice{}, err
	}
	return invoice, nil
}

func validRoundingMode(mode RoundingMode) bool {
	return mode == RoundingHalfUp || mode == RoundingDown || mode == RoundingUp
}

func allocateInvoiceTax(invoice *Invoice) error {
	rate := *invoice.InvoiceTaxRateBasisPoints
	numerator := new(big.Int).Mul(big.NewInt(invoice.SubtotalMinor), big.NewInt(rate))
	tax, err := roundedInt64(numerator, big.NewInt(10000), invoice.InvoiceTaxRoundingMode)
	if err != nil {
		return err
	}
	total := new(big.Int).Add(big.NewInt(invoice.SubtotalMinor), big.NewInt(tax))
	if !total.IsInt64() {
		return ErrOverflow
	}
	type share struct {
		index     int
		remainder int64
	}
	shares := make([]share, len(invoice.Lines))
	var allocated int64
	for i := range invoice.Lines {
		part := new(big.Int).Mul(big.NewInt(invoice.Lines[i].SubtotalMinor), big.NewInt(rate))
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(part, big.NewInt(10000), remainder)
		if !quotient.IsInt64() || quotient.Int64() > tax-allocated {
			return ErrOverflow
		}
		invoice.Lines[i].TaxMinor = quotient.Int64()
		allocated += quotient.Int64()
		shares[i] = share{index: i, remainder: remainder.Int64()}
	}
	sort.SliceStable(shares, func(i, j int) bool { return shares[i].remainder > shares[j].remainder })
	remaining := tax - allocated
	if remaining < 0 || remaining > int64(len(shares)) {
		return ErrInvoiceMismatch
	}
	for i := int64(0); i < remaining; i++ {
		invoice.Lines[shares[i].index].TaxMinor++
	}
	for i := range invoice.Lines {
		lineTotal := new(big.Int).Add(big.NewInt(invoice.Lines[i].SubtotalMinor), big.NewInt(invoice.Lines[i].TaxMinor))
		if !lineTotal.IsInt64() {
			return ErrOverflow
		}
		invoice.Lines[i].TotalMinor = lineTotal.Int64()
	}
	invoice.TaxMinor = tax
	invoice.TotalMinor = total.Int64()
	return nil
}

func IssueInvoice(invoice Invoice, number string, now time.Time, dueAt time.Time) (Invoice, error) {
	if invoice.Recipient.RequiresConfiguration {
		return Invoice{}, ErrProfileConfigurationRequired
	}
	if invoice.State != InvoiceStateDraft || invoice.IssuedAt != nil || strings.TrimSpace(number) == "" || dueAt.Before(now) {
		return Invoice{}, ErrInvalidInvoice
	}
	if err := ValidateInvoiceTotals(invoice); err != nil {
		return Invoice{}, err
	}
	invoice.State = InvoiceStateIssued
	invoice.InvoiceNumber = strings.TrimSpace(number)
	issuedAt := now.UTC()
	due := dueAt.UTC()
	invoice.IssuedAt = &issuedAt
	invoice.DueAt = &due
	invoice.Version++
	invoice.UpdatedAt = issuedAt
	return invoice, nil
}

func SettleInvoice(invoice Invoice, settledMinor int64, now time.Time) (Invoice, error) {
	if invoice.State == InvoiceStateDraft || invoice.State == InvoiceStateVoid || settledMinor < 0 || settledMinor > invoice.TotalMinor {
		return Invoice{}, ErrInvalidInvoice
	}
	invoice.AmountSettledMinor = settledMinor
	invoice.AmountDueMinor = invoice.TotalMinor - settledMinor
	invoice.SettledAt = nil
	switch {
	case settledMinor == invoice.TotalMinor:
		invoice.State = InvoiceStateSettled
		settledAt := now.UTC()
		invoice.SettledAt = &settledAt
	case settledMinor > 0:
		invoice.State = InvoiceStatePartiallySettled
	default:
		invoice.State = InvoiceStateIssued
	}
	invoice.Version++
	invoice.UpdatedAt = now.UTC()
	return invoice, ValidateInvoiceTotals(invoice)
}
