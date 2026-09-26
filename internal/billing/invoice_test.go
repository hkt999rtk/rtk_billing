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

func TestTWDPerMillionPriceRoundsAfterMonthlyAggregation(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	rate := PricingRate{ID: "mqtt-rate", PricingVersionID: "twd-v2", ServiceCode: "mqtt", MetricCode: "publish_count", Unit: "requests", UnitPriceMinor: 32, UnitPriceScale: 6, RoundingMode: RoundingHalfUp}
	facts := []UsageFact{
		{UsageID: "first", OrganizationID: "org-1", ServiceCode: "mqtt", MetricCode: "publish_count", Unit: "requests", Quantity: 10000, WindowStart: start, WindowEnd: start.Add(time.Hour)},
		{UsageID: "second", OrganizationID: "org-1", ServiceCode: "mqtt", MetricCode: "publish_count", Unit: "requests", Quantity: 10000, WindowStart: start.Add(time.Hour), WindowEnd: start.Add(2 * time.Hour)},
	}
	invoice, err := BuildDraftInvoice(Invoice{OrganizationID: "org-1", PricingVersionID: "twd-v2", Currency: CurrencyTWD, PeriodStart: start, PeriodEnd: end}, facts, []PricingRate{rate})
	if err != nil || invoice.SubtotalMinor != 1 || invoice.TotalMinor != 1 || len(invoice.Lines) != 1 || invoice.Lines[0].Quantity != 20000 {
		t.Fatalf("monthly aggregate: invoice=%+v err=%v", invoice, err)
	}
	for _, unsupported := range []Currency{CurrencyUSD, CurrencyCNY} {
		if _, err := BuildDraftInvoice(Invoice{OrganizationID: "org-1", PricingVersionID: "twd-v2", Currency: unsupported, PeriodStart: start, PeriodEnd: end}, facts, []PricingRate{rate}); !errors.Is(err, ErrInvalidInvoice) {
			t.Fatalf("%s must not issue before settlement qualification: %v", unsupported, err)
		}
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

func TestBuildDraftInvoiceKeepsProductDetailsAndTotals(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	facts := []UsageFact{
		{UsageID: "a", OrganizationID: "org", ProductID: "product-a", ServiceCode: "logger", MetricCode: "ingest_gib", Quantity: 2, Unit: "GiB", WindowStart: start, WindowEnd: start.Add(time.Hour)},
		{UsageID: "b", OrganizationID: "org", ProductID: "product-b", ServiceCode: "logger", MetricCode: "ingest_gib", Quantity: 3, Unit: "GiB", WindowStart: start, WindowEnd: start.Add(time.Hour)},
	}
	invoice, err := BuildDraftInvoice(Invoice{OrganizationID: "org", PricingVersionID: "price", Currency: CurrencyTWD, PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0)}, facts,
		[]PricingRate{{ServiceCode: "logger", MetricCode: "ingest_gib", Unit: "GiB", UnitPriceMinor: 10, RoundingMode: RoundingHalfUp}})
	if err != nil || len(invoice.Lines) != 2 || invoice.SubtotalMinor != 50 || invoice.Lines[0].ProductID != "product-a" || invoice.Lines[1].ProductID != "product-b" {
		t.Fatalf("invoice=%+v err=%v", invoice, err)
	}
}

func TestInvoiceTotalTaxUsesCombinedSubtotalAndStableAllocation(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rate := int64(500)
	facts := []UsageFact{
		{UsageID: "b", OrganizationID: "org", ProductID: "product-b", ServiceCode: "logger", MetricCode: "ingest", Quantity: 5, Unit: "requests", WindowStart: start, WindowEnd: start.Add(time.Minute)},
		{UsageID: "a", OrganizationID: "org", ProductID: "product-a", ServiceCode: "logger", MetricCode: "ingest", Quantity: 5, Unit: "requests", WindowStart: start, WindowEnd: start.Add(time.Minute)},
	}
	rates := []PricingRate{{ServiceCode: "logger", MetricCode: "ingest", Unit: "requests", UnitPriceMinor: 1, RoundingMode: RoundingHalfUp}}
	base := Invoice{OrganizationID: "org", PricingVersionID: "price", Currency: CurrencyTWD, PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0),
		TaxMode: TaxModeInvoiceTotal, InvoiceTaxRateBasisPoints: &rate, InvoiceTaxRoundingMode: RoundingHalfUp, InvoiceTaxCategory: "standard"}
	got, err := BuildDraftInvoice(base, facts, rates)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubtotalMinor != 10 || got.TaxMinor != 1 || got.TotalMinor != 11 || len(got.Lines) != 2 ||
		got.Lines[0].ProductID != "product-a" || got.Lines[0].TaxMinor != 1 || got.Lines[1].TaxMinor != 0 {
		t.Fatalf("invoice total tax allocation: %+v", got)
	}
	reordered, err := BuildDraftInvoice(base, []UsageFact{facts[1], facts[0]}, rates)
	if err != nil || reordered.Lines[0].TaxMinor != got.Lines[0].TaxMinor || reordered.Lines[1].TaxMinor != got.Lines[1].TaxMinor {
		t.Fatalf("input order changed tax allocation: %+v err=%v", reordered, err)
	}
	legacy := base
	legacy.TaxMode, legacy.InvoiceTaxRateBasisPoints, legacy.InvoiceTaxRoundingMode, legacy.InvoiceTaxCategory = TaxModeLine, nil, "", ""
	legacy, err = BuildDraftInvoice(legacy, facts, rates)
	if err != nil || legacy.TaxMinor != 0 {
		t.Fatalf("legacy line tax changed: %+v err=%v", legacy, err)
	}
	missing := base
	missing.InvoiceTaxRateBasisPoints = nil
	if _, err := BuildDraftInvoice(missing, facts, rates); !errors.Is(err, ErrInvalidInvoice) {
		t.Fatalf("missing approved tax rate must fail closed: %v", err)
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
