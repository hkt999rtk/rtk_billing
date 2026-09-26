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

func TestEmptyOTASealsDoNotCompleteMixedServiceInvoice(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE ota_period_seals, billing_activity_events, invoice_settlement_links,
		billing_invoice_documents, billing_invoice_lines, billing_invoices, billing_periods,
		billing_usage_facts, pricing_rates, pricing_plan_versions, billing_profiles,
		balance_ledger_entries, commercial_accounts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}

	org := testutil.OrganizationID(t.Name())
	account, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, org, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	store := New(db)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	now := end.Add(time.Hour)
	rates := append(billing.ProposedOTARates(), billing.PricingRate{
		ServiceCode: "mqtt", MetricCode: "publish_count", Description: "MQTT publishes",
		Unit: "requests", UnitPriceMinor: 32, UnitPriceScale: 6, RoundingMode: billing.RoundingHalfUp,
	})
	version, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{
		PlanKey: "mixed-ota-test", Version: 1, Currency: billing.CurrencyTWD,
		EffectiveFrom: start, CreatedBy: "integration-test", Now: now, Rates: rates,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fixture for post-activation close behavior. Production activation remains
	// blocked until P2 can publish a future complete UTC month.
	if _, err := db.Exec(ctx, `UPDATE pricing_plan_versions SET status='active', activated_at=$2 WHERE id=$1`, version.ID, now); err != nil {
		t.Fatal(err)
	}
	platform := testOTAPeriodSeal(OTAIssuerPlatformGrants)
	platform.OrganizationID = org
	producer := testOTAPeriodSeal(OTAIssuerProducer)
	producer.OrganizationID = org
	producer.SealID = "44444444-4444-4444-8444-444444444444"
	for _, seal := range []OTAPeriodSeal{platform, producer} {
		if _, _, err := store.PutOTAPeriodSeal(ctx, seal); err != nil {
			t.Fatal(err)
		}
	}
	closeInput := PrepareInvoiceInput{OrganizationID: org, AccountID: account.ID, Currency: billing.CurrencyTWD,
		PeriodStart: start, PeriodEnd: end, Now: now}
	if invoice, created, err := store.PrepareInvoice(ctx, closeInput); !errors.Is(err, ErrIncomplete) || created {
		t.Fatalf("empty OTA seals closed a mixed-service month: invoice=%+v created=%v err=%v", invoice, created, err)
	}
	var code string
	if err := db.QueryRow(ctx, `SELECT close_error_code FROM billing_periods WHERE organization_id=$1`, org).Scan(&code); err != nil || code != "usage_missing" {
		t.Fatalf("incomplete code=%q err=%v, want usage_missing", code, err)
	}
	var invoices int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM billing_invoices WHERE organization_id=$1`, org).Scan(&invoices); err != nil || invoices != 0 {
		t.Fatalf("invoice count=%d err=%v, want zero", invoices, err)
	}

	if _, created, err := store.PutUsageFact(ctx, billing.UsageFact{
		UsageID: "mqtt-publishes", OrganizationID: org, ServiceCode: "mqtt", MetricCode: "publish_count",
		Quantity: 1_000_000, Unit: "requests", WindowStart: start, WindowEnd: start.Add(time.Minute),
		Source: "mqtt-producer", SourceSHA256: strings.Repeat("a", 64),
	}); err != nil || !created {
		t.Fatalf("MQTT fact created=%v err=%v", created, err)
	}
	invoice, created, err := store.PrepareInvoice(ctx, closeInput)
	if err != nil || !created || invoice.TotalMinor != 32 || len(invoice.Lines) != 1 || invoice.Lines[0].ServiceCode != "mqtt" {
		t.Fatalf("mixed-service close after MQTT fact: invoice=%+v created=%v err=%v", invoice, created, err)
	}
}

func TestPreactivationOTAFactsDoNotBlockMQTTOrBecomeRetroactiveCharges(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE ota_period_seals, billing_activity_events, invoice_settlement_links,
		billing_invoice_documents, billing_invoice_lines, billing_invoices, billing_periods,
		billing_usage_facts, pricing_rates, pricing_plan_versions, billing_profiles,
		balance_ledger_entries, commercial_accounts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	store := New(db)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cutover := start.AddDate(0, 1, 0)
	mqtt := billing.PricingRate{ServiceCode: "mqtt", MetricCode: "publish_count", Description: "MQTT publishes",
		Unit: "requests", UnitPriceMinor: 32, UnitPriceScale: 6, RoundingMode: billing.RoundingHalfUp}
	oldCard, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{
		PlanKey: "preactivation", Version: 1, Currency: billing.CurrencyTWD,
		EffectiveFrom: start, CreatedBy: "integration-test", Now: start, Rates: []billing.PricingRate{mqtt},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePricingVersion(ctx, oldCard.ID, start); err != nil {
		t.Fatal(err)
	}
	newRates := append([]billing.PricingRate{mqtt}, billing.ProposedOTARates()...)
	newCard, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{
		PlanKey: "preactivation", Version: 2, Currency: billing.CurrencyTWD,
		EffectiveFrom: cutover, CreatedBy: "integration-test", Now: cutover, Rates: newRates,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePricingVersion(ctx, newCard.ID, cutover.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("OTA pricing was activated through the immediate API: %v", err)
	}

	makeFacts := func(org string) {
		t.Helper()
		product := "33333333-3333-4333-8333-333333333333"
		for _, fact := range []billing.UsageFact{
			{UsageID: "ota-" + org, OrganizationID: org, ProductID: product, ServiceCode: billing.ServiceOTA,
				MetricCode: billing.MetricOTADeviceTask, Quantity: 1, Unit: billing.UnitOTADeviceTask,
				WindowStart: start, WindowEnd: start.Add(time.Minute), Source: "ota-producer", SourceSHA256: strings.Repeat("a", 64)},
			{UsageID: "mqtt-" + org, OrganizationID: org, ServiceCode: "mqtt", MetricCode: "publish_count",
				Quantity: 1_000_000, Unit: "requests", WindowStart: start, WindowEnd: start.Add(time.Minute),
				Source: "mqtt-producer", SourceSHA256: strings.Repeat("b", 64)},
		} {
			if _, created, err := store.PutUsageFact(ctx, fact); err != nil || !created {
				t.Fatalf("source fact created=%v err=%v", created, err)
			}
		}
	}
	org := testutil.OrganizationID(t.Name())
	account, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, org, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	makeFacts(org)
	closeInput := PrepareInvoiceInput{OrganizationID: org, AccountID: account.ID, Currency: billing.CurrencyTWD,
		PeriodStart: start, PeriodEnd: cutover, Now: cutover.Add(time.Hour)}
	oldInvoice, created, err := store.PrepareInvoice(ctx, closeInput)
	if err != nil || !created || oldInvoice.TotalMinor != 32 || len(oldInvoice.Lines) != 1 || oldInvoice.Lines[0].ServiceCode != "mqtt" {
		t.Fatalf("old mixed invoice=%+v created=%v err=%v", oldInvoice, created, err)
	}
	if _, err := store.GetUsageFact(ctx, "ota-"+org); err != nil {
		t.Fatalf("preactivation OTA fact was lost: %v", err)
	}
	// Simulate P2's later UTC-month publication only to prove that old facts
	// remain tied to the old card. The production activation path is blocked.
	if _, err := db.Exec(ctx, `UPDATE pricing_plan_versions SET status='retired', effective_until=$2 WHERE id=$1`, oldCard.ID, cutover); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE pricing_plan_versions SET status='active', activated_at=$2 WHERE id=$1`, newCard.ID, cutover); err != nil {
		t.Fatal(err)
	}
	if pricing, err := store.ActivePricingVersion(ctx, start, billing.CurrencyTWD); err != nil || pricing.ID != oldCard.ID {
		t.Fatalf("old month pricing=%+v err=%v", pricing, err)
	}
	if pricing, err := store.ActivePricingVersion(ctx, cutover, billing.CurrencyTWD); err != nil || pricing.ID != newCard.ID {
		t.Fatalf("new month pricing=%+v err=%v", pricing, err)
	}
	again, created, err := store.PrepareInvoice(ctx, closeInput)
	if err != nil || created || again.ID != oldInvoice.ID || again.TotalMinor != 32 || len(again.Lines) != 1 {
		t.Fatalf("old issued invoice changed: %+v created=%v err=%v", again, created, err)
	}
	// A still-open old month also stays on the old card after publication.
	secondOrg := testutil.OrganizationID(t.Name() + "-open")
	secondAccount, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, secondOrg, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	makeFacts(secondOrg)
	openInvoice, created, err := store.PrepareInvoice(ctx, PrepareInvoiceInput{OrganizationID: secondOrg,
		AccountID: secondAccount.ID, Currency: billing.CurrencyTWD, PeriodStart: start,
		PeriodEnd: cutover, Now: cutover.Add(2 * time.Hour)})
	if err != nil || !created || openInvoice.TotalMinor != 32 || len(openInvoice.Lines) != 1 || openInvoice.Lines[0].ServiceCode != "mqtt" {
		t.Fatalf("old open month was back-billed: %+v created=%v err=%v", openInvoice, created, err)
	}
	otaOnlyOrg := testutil.OrganizationID(t.Name() + "-ota-only")
	otaOnlyAccount, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, otaOnlyOrg, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.PutUsageFact(ctx, billing.UsageFact{
		UsageID: "ota-only", OrganizationID: otaOnlyOrg, ProductID: "33333333-3333-4333-8333-333333333333",
		ServiceCode: billing.ServiceOTA, MetricCode: billing.MetricOTADeviceTask, Quantity: 1, Unit: billing.UnitOTADeviceTask,
		WindowStart: start, WindowEnd: start.Add(time.Minute), Source: "ota-producer", SourceSHA256: strings.Repeat("a", 64),
	}); err != nil || !created {
		t.Fatalf("OTA-only fact created=%v err=%v", created, err)
	}
	if invoice, created, err := store.PrepareInvoice(ctx, PrepareInvoiceInput{OrganizationID: otaOnlyOrg,
		AccountID: otaOnlyAccount.ID, Currency: billing.CurrencyTWD, PeriodStart: start,
		PeriodEnd: cutover, Now: cutover.Add(2 * time.Hour)}); !errors.Is(err, ErrIncomplete) || created {
		t.Fatalf("unpriced OTA alone closed a mixed-service month: %+v created=%v err=%v", invoice, created, err)
	}
}
