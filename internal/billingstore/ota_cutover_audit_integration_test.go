package billingstore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestAuditOTACutoverFindsTimezoneBridgeAndOwnerEvidence(t *testing.T) {
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

	future := time.Now().UTC().AddDate(0, 2, 0)
	cutover := time.Date(future.Year(), future.Month(), 1, 0, 0, 0, 0, time.UTC)
	owner := testutil.OrganizationID(t.Name() + "/owner")
	type fixture struct {
		org, account, zone string
	}
	makeAccount := func(label, zone string, ownerStart time.Time) fixture {
		t.Helper()
		org := testutil.OrganizationID(t.Name() + "/" + label)
		account, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, org, payment.CurrencyTWD)
		if err != nil {
			t.Fatal(err)
		}
		if zone != "" {
			if _, err := db.Exec(ctx, `INSERT INTO billing_responsibility_periods
				(account_id,owner_user_id,ownership_version,effective_from,source_evidence_sha256)
				VALUES ($1,$2,1,$3,$4)`, account.ID, owner, ownerStart, strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(ctx, `INSERT INTO billing_profiles
				(organization_id,legal_name,timezone,ownership_version,requires_configuration)
				VALUES ($1,'Audit test',$2,1,false)`, org, zone); err != nil {
				t.Fatal(err)
			}
		}
		return fixture{org: org, account: account.ID, zone: zone}
	}
	taipei := makeAccount("taipei", "Asia/Taipei", cutover.AddDate(0, -1, 0))
	losAngeles := makeAccount("los-angeles", "America/Los_Angeles", cutover.Add(time.Hour))
	missing := makeAccount("missing", "", time.Time{})
	taipeiLocation, _ := time.LoadLocation("Asia/Taipei")
	taipeiBoundary := time.Date(cutover.Year(), cutover.Month(), 1, 0, 0, 0, 0, taipeiLocation).UTC()
	if _, err := db.Exec(ctx, `INSERT INTO billing_usage_facts
		(usage_id,organization_id,service_code,metric_code,quantity,unit,window_start,window_end,source,source_sha256)
		VALUES ($1,$2,'mqtt','publish_count',1,'requests',$3,$4,'audit-test',$5)`,
		"audit-taipei", taipei.org, taipeiBoundary.Add(time.Hour), taipeiBoundary.Add(2*time.Hour), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO billing_periods
		(organization_id,currency,period_start,period_end,state) VALUES ($1,'TWD',$2,$3,'closed')`,
		taipei.org, taipeiBoundary.Add(-24*time.Hour), taipeiBoundary.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	report, err := New(db).AuditOTACutover(ctx, cutover)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "technical_inventory_only" || len(report.Accounts) != 3 || !report.EffectiveFrom.Equal(cutover) {
		t.Fatalf("audit summary=%+v", report)
	}
	byDigest := map[string]OTACutoverAccountAudit{}
	for _, item := range report.Accounts {
		byDigest[item.OrganizationSHA256] = item
	}
	item := byDigest[fmt.Sprintf("%x", sha256.Sum256([]byte(taipei.org)))]
	if item.BridgeKind != "gap_risk" || item.OwnerEvidence != "complete_at_cutover" || item.BridgeUsageFacts != 1 ||
		item.BridgeClosedPeriods != 1 || item.TargetMonthPeriods != 0 || item.LocalMonthBoundary == nil || !item.LocalMonthBoundary.Equal(taipeiBoundary) {
		t.Fatalf("Taipei bridge=%+v", item)
	}
	item = byDigest[fmt.Sprintf("%x", sha256.Sum256([]byte(losAngeles.org)))]
	if item.BridgeKind != "overlap_risk" || item.OwnerEvidence != "owner_month_incomplete" || item.LocalMonthBoundary == nil || !item.LocalMonthBoundary.After(cutover) {
		t.Fatalf("Los Angeles bridge=%+v", item)
	}
	item = byDigest[fmt.Sprintf("%x", sha256.Sum256([]byte(missing.org)))]
	if item.BridgeKind != "unknown_profile" || item.OwnerEvidence != "profile_missing" || item.LocalMonthBoundary != nil {
		t.Fatalf("missing profile=%+v", item)
	}
	if _, err := New(db).AuditOTACutover(ctx, cutover.Add(time.Hour)); err != ErrConflict {
		t.Fatalf("non-month cutover error=%v", err)
	}
	if _, err := New(db).AuditOTACutover(ctx, cutover.AddDate(0, -3, 0)); err != ErrConflict {
		t.Fatalf("past cutover error=%v", err)
	}
}
