package api

import (
	"math"
	"testing"
	"time"
)

func TestForecastBillingUsageUsesEvidenceWindow(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	through := start.Add(10 * 24 * time.Hour)
	forecast := forecastBillingUsage(billingUsageResponse{
		PeriodStart: start, PeriodEnd: start.Add(31 * 24 * time.Hour), Total: 1000,
		FactCount: 4, UsageThrough: &through,
	}, through)
	if forecast.State != "available" || forecast.Confidence != "medium" || forecast.ObservationDays != 10 {
		t.Fatalf("forecast=%+v", forecast)
	}
	if *forecast.ProjectedPeriodTotalMinor != 3100 || *forecast.ProjectedRemainingMinor != 2100 || *forecast.AverageDailyCostMinor != 100 {
		t.Fatalf("forecast totals=%+v", forecast)
	}
}

func TestForecastBillingUsageFailsClosedForInsufficientOrOverflowingEvidence(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	through := start.Add(23 * time.Hour)
	if got := forecastBillingUsage(billingUsageResponse{PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0), Total: 100, FactCount: 1, UsageThrough: &through}, through); got.State != "unavailable" {
		t.Fatalf("short observation forecast=%+v", got)
	}
	if _, ok := checkedRatio(math.MaxInt64, math.MaxInt64, 1); ok {
		t.Fatal("overflowing ratio passed")
	}
	future := start.Add(48 * time.Hour)
	if got := forecastBillingUsage(billingUsageResponse{PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0), Total: 100, FactCount: 1, UsageThrough: &future}, start.Add(24*time.Hour)); got.State != "unavailable" {
		t.Fatalf("future evidence forecast=%+v", got)
	}
}
