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

func TestReviewedOTALoggerPublicationPreservesLegacyBaseAndInvoices(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	if _, err := db.Exec(ctx, `TRUNCATE ota_pricing_publications,ota_pricing_drafts,
		billing_activity_events,invoice_settlement_links,billing_invoice_documents,billing_invoice_lines,
		billing_invoices,billing_periods,billing_usage_facts,pricing_rates,pricing_plan_versions,
		billing_profiles,balance_ledger_entries,commercial_accounts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	store := New(db)
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	cutover := month.AddDate(0, 1, 0)
	zero, fractional := 0, 9
	standard := "standard"
	baseRates := []billing.PricingRate{
		{ServiceCode: "mqtt", MetricCode: "publish_count", Unit: "requests", Description: "MQTT publishes", UnitPriceMinor: 32, UnitPriceScale: 6, RoundingMode: billing.RoundingHalfUp, TaxRateBasisPoints: 500},
		{ServiceCode: "shadow", MetricCode: "read_count", Unit: "requests", Description: "Shadow reads", UnitPriceMinor: 48, UnitPriceScale: 6, RoundingMode: billing.RoundingHalfUp, TaxRateBasisPoints: 500},
		{ServiceCode: "shadow", MetricCode: "write_count", Unit: "requests", Description: "Shadow writes", UnitPriceMinor: 64, UnitPriceScale: 6, RoundingMode: billing.RoundingHalfUp, TaxRateBasisPoints: 500},
		{ServiceCode: "webrtc", MetricCode: "relay_minutes", Unit: "minutes", Description: "WebRTC relay", UnitPriceMinor: 9, UnitPriceScale: 2, RoundingMode: billing.RoundingHalfUp, TaxRateBasisPoints: 500},
		{ServiceCode: "storage", MetricCode: "clip_gib_month", Unit: "GiB-month", Description: "Clip storage", UnitPriceMinor: 131, UnitPriceScale: 2, RoundingMode: billing.RoundingHalfUp, TaxRateBasisPoints: 500},
	}
	base, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{PlanKey: "legacy-logger-additions", Version: 1, Currency: billing.CurrencyTWD, EffectiveFrom: month.AddDate(0, -3, 0), CreatedBy: "test", Now: now, Rates: baseRates})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePricingVersion(ctx, base.ID, now); err != nil {
		t.Fatal(err)
	}
	periodStart := month.AddDate(0, -2, 0)
	periodEnd := periodStart.AddDate(0, 1, 0)
	for i := 0; i < 4; i++ {
		org := testutil.OrganizationID(t.Name() + string(rune('a'+i)))
		account, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, org, payment.CurrencyTWD)
		if err != nil {
			t.Fatal(err)
		}
		putOTATestCurrentOwner(t, ctx, db, account.ID, periodStart.Add(-time.Hour))
		if _, _, err := store.PutUsageFact(ctx, billing.UsageFact{UsageID: org + "/mqtt", OrganizationID: org, ServiceCode: "mqtt", MetricCode: "publish_count", Quantity: 1_000_000, Unit: "requests", WindowStart: periodStart, WindowEnd: periodStart.Add(time.Minute), Source: "mqtt-test", SourceSHA256: strings.Repeat("a", 64)}); err != nil {
			t.Fatal(err)
		}
		if _, created, err := store.PrepareInvoice(ctx, PrepareInvoiceInput{OrganizationID: org, AccountID: account.ID, Currency: billing.CurrencyTWD, PeriodStart: periodStart, PeriodEnd: periodEnd, Now: now}); err != nil || !created {
			t.Fatalf("historical invoice created=%v err=%v", created, err)
		}
	}
	var historical, oldRates string
	if err := db.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(i) ORDER BY id)::text FROM billing_invoices i`).Scan(&historical); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(r) ORDER BY id)::text FROM pricing_rates r WHERE pricing_version_id=$1`, base.ID).Scan(&oldRates); err != nil {
		t.Fatal(err)
	}
	candidate := append([]billing.PricingRate{}, base.Rates...)
	for i := range candidate {
		candidate[i].TaxCategory = &standard
		candidate[i].QuantityScale = &zero
		if candidate[i].Unit == "GiB-month" {
			candidate[i].QuantityScale = &fractional
		}
	}
	for _, rate := range billing.ProposedOTARates() {
		rate.TaxCategory = &standard
		candidate = append(candidate, rate)
	}
	candidate = append(candidate,
		billing.PricingRate{ServiceCode: billing.ServiceLogger, MetricCode: billing.MetricLoggerIngest, Unit: "GiB", Description: "Device and application log ingest", UnitPriceMinor: 2880, UnitPriceScale: 2, QuantityScale: &fractional, RoundingMode: billing.RoundingHalfUp, TaxCategory: &standard},
		billing.PricingRate{ServiceCode: billing.ServiceLogger, MetricCode: billing.MetricLoggerRetained, Unit: "GiB-month", Description: "Device and application log retention", UnitPriceMinor: 131, UnitPriceScale: 2, QuantityScale: &fractional, RoundingMode: billing.RoundingHalfUp, TaxCategory: &standard})
	const reference = "finance/2026-10-02/logger-reference-rates"
	reviewInput := ReviewOTACandidateInput{BaseVersionID: base.ID, EffectiveFrom: cutover, Rates: candidate}
	if _, err := store.ReviewOTACandidate(ctx, reviewInput); !errors.Is(err, billing.ErrOTACardReview) {
		t.Fatalf("unapproved pair passed review: %v", err)
	}
	reviewInput.LoggerApprovalReference = reference
	review, err := store.ReviewOTACandidate(ctx, reviewInput)
	if err != nil {
		t.Fatal(err)
	}
	draftInput := CreateOTAPricingDraftInput{BaseVersionID: base.ID, EffectiveFrom: cutover, CreatedBy: "operator", CandidateRates: candidate, ReviewedRateSetSHA256: review.Review.RateSetSHA256, LoggerApprovalReference: reference, LoggerRateSetSHA256: review.Review.LoggerRateSetSHA256}
	for name, mutate := range map[string]func(*CreateOTAPricingDraftInput){
		"missing approval":    func(in *CreateOTAPricingDraftInput) { in.LoggerApprovalReference = "" },
		"missing pair digest": func(in *CreateOTAPricingDraftInput) { in.LoggerRateSetSHA256 = "" },
		"changed pair digest": func(in *CreateOTAPricingDraftInput) { in.LoggerRateSetSHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := draftInput
			mutate(&bad)
			if _, err := store.CreateOTAPricingDraft(ctx, bad); !errors.Is(err, ErrConflict) {
				t.Fatalf("unreviewed draft accepted: %v", err)
			}
		})
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ota_pricing_drafts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed drafts wrote manifests: %d %v", count, err)
	}
	draft, err := store.CreateOTAPricingDraft(ctx, draftInput)
	if err != nil {
		t.Fatal(err)
	}
	if draft.LoggerApprovalReference != reference || draft.LoggerRateSetSHA256 != review.Review.LoggerRateSetSHA256 || len(draft.PricingVersion.Rates) != 11 {
		t.Fatalf("Logger manifest missing: %+v", draft)
	}
	if _, err := store.ActivatePricingVersion(ctx, draft.PricingVersion.ID, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("generic activation accepted reviewed pair: %v", err)
	}
	approval := ReviewedOTAPublication{BaseVersionID: base.ID, RateSetSHA256: review.Review.RateSetSHA256, FirstReviewer: "finance", SecondReviewer: "billing", ApprovedAt: now.Add(-time.Minute), LoggerApprovalReference: reference, LoggerRateSetSHA256: review.Review.LoggerRateSetSHA256}
	for name, mutate := range map[string]func(*ReviewedOTAPublication){
		"missing reference":   func(in *ReviewedOTAPublication) { in.LoggerApprovalReference = "" },
		"different reference": func(in *ReviewedOTAPublication) { in.LoggerApprovalReference = "unrecorded/approval" },
		"missing digest":      func(in *ReviewedOTAPublication) { in.LoggerRateSetSHA256 = "" },
		"different digest":    func(in *ReviewedOTAPublication) { in.LoggerRateSetSHA256 = strings.Repeat("0", 64) },
	} {
		t.Run("publication/"+name, func(t *testing.T) {
			bad := approval
			mutate(&bad)
			if _, err := store.PublishReviewedOTAPricingVersion(ctx, draft.PricingVersion.ID, now, bad); !errors.Is(err, ErrConflict) {
				t.Fatalf("unapproved publication accepted: %v", err)
			}
		})
	}
	if _, err := store.PublishReviewedOTAPricingVersion(ctx, draft.PricingVersion.ID, now, approval); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := db.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(i) ORDER BY id)::text FROM billing_invoices i`).Scan(&after); err != nil || after != historical {
		t.Fatalf("publication changed historical invoices: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(r) ORDER BY id)::text FROM pricing_rates r WHERE pricing_version_id=$1`, base.ID).Scan(&after); err != nil || after != oldRates {
		t.Fatalf("publication changed legacy rate rows: %v", err)
	}
	for _, table := range []string{"ota_pricing_drafts", "ota_pricing_publications"} {
		var recordedReference, recordedDigest string
		if err := db.QueryRow(ctx, `SELECT logger_approval_reference,logger_rate_set_sha256 FROM `+table+` WHERE pricing_version_id=$1`, draft.PricingVersion.ID).Scan(&recordedReference, &recordedDigest); err != nil || recordedReference != reference || recordedDigest != approval.LoggerRateSetSHA256 {
			t.Fatalf("%s approval missing: %q %q %v", table, recordedReference, recordedDigest, err)
		}
		if _, err := db.Exec(ctx, `UPDATE `+table+` SET logger_approval_reference='changed' WHERE pricing_version_id=$1`, draft.PricingVersion.ID); err == nil {
			t.Fatalf("%s Logger approval mutable", table)
		}
	}
	current, err := store.ActivePricingVersion(ctx, now, billing.CurrencyTWD)
	if err != nil || current.ID != base.ID {
		t.Fatalf("new card took effect before UTC cutover: %+v %v", current, err)
	}
	current, err = store.ActivePricingVersion(ctx, cutover, billing.CurrencyTWD)
	if err != nil || current.ID != draft.PricingVersion.ID || current.TaxMode != billing.TaxModeInvoiceTotal || current.InvoiceTaxRateBasisPoints == nil || *current.InvoiceTaxRateBasisPoints != 500 {
		t.Fatalf("new card/tax missing at UTC cutover: %+v %v", current, err)
	}
}
