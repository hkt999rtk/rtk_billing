package billingstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/billingdocument"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestBillingPersistenceInvoiceLifecycle(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE billing_activity_events, invoice_settlement_links, billing_invoice_documents,
		billing_invoice_lines, billing_invoices, billing_periods, billing_usage_facts, pricing_rates,
		pricing_plan_versions, billing_profiles, balance_ledger_entries, commercial_accounts
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}

	organizationID := testutil.OrganizationID("billing-store")
	account, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, organizationID, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	store := New(db)
	now := time.Date(2026, 8, 17, 2, 0, 0, 0, time.UTC)
	periodStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	quantityScale := 0
	taxCategory := "reviewed-test-category"
	pricing, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{
		PlanKey: "default", Version: 1, Currency: billing.CurrencyTWD, EffectiveFrom: periodStart.AddDate(0, -1, 0),
		CreatedBy: "integration-test", Now: now,
		Rates: []billing.PricingRate{{ServiceCode: "video", MetricCode: "relay_minutes", Description: "Video relay", Unit: "minute", UnitPriceMinor: 2, QuantityScale: &quantityScale, TaxCategory: &taxCategory, RoundingMode: billing.RoundingHalfUp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetPricingVersion(ctx, pricing.ID)
	if err != nil || len(loaded.Rates) != 1 || loaded.Rates[0].QuantityScale == nil || *loaded.Rates[0].QuantityScale != 0 ||
		loaded.Rates[0].TaxCategory == nil || *loaded.Rates[0].TaxCategory != taxCategory {
		t.Fatalf("review metadata roundtrip: pricing=%+v err=%v", loaded, err)
	}
	if _, err := store.ActivatePricingVersion(ctx, pricing.ID, now); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("usage-1")))
	if _, created, err := store.PutUsageFact(ctx, billing.UsageFact{
		UsageID: "usage-1", OrganizationID: organizationID, ServiceCode: "video", MetricCode: "relay_minutes",
		Quantity: 100, Unit: "minute", WindowStart: periodStart, WindowEnd: periodStart.Add(24 * time.Hour), Source: "integration-test", SourceSHA256: digest,
	}); err != nil || !created {
		t.Fatalf("put usage fact: created=%v err=%v", created, err)
	}
	invoice, created, err := store.PrepareInvoice(ctx, PrepareInvoiceInput{
		OrganizationID: organizationID, AccountID: account.ID, Currency: billing.CurrencyTWD,
		PeriodStart: periodStart, PeriodEnd: periodEnd, Now: now,
	})
	if err != nil || !created || invoice.TotalMinor != 200 || invoice.State != billing.InvoiceStateIssued {
		t.Fatalf("prepare invoice: invoice=%+v created=%v err=%v", invoice, created, err)
	}
	data, metadata, err := billingdocument.RenderInvoice(invoice, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutInvoiceDocument(ctx, organizationID, invoice.ID, metadata, data); err != nil {
		t.Fatal(err)
	}
	retroactive, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{
		PlanKey: "default", Version: 2, Currency: billing.CurrencyTWD,
		EffectiveFrom: periodStart.Add(-24 * time.Hour), CreatedBy: "integration-test", Now: now,
		Rates: []billing.PricingRate{{ServiceCode: "video", MetricCode: "relay_minutes", Description: "Video relay", Unit: "minute", UnitPriceMinor: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := store.GetPricingVersion(ctx, retroactive.ID)
	if err != nil || len(legacy.Rates) != 1 || legacy.Rates[0].QuantityScale != nil || legacy.Rates[0].TaxCategory != nil {
		t.Fatalf("legacy rate metadata must remain unknown: pricing=%+v err=%v", legacy, err)
	}
	if _, err := store.ActivatePricingVersion(ctx, retroactive.ID, now); err != ErrConflict {
		t.Fatalf("cutover affecting an issued invoice must conflict, got %v", err)
	}
	settled, err := store.RecordInvoiceSettlement(ctx, organizationID, invoice.ID, "", now)
	if err != nil || settled.State != billing.InvoiceStateSettled {
		t.Fatalf("settle invoice: invoice=%+v err=%v", settled, err)
	}
	if _, created, err := store.PutUsageFact(ctx, billing.UsageFact{
		UsageID: "usage-1", OrganizationID: organizationID, ServiceCode: "video", MetricCode: "relay_minutes",
		Quantity: 100, Unit: "minute", WindowStart: periodStart, WindowEnd: periodStart.Add(24 * time.Hour), Source: "integration-test", SourceSHA256: digest,
	}); err != nil || created {
		t.Fatalf("closed-period idempotent retry: created=%v err=%v", created, err)
	}
	if _, _, err := store.PutUsageFact(ctx, billing.UsageFact{
		UsageID: "late-usage", OrganizationID: organizationID, ServiceCode: "video", MetricCode: "relay_minutes",
		Quantity: 1, Unit: "minute", WindowStart: periodStart, WindowEnd: periodStart.Add(time.Hour), Source: "integration-test", SourceSHA256: digest,
	}); err != ErrInvoiceImmutable {
		t.Fatalf("closed-period usage must be immutable, got %v", err)
	}
}

func TestInvoiceTotalTaxDraftPersistsButCannotActivateWithoutReview(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := New(db)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rate := int64(500)
	category := "standard"
	created, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{
		PlanKey: "invoice-total-tax-draft", Version: 1, Currency: billing.CurrencyTWD,
		EffectiveFrom: start, CreatedBy: "integration-test", Now: start.Add(-time.Hour),
		TaxMode: billing.TaxModeInvoiceTotal, InvoiceTaxRateBasisPoints: &rate,
		InvoiceTaxRoundingMode: billing.RoundingHalfUp, InvoiceTaxCategory: category,
		Rates: []billing.PricingRate{{ServiceCode: "logger", MetricCode: "ingest", Description: "Log ingest", Unit: "requests",
			UnitPriceMinor: 1, RoundingMode: billing.RoundingHalfUp, TaxCategory: &category}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetPricingVersion(ctx, created.ID)
	if err != nil || loaded.TaxMode != billing.TaxModeInvoiceTotal || loaded.InvoiceTaxRateBasisPoints == nil ||
		*loaded.InvoiceTaxRateBasisPoints != rate || loaded.InvoiceTaxCategory != category || loaded.InvoiceTaxRoundingMode != billing.RoundingHalfUp {
		t.Fatalf("tax policy roundtrip: %+v err=%v", loaded, err)
	}
	if _, err := store.ActivatePricingVersion(ctx, created.ID, start.Add(-time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("unreviewed invoice-total draft must not activate: %v", err)
	}
}

func TestPricingHistoryKeepsOldPeriodsAndRejectsAmbiguity(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE billing_activity_events, invoice_settlement_links, billing_invoice_documents,
		billing_invoice_lines, billing_invoices, billing_periods, billing_usage_facts, pricing_rates,
		pricing_plan_versions, billing_profiles, balance_ledger_entries, commercial_accounts
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	store := New(db)
	oldStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	cutover := oldStart.AddDate(0, 1, 0)
	now := cutover.Add(24 * time.Hour)
	create := func(version int64, from time.Time) billing.PricingVersion {
		t.Helper()
		v, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{
			PlanKey: "history", Version: version, Currency: billing.CurrencyTWD,
			EffectiveFrom: from, CreatedBy: "integration-test", Now: now,
			Rates: []billing.PricingRate{{ServiceCode: "mqtt", MetricCode: "publish_count", Description: "Published messages", Unit: "requests", UnitPriceMinor: 32, UnitPriceScale: 6}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	old := create(1, oldStart)
	if _, err := store.ActivatePricingVersion(ctx, old.ID, now); err != nil {
		t.Fatal(err)
	}
	newVersion := create(2, cutover)
	if _, err := store.ActivatePricingVersion(ctx, newVersion.ID, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		at time.Time
		id string
	}{{oldStart, old.ID}, {cutover, newVersion.ID}} {
		got, err := store.ActivePricingVersion(ctx, tc.at, billing.CurrencyTWD)
		if err != nil || got.ID != tc.id {
			t.Fatalf("rate at %s: got=%s want=%s err=%v", tc.at, got.ID, tc.id, err)
		}
	}
	if _, err := store.ActivatePricingVersion(ctx, newVersion.ID, now); err != ErrConflict {
		t.Fatalf("re-activation must conflict, got %v", err)
	}
	if _, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{
		PlanKey: "bad", Version: 1, Currency: billing.CurrencyUSD, EffectiveFrom: now,
		CreatedBy: "integration-test", Now: now,
		Rates: []billing.PricingRate{{ServiceCode: "mqtt", MetricCode: "publish_count", Description: "Published messages", Unit: "requests", UnitPriceMinor: 32, UnitPriceScale: 6}},
	}); err != ErrConflict {
		t.Fatalf("USD pricing must remain unavailable, got %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE pricing_plan_versions SET status='retired', effective_until=$2 WHERE id=$1`, newVersion.ID, now); err != nil {
		t.Fatal(err)
	}
	gapStart := now.Add(48 * time.Hour)
	gap := create(3, gapStart)
	if _, err := store.ActivatePricingVersion(ctx, gap.ID, gapStart.Add(time.Hour)); err != ErrConflict {
		t.Fatalf("activation after an uncovered pricing gap must conflict, got %v", err)
	}
}

func TestFuturePricingCutoverKeepsCurrentMonthAndRejectsAmbiguity(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE billing_activity_events, invoice_settlement_links, billing_invoice_documents,
		billing_invoice_lines, billing_invoices, billing_periods, billing_usage_facts, pricing_rates,
		pricing_plan_versions, billing_profiles, balance_ledger_entries, commercial_accounts
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	store := New(db)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	cutover := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	create := func(version int64, from time.Time, rates []billing.PricingRate) billing.PricingVersion {
		t.Helper()
		v, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{
			PlanKey: "future", Version: version, Currency: billing.CurrencyTWD,
			EffectiveFrom: from, CreatedBy: "integration-test", Now: now, Rates: rates,
		})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	rate := billing.PricingRate{ServiceCode: "mqtt", MetricCode: "publish_count", Description: "Published messages",
		Unit: "requests", UnitPriceMinor: 32, UnitPriceScale: 6}
	old := create(1, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), []billing.PricingRate{rate})
	if _, err := store.ActivatePricingVersion(ctx, old.ID, now); err != nil {
		t.Fatal(err)
	}
	wrongDay := create(2, cutover.Add(24*time.Hour), []billing.PricingRate{rate})
	if _, err := store.ActivatePricingVersion(ctx, wrongDay.ID, now); err != ErrConflict {
		t.Fatalf("future cutover outside UTC month boundary must conflict: %v", err)
	}
	ota := create(3, cutover, append([]billing.PricingRate{rate}, billing.ProposedOTARates()...))
	if _, err := store.ActivatePricingVersion(ctx, ota.ID, now); err != ErrConflict {
		t.Fatalf("OTA publication still requires approved manifest and scope: %v", err)
	}
	newVersion := create(4, cutover, []billing.PricingRate{{ServiceCode: "mqtt", MetricCode: "publish_count", Description: "Published messages",
		Unit: "requests", UnitPriceMinor: 48, UnitPriceScale: 6}})
	if _, err := store.ActivatePricingVersion(ctx, newVersion.ID, now); err != nil {
		t.Fatalf("schedule complete future month: %v", err)
	}
	for _, tc := range []struct {
		at time.Time
		id string
	}{{now, old.ID}, {cutover.Add(-time.Microsecond), old.ID}, {cutover, newVersion.ID}, {cutover.AddDate(0, 1, 0), newVersion.ID}} {
		got, err := store.ActivePricingVersion(ctx, tc.at, billing.CurrencyTWD)
		if err != nil || got.ID != tc.id {
			t.Fatalf("version at %s: got=%s want=%s err=%v", tc.at, got.ID, tc.id, err)
		}
	}
	retired, err := store.GetPricingVersion(ctx, old.ID)
	if err != nil || retired.EffectiveUntil == nil || !retired.EffectiveUntil.Equal(cutover) {
		t.Fatalf("old interval must end exactly at cutover: %+v err=%v", retired, err)
	}
	second := create(5, cutover.AddDate(0, 1, 0), []billing.PricingRate{rate})
	if _, err := store.ActivatePricingVersion(ctx, second.ID, now); err != ErrConflict {
		t.Fatalf("second pending cutover must conflict: %v", err)
	}
}

func TestInvoicePreparationWaitsForPricingPublication(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if db.Config().MaxConns < 2 {
		t.Skip("requires two PostgreSQL connections")
	}
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	publisher, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Rollback(ctx)
	if _, err := publisher.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('billing-pricing-activation'))`); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	_, _, err = New(db).PrepareInvoice(deadline, PrepareInvoiceInput{
		OrganizationID: "00000000-0000-4000-8000-000000000001",
		AccountID:      "00000000-0000-4000-8000-000000000002",
		Currency:       billing.CurrencyTWD,
		PeriodStart:    start,
		PeriodEnd:      start.AddDate(0, 1, 0),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("invoice close must wait for pricing publication transaction: %v", err)
	}
}

func TestReviewOTACandidateUsesCurrentReadOnlyCard(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE billing_activity_events, invoice_settlement_links, billing_invoice_documents,
		billing_invoice_lines, billing_invoices, billing_periods, billing_usage_facts, pricing_rates,
		pricing_plan_versions, billing_profiles, balance_ledger_entries, commercial_accounts
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	store := New(db)
	now := time.Now().UTC()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	cutover := thisMonth.AddDate(0, 1, 0)
	whole := 0
	tax := "reviewed-test-category"
	baseRates := []billing.PricingRate{{ServiceCode: "mqtt", MetricCode: "publish_count", Description: "Published messages",
		Unit: "requests", UnitPriceMinor: 32, UnitPriceScale: 6, QuantityScale: &whole,
		RoundingMode: billing.RoundingHalfUp, TaxCategory: &tax, TaxRateBasisPoints: 500}}
	base, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{PlanKey: "review", Version: 1,
		Currency: billing.CurrencyTWD, EffectiveFrom: thisMonth.AddDate(0, -1, 0),
		CreatedBy: "integration-test", Now: now, Rates: baseRates})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePricingVersion(ctx, base.ID, now); err != nil {
		t.Fatal(err)
	}
	otaRates := billing.ProposedOTARates()
	for i := range otaRates {
		otaRates[i].TaxCategory = &tax
		otaRates[i].TaxRateBasisPoints = 500
	}
	candidate := append(baseRates, otaRates...)
	review, err := store.ReviewOTACandidate(ctx, ReviewOTACandidateInput{BaseVersionID: base.ID,
		EffectiveFrom: cutover, Rates: candidate})
	if err != nil || review.Base.ID != base.ID || len(review.Base.Rates) != 1 || len(review.Review.AddedOTARates) != 4 {
		t.Fatalf("read-only full-card review=%+v err=%v", review, err)
	}
	if _, err := store.ReviewOTACandidate(ctx, ReviewOTACandidateInput{BaseVersionID: "00000000-0000-4000-8000-000000000001",
		EffectiveFrom: cutover, Rates: candidate}); err != ErrConflict {
		t.Fatalf("stale base must fail closed: %v", err)
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM pricing_plan_versions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("review may not create a draft: count=%d err=%v", count, err)
	}
}
