package billing

import (
	"errors"
	"testing"
	"time"
)

func TestBuildDraftInvoiceAggregatesUsageAndReconciles(t *testing.T) {
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	invoice, err := BuildDraftInvoice(Invoice{
		OrganizationID: "org-1", PricingVersionID: "price-1", Currency: CurrencyTWD,
		PeriodStart: start, PeriodEnd: end,
	}, []UsageFact{
		{UsageID: "usage-2", OrganizationID: "org-1", ServiceCode: "video", MetricCode: "bytes", Quantity: 100, Unit: "gb", WindowStart: start, WindowEnd: start.Add(time.Hour)},
		{UsageID: "usage-1", OrganizationID: "org-1", ServiceCode: "video", MetricCode: "bytes", Quantity: 86, Unit: "gb", WindowStart: start.Add(time.Hour), WindowEnd: start.Add(2 * time.Hour)},
	}, []PricingRate{
		{ID: "rate-1", PricingVersionID: "price-1", ServiceCode: "video", MetricCode: "bytes", Description: "Video", Unit: "gb", UnitPriceMinor: 3, RoundingMode: RoundingHalfUp, TaxRateBasisPoints: 500},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(invoice.Lines) != 1 || invoice.Lines[0].Quantity != 186 || invoice.SubtotalMinor != 558 || invoice.TaxMinor != 28 || invoice.TotalMinor != 586 {
		t.Fatalf("invoice=%+v", invoice)
	}
	if invoice.Lines[0].UsageFactRefs[0] != "usage-1" {
		t.Fatalf("refs not stable: %+v", invoice.Lines[0].UsageFactRefs)
	}
}

func TestBuildDraftInvoiceRejectsMissingRateAndCrossTenantFact(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Hour)
	base := Invoice{OrganizationID: "org-1", PricingVersionID: "price-1", Currency: CurrencyTWD, PeriodStart: start, PeriodEnd: start.Add(time.Hour)}
	_, err := BuildDraftInvoice(base, []UsageFact{{UsageID: "u", OrganizationID: "org-1", ServiceCode: "mqtt", MetricCode: "count", Quantity: 1, Unit: "requests", WindowStart: start, WindowEnd: start.Add(time.Minute)}}, nil)
	if !errors.Is(err, ErrRateNotFound) {
		t.Fatalf("missing rate err=%v", err)
	}
	_, err = BuildDraftInvoice(base, []UsageFact{{UsageID: "u", OrganizationID: "org-2", ServiceCode: "mqtt", MetricCode: "count", Quantity: 1, Unit: "requests", WindowStart: start, WindowEnd: start.Add(time.Minute)}}, []PricingRate{{ServiceCode: "mqtt", MetricCode: "count", Unit: "requests", RoundingMode: RoundingHalfUp}})
	if !errors.Is(err, ErrInvalidInvoice) {
		t.Fatalf("cross tenant err=%v", err)
	}
}

func TestIssuedInvoiceCannotBeRebuiltAndSettlementIsExact(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	invoice := Invoice{
		OrganizationID: "org-1", PricingVersionID: "price-1", Currency: CurrencyTWD,
		State: InvoiceStateDraft, PeriodStart: now.Add(-time.Hour), PeriodEnd: now,
		SubtotalMinor: 100, TaxMinor: 5, TotalMinor: 105, AmountDueMinor: 105,
		Lines: []InvoiceLine{{SubtotalMinor: 100, TaxMinor: 5, TotalMinor: 105}}, Version: 1,
	}
	issued, err := IssueInvoice(invoice, "INV-2026-000001", now, now.AddDate(0, 0, 14))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDraftInvoice(issued, nil, nil); !errors.Is(err, ErrInvoiceIssued) {
		t.Fatalf("rebuild err=%v", err)
	}
	settled, err := SettleInvoice(issued, 105, now.Add(time.Minute))
	if err != nil || settled.State != InvoiceStateSettled || settled.AmountDueMinor != 0 || settled.SettledAt == nil {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
}
