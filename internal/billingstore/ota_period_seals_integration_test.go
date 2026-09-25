package billingstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestOTAPeriodSealGatesInvoiceAndRejectsChangedReplay(t *testing.T) {
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
	product := "33333333-3333-4333-8333-333333333333"
	account, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, org, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	store := New(db)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	now := end.Add(time.Hour)
	version, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{
		PlanKey: "ota-test", Version: 1, Currency: billing.CurrencyTWD,
		EffectiveFrom: start, CreatedBy: "integration-test", Now: now,
		Rates: billing.ProposedOTARates(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePricingVersion(ctx, version.ID, now); err != nil {
		t.Fatal(err)
	}
	fact := billing.UsageFact{
		UsageID: "ota-download-1", OrganizationID: org, ProductID: product,
		ServiceCode: billing.ServiceOTA, MetricCode: billing.MetricOTASuccessfulDownloadGiB,
		Quantity: 2_000_000_000, QuantityScale: 9, Unit: billing.UnitOTAGiB,
		WindowStart: start.Add(time.Minute), WindowEnd: start.Add(2 * time.Minute),
		Source: "ota-producer", SourceSHA256: strings.Repeat("a", 64),
	}
	for name, mutate := range map[string]func(*billing.UsageFact){
		"missing-product": func(f *billing.UsageFact) { f.ProductID = "" },
		"wrong-scale":     func(f *billing.UsageFact) { f.QuantityScale = 0 },
		"unknown-meter":   func(f *billing.UsageFact) { f.MetricCode = "object_read" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := fact
			invalid.UsageID = "ota-invalid-" + name
			mutate(&invalid)
			if _, _, err := store.PutUsageFact(ctx, invalid); !errors.Is(err, ErrConflict) {
				t.Fatalf("invalid OTA fact accepted: %v", err)
			}
		})
	}
	if _, created, err := store.PutUsageFact(ctx, fact); err != nil || !created {
		t.Fatalf("put OTA fact: created=%v err=%v", created, err)
	}
	closeInput := PrepareInvoiceInput{OrganizationID: org, AccountID: account.ID, Currency: billing.CurrencyTWD,
		PeriodStart: start, PeriodEnd: end, Now: now}
	assertIncomplete := func() {
		t.Helper()
		if _, _, err := store.PrepareInvoice(ctx, closeInput); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("unsealed close error=%v, want incomplete", err)
		}
		var code string
		if err := db.QueryRow(ctx, `SELECT close_error_code FROM billing_periods WHERE organization_id=$1`, org).Scan(&code); err != nil || code != "ota_source_incomplete" {
			t.Fatalf("incomplete code=%s err=%v", code, err)
		}
	}
	assertIncomplete()

	platform := testOTAPeriodSeal(OTAIssuerPlatformGrants)
	platform.OrganizationID = org
	platform.ProductIDs = []string{product}
	if _, created, err := store.PutOTAPeriodSeal(ctx, platform); err != nil || !created {
		t.Fatalf("platform seal insert: created=%v err=%v", created, err)
	}
	if _, created, err := store.PutOTAPeriodSeal(ctx, platform); err != nil || created {
		t.Fatalf("platform seal replay: created=%v err=%v", created, err)
	}
	changed := platform
	changed.SourceSHA256 = strings.Repeat("b", 64)
	if _, _, err := store.PutOTAPeriodSeal(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed seal accepted: %v", err)
	}
	assertIncomplete()

	producer := testOTAPeriodSeal(OTAIssuerProducer)
	producer.OrganizationID = org
	producer.SealID = "44444444-4444-4444-8444-444444444444"
	producer.ProductIDs = []string{product}
	producer.MetricCounts[billing.MetricOTASuccessfulDownloadGiB] = 1
	entry, err := json.Marshal([]any{fact.UsageID, product, fact.MetricCode, fact.Quantity,
		fact.QuantityScale, fact.Unit, fact.WindowStart.Format(time.RFC3339Nano),
		fact.WindowEnd.Format(time.RFC3339Nano), fact.SourceSHA256})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(append(append([]byte("["), entry...), ']'))
	factSHA := hex.EncodeToString(digest[:])
	producer.FactSetSHA256 = &factSHA
	if _, created, err := store.PutOTAPeriodSeal(ctx, producer); err != nil || !created {
		t.Fatalf("producer seal insert: created=%v err=%v", created, err)
	}
	invoice, created, err := store.PrepareInvoice(ctx, closeInput)
	if err != nil || !created || invoice.TotalMinor != 2 || len(invoice.Lines) != 1 || invoice.Lines[0].ProductID != product {
		t.Fatalf("attested close: created=%v invoice=%+v err=%v", created, invoice, err)
	}
	late := fact
	late.UsageID = "ota-download-late"
	if _, _, err := store.PutUsageFact(ctx, late); !errors.Is(err, ErrInvoiceImmutable) {
		t.Fatalf("late OTA fact accepted: %v", err)
	}

	// Explicit zero-use seals permit a zero-total close, including a Product
	// that had an OTA grant but emitted no usage fact.
	zeroOrg := testutil.OrganizationID(t.Name() + "/zero")
	zeroAccount, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, zeroOrg, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	zeroPlatform := testOTAPeriodSeal(OTAIssuerPlatformGrants)
	zeroPlatform.OrganizationID = zeroOrg
	zeroPlatform.SealID = "55555555-5555-4555-8555-555555555555"
	zeroPlatform.ProductIDs = []string{product}
	zeroProducer := testOTAPeriodSeal(OTAIssuerProducer)
	zeroProducer.OrganizationID = zeroOrg
	zeroProducer.SealID = "66666666-6666-4666-8666-666666666666"
	for _, seal := range []OTAPeriodSeal{zeroPlatform, zeroProducer} {
		if _, _, err := store.PutOTAPeriodSeal(ctx, seal); err != nil {
			t.Fatal(err)
		}
	}
	zeroInvoice, created, err := store.PrepareInvoice(ctx, PrepareInvoiceInput{
		OrganizationID: zeroOrg, AccountID: zeroAccount.ID, Currency: billing.CurrencyTWD,
		PeriodStart: start, PeriodEnd: end, Now: now,
	})
	if err != nil || !created || zeroInvoice.TotalMinor != 0 || len(zeroInvoice.Lines) != 0 {
		t.Fatalf("zero-use close: created=%v invoice=%+v err=%v", created, zeroInvoice, err)
	}

	// A source that claims an empty fact set with the wrong digest cannot close.
	badOrg := testutil.OrganizationID(t.Name() + "/bad")
	badAccount, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, badOrg, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	zeroStorageFact := billing.UsageFact{UsageID: "ota-zero-storage", OrganizationID: badOrg, ProductID: product,
		ServiceCode: billing.ServiceOTA, MetricCode: billing.MetricOTAArtifactStorageGiBMonth,
		Quantity: 0, QuantityScale: 9, Unit: billing.UnitOTAGiBMonth,
		WindowStart: start, WindowEnd: end, Source: "ota-producer", SourceSHA256: strings.Repeat("a", 64)}
	if _, created, err := store.PutUsageFact(ctx, zeroStorageFact); err != nil || !created {
		t.Fatalf("zero-rounded storage fact: created=%v err=%v", created, err)
	}
	badPlatform := zeroPlatform
	badPlatform.OrganizationID = badOrg
	badPlatform.SealID = "77777777-7777-4777-8777-777777777777"
	badProducer := zeroProducer
	badProducer.OrganizationID = badOrg
	badProducer.SealID = "88888888-8888-4888-8888-888888888888"
	badProducer.ProductIDs = []string{product}
	badProducer.MetricCounts = map[string]int64{}
	for _, metric := range otaMetricCodes {
		badProducer.MetricCounts[metric] = 0
	}
	badProducer.MetricCounts[billing.MetricOTAArtifactStorageGiBMonth] = 1
	badDigest := strings.Repeat("f", 64)
	badProducer.FactSetSHA256 = &badDigest
	for _, seal := range []OTAPeriodSeal{badPlatform, badProducer} {
		if _, _, err := store.PutOTAPeriodSeal(ctx, seal); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.PrepareInvoice(ctx, PrepareInvoiceInput{
		OrganizationID: badOrg, AccountID: badAccount.ID, Currency: billing.CurrencyTWD,
		PeriodStart: start, PeriodEnd: end, Now: now,
	}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("mismatched digest close error=%v, want incomplete", err)
	}
}
