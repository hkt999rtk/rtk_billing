package billingstore

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestReviewedOTAPublicationRequiresExactCardTaxAndFutureUTCMonth(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE ota_pricing_publications, billing_activity_events,
		invoice_settlement_links, billing_invoice_documents, billing_invoice_lines,
		billing_invoices, billing_periods, billing_usage_facts, pricing_rates,
		pricing_plan_versions, billing_profiles, balance_ledger_entries,
		commercial_accounts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	store := New(db)
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	cutover := month.AddDate(0, 1, 0)
	whole := 0
	standard := "standard"
	baseRate := billing.PricingRate{ServiceCode: "mqtt", MetricCode: "publish_count",
		Description: "MQTT publishes", Unit: "requests", UnitPriceMinor: 32,
		UnitPriceScale: 6, QuantityScale: &whole, RoundingMode: billing.RoundingHalfUp,
		TaxCategory: &standard, TaxRateBasisPoints: 500}
	base, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{PlanKey: "ota-reviewed-test", Version: 1,
		Currency: billing.CurrencyTWD, EffectiveFrom: month.AddDate(0, -1, 0),
		CreatedBy: "integration-test", Now: now, Rates: []billing.PricingRate{baseRate}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePricingVersion(ctx, base.ID, now); err != nil {
		t.Fatal(err)
	}
	rates := append([]billing.PricingRate{baseRate}, billing.ProposedOTARates()...)
	for i := range rates {
		rates[i].TaxCategory = &standard
	}
	review, err := store.ReviewOTACandidate(ctx, ReviewOTACandidateInput{
		BaseVersionID: base.ID, EffectiveFrom: cutover, Rates: rates})
	if err != nil {
		t.Fatal(err)
	}
	taxRate := int64(500)
	draft, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{PlanKey: "ota-reviewed-test", Version: 2,
		Currency: billing.CurrencyTWD, EffectiveFrom: cutover, CreatedBy: "integration-test", Now: now,
		Rates: rates, TaxMode: billing.TaxModeInvoiceTotal, InvoiceTaxRateBasisPoints: &taxRate,
		InvoiceTaxRoundingMode: billing.RoundingHalfUp, InvoiceTaxCategory: standard})
	if err != nil {
		t.Fatal(err)
	}
	approval := ReviewedOTAPublication{BaseVersionID: base.ID, RateSetSHA256: review.Review.RateSetSHA256,
		FirstReviewer: "finance-reviewer", SecondReviewer: "billing-reviewer", ApprovedAt: now.Add(-time.Minute)}
	if _, err := store.ActivatePricingVersion(ctx, draft.ID, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("generic activation accepted OTA: %v", err)
	}
	wrongTax := int64(0)
	untaxedDraft, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{PlanKey: "ota-reviewed-test", Version: 3,
		Currency: billing.CurrencyTWD, EffectiveFrom: cutover, CreatedBy: "integration-test", Now: now,
		Rates: rates, TaxMode: billing.TaxModeInvoiceTotal, InvoiceTaxRateBasisPoints: &wrongTax,
		InvoiceTaxRoundingMode: billing.RoundingHalfUp, InvoiceTaxCategory: standard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishReviewedOTAPricingVersion(ctx, untaxedDraft.ID, now, approval); !errors.Is(err, ErrConflict) {
		t.Fatalf("OTA tax-exempt draft accepted: %v", err)
	}
	bad := approval
	bad.RateSetSHA256 = strings.Repeat("0", 64)
	if _, err := store.PublishReviewedOTAPricingVersion(ctx, draft.ID, now, bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed digest accepted: %v", err)
	}
	bad = approval
	bad.SecondReviewer = bad.FirstReviewer
	if _, err := store.PublishReviewedOTAPricingVersion(ctx, draft.ID, now, bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("single reviewer accepted: %v", err)
	}
	prior, err := store.ActivePricingVersion(ctx, now, billing.CurrencyTWD)
	if err != nil || prior.ID != base.ID {
		t.Fatalf("failed publication changed current card: %+v %v", prior, err)
	}
	published, err := store.PublishReviewedOTAPricingVersion(ctx, draft.ID, now, approval)
	if err != nil || published.ID != draft.ID || published.Status != "active" {
		t.Fatalf("reviewed publication failed: %+v %v", published, err)
	}
	prior, err = store.ActivePricingVersion(ctx, now, billing.CurrencyTWD)
	if err != nil || prior.ID != base.ID {
		t.Fatalf("future card applied too early: %+v %v", prior, err)
	}
	current, err := store.ActivePricingVersion(ctx, cutover, billing.CurrencyTWD)
	if err != nil || current.ID != draft.ID || current.TaxMode != billing.TaxModeInvoiceTotal ||
		current.InvoiceTaxRateBasisPoints == nil || *current.InvoiceTaxRateBasisPoints != 500 {
		t.Fatalf("published OTA card absent at UTC cutover: %+v %v", current, err)
	}
	var digest, scope string
	if err := db.QueryRow(ctx, `SELECT rate_set_sha256, scope_code FROM ota_pricing_publications
		WHERE pricing_version_id=$1`, draft.ID).Scan(&digest, &scope); err != nil ||
		digest != approval.RateSetSHA256 || scope != "commercial_active_product_ota" {
		t.Fatalf("publication audit missing: digest=%s scope=%s err=%v", digest, scope, err)
	}
	if _, err := store.PublishReviewedOTAPricingVersion(ctx, draft.ID, now, approval); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate publication accepted: %v", err)
	}
}
