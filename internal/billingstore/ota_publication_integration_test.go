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
		UnitPriceScale: 6, RoundingMode: billing.RoundingHalfUp, TaxRateBasisPoints: 500}
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
	rates[0].QuantityScale = &whole
	for i := range rates {
		rates[i].TaxCategory = &standard
	}
	review, err := store.ReviewOTACandidate(ctx, ReviewOTACandidateInput{
		BaseVersionID: base.ID, EffectiveFrom: cutover, Rates: rates})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOTAPricingDraft(ctx, CreateOTAPricingDraftInput{
		BaseVersionID: base.ID, EffectiveFrom: cutover, CreatedBy: "integration-test"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy base without reviewed metadata must fail: %v", err)
	}
	if _, err := store.CreateOTAPricingDraft(ctx, CreateOTAPricingDraftInput{
		BaseVersionID: base.ID, EffectiveFrom: cutover, CreatedBy: "integration-test",
		CandidateRates: rates, ReviewedRateSetSHA256: strings.Repeat("0", 64)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("unreviewed candidate digest must fail: %v", err)
	}
	if _, err := store.CreateOTAPricingDraft(ctx, CreateOTAPricingDraftInput{
		BaseVersionID: base.ID, EffectiveFrom: cutover.Add(time.Hour), CreatedBy: "integration-test"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("mid-month OTA draft accepted: %v", err)
	}
	if _, err := store.CreateOTAPricingDraft(ctx, CreateOTAPricingDraftInput{
		BaseVersionID: testutil.OrganizationID("stale-base"), EffectiveFrom: cutover, CreatedBy: "integration-test"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale OTA base accepted: %v", err)
	}
	var draftCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ota_pricing_drafts`).Scan(&draftCount); err != nil || draftCount != 0 {
		t.Fatalf("failed validation left a draft: count=%d err=%v", draftCount, err)
	}
	created, err := store.CreateOTAPricingDraft(ctx, CreateOTAPricingDraftInput{
		BaseVersionID: base.ID, EffectiveFrom: cutover, CreatedBy: "integration-test",
		CandidateRates: rates, ReviewedRateSetSHA256: review.Review.RateSetSHA256})
	if err != nil {
		t.Fatal(err)
	}
	draft := created.PricingVersion
	if created.RateSetSHA256 != review.Review.RateSetSHA256 || created.BaseVersionID != base.ID ||
		len(draft.Rates) != len(rates) {
		t.Fatalf("atomic draft does not match reviewed complete card: %+v", created)
	}
	if _, err := db.Exec(ctx, `UPDATE ota_pricing_drafts SET rate_set_sha256=$2
		WHERE pricing_version_id=$1`, draft.ID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("draft manifest was mutable")
	}
	approval := ReviewedOTAPublication{BaseVersionID: base.ID, RateSetSHA256: review.Review.RateSetSHA256,
		FirstReviewer: "finance-reviewer", SecondReviewer: "billing-reviewer", ApprovedAt: now.Add(-time.Minute)}
	if _, err := store.ActivatePricingVersion(ctx, draft.ID, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("generic activation accepted OTA: %v", err)
	}
	taxRate := int64(500)
	unreviewedDraft, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{PlanKey: "ota-reviewed-test", Version: 4,
		Currency: billing.CurrencyTWD, EffectiveFrom: cutover, CreatedBy: "integration-test", Now: now,
		Rates: rates, TaxMode: billing.TaxModeInvoiceTotal, InvoiceTaxRateBasisPoints: &taxRate,
		InvoiceTaxRoundingMode: billing.RoundingHalfUp, InvoiceTaxCategory: standard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishReviewedOTAPricingVersion(ctx, unreviewedDraft.ID, now, approval); !errors.Is(err, ErrConflict) {
		t.Fatalf("unrecorded OTA draft accepted: %v", err)
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
	if err := db.QueryRow(ctx, `SELECT rate_set_sha256, scope_code FROM ota_pricing_drafts
		WHERE pricing_version_id=$1`, draft.ID).Scan(&digest, &scope); err != nil ||
		digest != approval.RateSetSHA256 || scope != "commercial_active_product_ota" {
		t.Fatalf("draft manifest missing: digest=%s scope=%s err=%v", digest, scope, err)
	}
	if _, err := store.PublishReviewedOTAPricingVersion(ctx, draft.ID, now, approval); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate publication accepted: %v", err)
	}
	cancel := ReviewedOTACancellation{BaseVersionID: base.ID, FirstReviewer: "finance-reviewer",
		SecondReviewer: "billing-reviewer", Reason: "CDN qualification failed", ApprovedAt: now.Add(-time.Minute)}
	if _, err := store.CancelReviewedOTAPricingVersion(ctx, draft.ID, cutover, cancel); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancellation at cutover accepted: %v", err)
	}
	badCancel := cancel
	badCancel.SecondReviewer = badCancel.FirstReviewer
	if _, err := store.CancelReviewedOTAPricingVersion(ctx, draft.ID, now, badCancel); !errors.Is(err, ErrConflict) {
		t.Fatalf("single-reviewer cancellation accepted: %v", err)
	}
	badCancel = cancel
	badCancel.BaseVersionID = unreviewedDraft.ID
	if _, err := store.CancelReviewedOTAPricingVersion(ctx, draft.ID, now, badCancel); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-base cancellation accepted: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO billing_periods
		(organization_id,currency,period_start,period_end,state)
		VALUES ($1,'TWD',$2,$3,'incomplete')`, testutil.OrganizationID("future-period"),
		cutover, cutover.AddDate(0, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CancelReviewedOTAPricingVersion(ctx, draft.ID, now, cancel); !errors.Is(err, ErrConflict) {
		t.Fatalf("future financial period did not block cancellation: %v", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM billing_periods WHERE organization_id=$1`,
		testutil.OrganizationID("future-period")); err != nil {
		t.Fatal(err)
	}
	canceled, err := store.CancelReviewedOTAPricingVersion(ctx, draft.ID, now, cancel)
	if err != nil || canceled.Status != "canceled" {
		t.Fatalf("reviewed cancellation failed: %+v %v", canceled, err)
	}
	current, err = store.ActivePricingVersion(ctx, cutover, billing.CurrencyTWD)
	if err != nil || current.ID != base.ID {
		t.Fatalf("base card was not restored across cutover: %+v %v", current, err)
	}
	if _, err := store.UpcomingPricingVersion(ctx, now, billing.CurrencyTWD); !errors.Is(err, ErrPricingUnavailable) {
		t.Fatalf("canceled card remained upcoming: %v", err)
	}
	var cancellationCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ota_pricing_cancellations WHERE pricing_version_id=$1`,
		draft.ID).Scan(&cancellationCount); err != nil || cancellationCount != 1 {
		t.Fatalf("cancellation audit missing: count=%d err=%v", cancellationCount, err)
	}
	if _, err := db.Exec(ctx, `UPDATE ota_pricing_cancellations SET reason='changed' WHERE pricing_version_id=$1`, draft.ID); err == nil {
		t.Fatal("cancellation approval record was mutable")
	}
	if _, err := db.Exec(ctx, `DELETE FROM ota_pricing_publications WHERE pricing_version_id=$1`, draft.ID); err == nil {
		t.Fatal("publication approval record was deletable")
	}
	if _, err := store.CancelReviewedOTAPricingVersion(ctx, draft.ID, now, cancel); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate cancellation accepted: %v", err)
	}
}
