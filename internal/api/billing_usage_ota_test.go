package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
)

type otaUsagePreviewStore struct {
	billingPersistence
	pricing billing.PricingVersion
	facts   []billing.UsageFact
	profile billing.BillingProfile
}

func (s otaUsagePreviewStore) EnsureBillingProfile(context.Context, string, time.Time) (billing.BillingProfile, bool, error) {
	return s.profile, false, nil
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

func testPricedOTARates() []billing.PricingRate {
	rates := billing.ProposedOTARates()
	for i := range rates {
		category := "standard"
		rates[i].TaxCategory = &category
	}
	return rates
}

func TestCurrentBillingUsageUsesUTCMonthWhenOTAIsPriced(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC)
	store := otaUsagePreviewStore{pricing: billing.PricingVersion{ID: "ota", Rates: testPricedOTARates()},
		profile: billing.BillingProfile{Timezone: "America/Los_Angeles"}}
	s := &Server{billing: &billingRuntime{store: store, now: func() time.Time { return now }}}
	usage, err := s.currentBillingUsage(context.Background(), "cloud")
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err != nil || !usage.PeriodStart.Equal(start) || !usage.PeriodEnd.Equal(start.AddDate(0, 1, 0)) || usage.OTAEstimateStatus != "estimated" {
		t.Fatalf("UTC OTA month usage=%+v err=%v", usage, err)
	}
}

func TestBillingUsagePreviewHoldsOTAOutsideCurrentOwnerUTCMonth(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	ota := billing.UsageFact{UsageID: "ota", OrganizationID: "cloud", ProductID: "33333333-3333-4333-8333-333333333333",
		ServiceCode: billing.ServiceOTA, MetricCode: billing.MetricOTASuccessfulDownloadGiB,
		Quantity: 2_000_000_000, QuantityScale: 9, Unit: billing.UnitOTAGiB,
		WindowStart: start.Add(16 * 24 * time.Hour), WindowEnd: start.Add(16*24*time.Hour + time.Minute)}
	mqtt := billing.UsageFact{UsageID: "mqtt", OrganizationID: "cloud", ServiceCode: "mqtt", MetricCode: "publish_count",
		Quantity: 1_000_000, Unit: "requests", WindowStart: ota.WindowStart, WindowEnd: ota.WindowEnd}
	mqttRate := billing.PricingRate{ServiceCode: "mqtt", MetricCode: "publish_count", Unit: "requests",
		UnitPriceMinor: 32, UnitPriceScale: 6, RoundingMode: billing.RoundingHalfUp}
	store := otaUsagePreviewStore{pricing: billing.PricingVersion{ID: "ota", Rates: append(testPricedOTARates(), mqttRate)},
		facts: []billing.UsageFact{ota, mqtt}}
	s := &Server{billing: &billingRuntime{store: store}}
	ownerStart := start.Add(15 * 24 * time.Hour)
	ctx := billingidentity.WithScope(context.Background(), billingidentity.Scope{CurrentPeriodStart: ownerStart})
	usage, err := s.billingUsageForPeriod(ctx, "cloud", billing.BillingProfile{}, ownerStart, end)
	if err != nil || usage.Total != 32 || usage.FactCount != 1 || len(usage.Lines) != 1 || usage.Lines[0].ServiceCode != "mqtt" ||
		usage.OTAEstimateStatus != "held_for_review" || usage.OTAEstimateReason != "owner_month_incomplete" {
		t.Fatalf("owner transfer usage=%+v err=%v", usage, err)
	}
	if forecast := forecastBillingUsage(usage, end); forecast.State != "unavailable" {
		t.Fatalf("held OTA estimate projected a full bill: %+v", forecast)
	}
	usage, err = s.billingUsageForPeriod(context.Background(), "cloud", billing.BillingProfile{}, start.Add(time.Hour), end)
	if err != nil || usage.Total != 32 || usage.OTAEstimateStatus != "held_for_review" || usage.OTAEstimateReason != "period_not_utc_month" {
		t.Fatalf("partial UTC window usage=%+v err=%v", usage, err)
	}
	usage, err = s.billingUsageForPeriod(context.Background(), "cloud", billing.BillingProfile{}, start, end)
	if err != nil || usage.OTAEstimateStatus != "estimated" || usage.FactCount != 2 || len(usage.Lines) != 2 || usage.Total <= 32 {
		t.Fatalf("complete UTC owner month usage=%+v err=%v", usage, err)
	}
}

func TestBillingUsageAPIReportsHeldOTAEstimate(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	store := otaUsagePreviewStore{pricing: billing.PricingVersion{ID: "ota", Rates: testPricedOTARates()},
		facts: []billing.UsageFact{{UsageID: "ota", OrganizationID: "cloud",
			ProductID: "33333333-3333-4333-8333-333333333333", ServiceCode: billing.ServiceOTA,
			MetricCode: billing.MetricOTASuccessfulDownloadGiB, Quantity: 2_000_000_000, QuantityScale: 9, Unit: billing.UnitOTAGiB,
			WindowStart: start.Add(time.Hour), WindowEnd: start.Add(time.Hour + time.Minute)}}}
	s := &Server{billing: &billingRuntime{store: store, now: func() time.Time { return end }}}
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/orgs/cloud/billing/usage?period_start="+
		start.Add(time.Hour).Format(time.RFC3339)+"&period_end="+end.Format(time.RFC3339), nil)
	c.Params = gin.Params{{Key: "orgId", Value: "cloud"}}
	s.getBillingUsage(c)
	var body struct {
		OTAEstimateStatus string `json:"ota_estimate_status"`
		OTAEstimateReason string `json:"ota_estimate_reason"`
		FactCount         int    `json:"fact_count"`
		Lines             []any  `json:"lines"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK ||
		body.OTAEstimateStatus != "held_for_review" || body.OTAEstimateReason != "period_not_utc_month" || body.FactCount != 0 || len(body.Lines) != 0 {
		t.Fatalf("usage API status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
}
