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
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestOTACutoverBridgeClosesReviewedOldPeriodExactlyOnce(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := database.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE ota_cutover_bridges, ota_pricing_publications, ota_pricing_drafts,
		billing_activity_events, invoice_settlement_links, billing_invoice_documents, billing_invoice_lines,
		billing_invoices, billing_periods, billing_usage_facts, pricing_rates, pricing_plan_versions,
		billing_profiles, balance_ledger_entries, commercial_accounts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	store := New(db)
	org := testutil.OrganizationID(t.Name())
	account, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, org, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	future := now.AddDate(0, 2, 0)
	cutover := time.Date(future.Year(), future.Month(), 1, 0, 0, 0, 0, time.UTC)
	period, err := otaBridgePeriod(cutover, "Asia/Taipei")
	if err != nil || !period.PeriodEnd.Equal(cutover) || !period.LocalPeriodEnd.Before(cutover) {
		t.Fatalf("bridge period=%+v err=%v", period, err)
	}
	putOTATestCurrentOwner(t, ctx, db, account.ID, period.PeriodStart.Add(-time.Hour))
	zero := 0
	standard := "standard"
	baseRate := billing.PricingRate{ServiceCode: "mqtt", MetricCode: "publish_count", Description: "MQTT publishes",
		Unit: "requests", UnitPriceMinor: 32, UnitPriceScale: 6, QuantityScale: &zero,
		RoundingMode: billing.RoundingHalfUp, TaxCategory: &standard, TaxRateBasisPoints: 500}
	base, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{PlanKey: "bridge-test", Version: 1,
		Currency: billing.CurrencyTWD, EffectiveFrom: now.Add(-time.Hour),
		CreatedBy: "integration-test", Now: now, Rates: []billing.PricingRate{baseRate}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePricingVersion(ctx, base.ID, now); err != nil {
		t.Fatal(err)
	}
	candidate := append([]billing.PricingRate{baseRate}, billing.ProposedOTARates()...)
	for i := range candidate {
		candidate[i].TaxCategory = &standard
	}
	card, err := store.ReviewOTACandidate(ctx, ReviewOTACandidateInput{
		BaseVersionID: base.ID, EffectiveFrom: cutover, Rates: candidate})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := store.CreateOTAPricingDraft(ctx, CreateOTAPricingDraftInput{
		BaseVersionID: base.ID, EffectiveFrom: cutover, CreatedBy: "integration-test",
		CandidateRates: candidate, ReviewedRateSetSHA256: card.Review.RateSetSHA256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishReviewedOTAPricingVersion(ctx, draft.PricingVersion.ID, now,
		ReviewedOTAPublication{BaseVersionID: base.ID, RateSetSHA256: card.Review.RateSetSHA256,
			FirstReviewer: "finance", SecondReviewer: "billing", ApprovedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	putFact := func(name string, at time.Time) {
		t.Helper()
		if _, created, err := store.PutUsageFact(ctx, billing.UsageFact{
			UsageID: name, OrganizationID: org, ServiceCode: "mqtt", MetricCode: "publish_count",
			Quantity: 1_000_000, Unit: "requests", WindowStart: at, WindowEnd: at.Add(time.Minute),
			Source: "bridge-test", SourceSHA256: strings.Repeat("a", 64),
		}); err != nil || !created {
			t.Fatalf("usage fact %s created=%v err=%v", name, created, err)
		}
	}
	putFact("before-local-boundary", period.PeriodStart.Add(time.Hour))
	putFact("in-local-utc-gap", period.LocalPeriodEnd.Add(time.Hour))
	visible, found, err := store.CurrentOTABridgePeriod(ctx, "Asia/Taipei", period.LocalPeriodEnd.Add(time.Hour))
	if err != nil || !found || visible != period {
		t.Fatalf("usage preview bridge=%+v found=%v err=%v", visible, found, err)
	}
	if _, _, err := store.PrepareInvoice(ctx, PrepareInvoiceInput{OrganizationID: org, AccountID: account.ID,
		Currency: billing.CurrencyTWD, PeriodStart: period.PeriodStart, PeriodEnd: period.LocalPeriodEnd,
		Now: cutover.Add(time.Hour)}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("normal close bypassed reviewed bridge: %v", err)
	}
	review, err := store.ReviewOTACutoverBridge(ctx, org, draft.PricingVersion.ID)
	if err != nil || review.FactCount != 2 || review.SubtotalMinor != 64 || review.ReviewSHA256 == "" {
		t.Fatalf("bridge review=%+v err=%v", review, err)
	}
	input := OTACutoverBridgeInput{OrganizationID: org, PricingVersionID: draft.PricingVersion.ID,
		ReviewSHA256: review.ReviewSHA256, CreatedBy: "operator", Now: now}
	if _, _, err := store.ApplyOTACutoverBridge(ctx, input); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("bridge closed before cutover: %v", err)
	}
	putFact("changed-after-review", period.LocalPeriodEnd.Add(2*time.Hour))
	input.Now = cutover.Add(time.Hour)
	if _, _, err := store.ApplyOTACutoverBridge(ctx, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale review was accepted: %v", err)
	}
	review, err = store.ReviewOTACutoverBridge(ctx, org, draft.PricingVersion.ID)
	if err != nil || review.FactCount != 3 || review.SubtotalMinor != 96 {
		t.Fatalf("updated review=%+v err=%v", review, err)
	}
	input.ReviewSHA256 = review.ReviewSHA256
	issued, created, err := store.ApplyOTACutoverBridge(ctx, input)
	if err != nil || !created || issued.PricingVersionID != base.ID ||
		issued.SubtotalMinor != review.SubtotalMinor || issued.TaxMinor != review.TaxMinor ||
		!issued.PeriodStart.Equal(period.PeriodStart) || !issued.PeriodEnd.Equal(cutover) {
		t.Fatalf("bridged invoice=%+v created=%v err=%v", issued, created, err)
	}
	replayed, created, err := store.ApplyOTACutoverBridge(ctx, input)
	if err != nil || created || replayed.ID != issued.ID {
		t.Fatalf("bridge replay=%+v created=%v err=%v", replayed, created, err)
	}
	input.ReviewSHA256 = strings.Repeat("0", 64)
	if _, _, err := store.ApplyOTACutoverBridge(ctx, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("different review replay was accepted: %v", err)
	}
	ordinary, created, err := store.PrepareInvoice(ctx, PrepareInvoiceInput{OrganizationID: org, AccountID: account.ID,
		Currency: billing.CurrencyTWD, PeriodStart: period.PeriodStart, PeriodEnd: cutover, Now: input.Now})
	if err != nil || created || ordinary.ID != issued.ID {
		t.Fatalf("issued bridge was not readable through ordinary invoice lookup: %+v created=%v err=%v", ordinary, created, err)
	}
	if _, _, err := store.PutUsageFact(ctx, billing.UsageFact{UsageID: "late-after-bridge", OrganizationID: org,
		ServiceCode: "mqtt", MetricCode: "publish_count", Quantity: 1, Unit: "requests",
		WindowStart: period.LocalPeriodEnd.Add(3 * time.Hour), WindowEnd: period.LocalPeriodEnd.Add(3*time.Hour + time.Minute),
		Source: "bridge-test", SourceSHA256: strings.Repeat("b", 64)}); !errors.Is(err, ErrInvoiceImmutable) {
		t.Fatalf("late usage crossed the closed bridge: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE ota_cutover_bridges SET created_by='someone-else'
		WHERE organization_id=$1`, org); err == nil {
		t.Fatal("bridge receipt was mutable")
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ota_cutover_bridges WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 1 {
		t.Fatalf("bridge receipt count=%d err=%v", count, err)
	}
}
