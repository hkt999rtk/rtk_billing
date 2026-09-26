package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

type otaUsagePreviewStore struct {
	billingPersistence
	pricing billing.PricingVersion
	facts   []billing.UsageFact
}

func (s otaUsagePreviewStore) ActivePricingVersion(context.Context, time.Time, billing.Currency) (billing.PricingVersion, error) {
	return s.pricing, nil
}

func (s otaUsagePreviewStore) ListUsageFacts(context.Context, string, time.Time, time.Time) ([]billing.UsageFact, error) {
	return s.facts, nil
}

func TestBillingUsagePreviewExcludesPreactivationOTAFacts(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	ota := billing.UsageFact{UsageID: "ota", OrganizationID: "cloud", ProductID: "product", ServiceCode: billing.ServiceOTA,
		MetricCode: billing.MetricOTADeviceTask, Quantity: 1, Unit: billing.UnitOTADeviceTask,
		WindowStart: start.Add(2 * time.Minute), WindowEnd: start.Add(3 * time.Minute)}
	mqtt := billing.UsageFact{UsageID: "mqtt", OrganizationID: "cloud", ServiceCode: "mqtt", MetricCode: "publish_count",
		Quantity: 1_000_000, Unit: "requests", WindowStart: start, WindowEnd: start.Add(time.Minute)}
	rate := billing.PricingRate{ServiceCode: "mqtt", MetricCode: "publish_count", Unit: "requests",
		UnitPriceMinor: 32, UnitPriceScale: 6, RoundingMode: billing.RoundingHalfUp}
	s := &Server{billing: &billingRuntime{store: otaUsagePreviewStore{
		pricing: billing.PricingVersion{ID: "old", Rates: []billing.PricingRate{rate}},
		facts:   []billing.UsageFact{ota, mqtt},
	}}}
	usage, err := s.billingUsageForPeriod(context.Background(), "cloud", billing.BillingProfile{}, start, end)
	if err != nil || usage.Total != 32 || usage.FactCount != 1 || len(usage.Lines) != 1 || usage.Lines[0].ServiceCode != "mqtt" ||
		usage.UsageThrough == nil || !usage.UsageThrough.Equal(mqtt.WindowEnd) {
		t.Fatalf("preactivation usage=%+v err=%v", usage, err)
	}
	s.billing.store = otaUsagePreviewStore{pricing: billing.PricingVersion{ID: "old", Rates: []billing.PricingRate{rate}}, facts: []billing.UsageFact{ota}}
	usage, err = s.billingUsageForPeriod(context.Background(), "cloud", billing.BillingProfile{}, start, end)
	if err != nil || usage.Total != 0 || usage.FactCount != 0 || len(usage.Lines) != 0 || usage.UsageThrough != nil {
		t.Fatalf("OTA-only preactivation usage=%+v err=%v", usage, err)
	}
	s.billing.store = otaUsagePreviewStore{pricing: billing.PricingVersion{ID: "partial", Rates: append([]billing.PricingRate{rate}, billing.ProposedOTARates()[:3]...)}, facts: []billing.UsageFact{ota, mqtt}}
	if _, err := s.billingUsageForPeriod(context.Background(), "cloud", billing.BillingProfile{}, start, end); !errors.Is(err, billing.ErrInvalidInvoice) {
		t.Fatalf("partial OTA card must fail closed, got %v", err)
	}
}
