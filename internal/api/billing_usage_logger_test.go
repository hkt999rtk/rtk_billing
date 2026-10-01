package api

import (
	"context"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
)

func loggerPreviewRates() []billing.PricingRate {
	scale := 9
	return []billing.PricingRate{{ServiceCode: billing.ServiceLogger, MetricCode: billing.MetricLoggerIngest, Unit: "GiB", QuantityScale: &scale, UnitPriceMinor: 10, RoundingMode: billing.RoundingHalfUp},
		{ServiceCode: billing.ServiceLogger, MetricCode: billing.MetricLoggerRetained, Unit: "GiB-month", QuantityScale: &scale, UnitPriceMinor: 4, RoundingMode: billing.RoundingHalfUp}}
}

func TestLoggerPricedUsageUsesUTCAndHoldsPartialOwnership(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC)
	start := billingUTCMonthStart(now)
	end := start.AddDate(0, 1, 0)
	store := otaUsagePreviewStore{pricing: billing.PricingVersion{ID: "logger", Rates: loggerPreviewRates()}, profile: billing.BillingProfile{Timezone: "America/Los_Angeles"},
		facts: []billing.UsageFact{{UsageID: "logger-ingest", OrganizationID: "cloud", ProductID: "product", ServiceCode: billing.ServiceLogger, MetricCode: billing.MetricLoggerIngest, Quantity: 1_000_000_000, QuantityScale: 9, Unit: "GiB", WindowStart: start, WindowEnd: end}}}
	server := &Server{billing: &billingRuntime{store: store, now: func() time.Time { return now }}}
	usage, err := server.currentBillingUsage(context.Background(), "cloud")
	if err != nil || !usage.PeriodStart.Equal(start) || !usage.PeriodEnd.Equal(end) || usage.LoggerEstimateStatus != "estimated" || usage.Total != 10 {
		t.Fatal("UTC Logger estimate", usage, err)
	}
	ctx := billingidentity.WithScope(context.Background(), billingidentity.Scope{CurrentPeriodStart: start.Add(24 * time.Hour)})
	usage, err = server.currentBillingUsage(ctx, "cloud")
	if err != nil || usage.LoggerEstimateStatus != "held_for_review" || usage.LoggerEstimateReason != "owner_month_incomplete" || usage.Total != 0 || len(usage.Lines) != 0 {
		t.Fatal("partial owner was charged Logger", usage, err)
	}
	usage, err = server.billingUsageForPeriod(context.Background(), "cloud", store.profile, start.Add(time.Hour), end)
	if err != nil || usage.LoggerEstimateStatus != "held_for_review" || usage.LoggerEstimateReason != "period_not_utc_month" || usage.Total != 0 {
		t.Fatal("partial month was charged Logger", usage, err)
	}
	if forecast := forecastBillingUsage(billingUsageResponse{LoggerEstimateStatus: "held_for_review", FactCount: 1, Total: 10, UsageThrough: &now, PeriodStart: start, PeriodEnd: end}, now); forecast.State != "unavailable" {
		t.Fatal("held Logger forecast is complete", forecast)
	}
}
