package billing

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	ErrInvalidInvoice               = errors.New("invalid invoice")
	ErrInvoiceIssued                = errors.New("issued invoice is immutable")
	ErrRateNotFound                 = errors.New("pricing rate not found")
	ErrProfileConfigurationRequired = errors.New("billing profile configuration is required")
)

func BuildDraftInvoice(invoice Invoice, facts []UsageFact, rates []PricingRate) (Invoice, error) {
	if strings.TrimSpace(invoice.OrganizationID) == "" || strings.TrimSpace(invoice.PricingVersionID) == "" ||
		invoice.Currency != CurrencyTWD || !invoice.PeriodEnd.After(invoice.PeriodStart) {
		return Invoice{}, ErrInvalidInvoice
	}
	if invoice.State != "" && invoice.State != InvoiceStateDraft {
		return Invoice{}, ErrInvoiceIssued
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
	type aggregate struct {
		quantity int64
		scale    int
		refs     []string
	}
	aggregates := make(map[string]aggregate)
	for _, fact := range facts {
		if fact.OrganizationID != invoice.OrganizationID || fact.WindowStart.Before(invoice.PeriodStart) || fact.WindowEnd.After(invoice.PeriodEnd) || !fact.WindowEnd.After(fact.WindowStart) {
			return Invoice{}, ErrInvalidInvoice
		}
		key := fact.ServiceCode + "\x00" + fact.MetricCode + "\x00" + fact.Unit
		if _, ok := rateByMetric[key]; !ok {
			return Invoice{}, fmt.Errorf("%w: %s/%s/%s", ErrRateNotFound, fact.ServiceCode, fact.MetricCode, fact.Unit)
		}
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
		rate := rateByMetric[key]
		agg := aggregates[key]
		subtotal, tax, total, err := PriceUsage(rate, agg.quantity, agg.scale)
		if err != nil {
			return Invoice{}, err
		}
		sort.Strings(agg.refs)
		line := InvoiceLine{
			PricingRateID: rate.ID, ServiceCode: rate.ServiceCode, MetricCode: rate.MetricCode,
			Description: rate.Description, Quantity: agg.quantity, QuantityScale: agg.scale, Unit: rate.Unit,
			UnitPriceMinor: rate.UnitPriceMinor, UnitPriceScale: rate.UnitPriceScale,
			SubtotalMinor: subtotal, TaxMinor: tax, TotalMinor: total, RoundingMode: rate.RoundingMode,
			UsageFactRefs: agg.refs,
		}
		invoice.Lines = append(invoice.Lines, line)
		invoice.SubtotalMinor += subtotal
		invoice.TaxMinor += tax
		invoice.TotalMinor += total
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
