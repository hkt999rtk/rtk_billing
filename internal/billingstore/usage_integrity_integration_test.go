package billingstore

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestUsageReplayBindsEveryFieldRatherThanTrustingSourceHash(t *testing.T) {
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
	if _, err := db.Exec(ctx, `TRUNCATE billing_usage_facts`); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	start := time.Date(2026, 8, 1, 0, 0, 0, 123456789, time.UTC)
	fact := billing.UsageFact{UsageID: "immutable-usage", OrganizationID: testutil.OrganizationID("immutable-usage-cloud"), ServiceCode: "mqtt", MetricCode: "publish_bytes", Quantity: 12, Unit: "bytes", WindowStart: start, WindowEnd: start.Add(time.Minute), Source: "meter-1", SourceSHA256: strings.Repeat("a", 64)}
	stored, added, err := s.PutUsageFact(ctx, fact)
	if err != nil || !added || stored.ID == "" {
		t.Fatal("insert", stored, added, err)
	}
	replay := fact
	replay.ID = "caller-cannot-select-server-id"
	replay.OrganizationID = strings.ToUpper(fact.OrganizationID)
	replay.SourceSHA256 = strings.ToUpper(fact.SourceSHA256)
	if got, added, err := s.PutUsageFact(ctx, replay); err != nil || added || got.ID != stored.ID {
		t.Fatal("exact replay", got, added, err)
	}
	for name, mutate := range map[string]func(*billing.UsageFact){
		"cloud":          func(f *billing.UsageFact) { f.OrganizationID = testutil.OrganizationID("other-cloud") },
		"service":        func(f *billing.UsageFact) { f.ServiceCode = "video" },
		"metric":         func(f *billing.UsageFact) { f.MetricCode = "delivery_bytes" },
		"quantity":       func(f *billing.UsageFact) { f.Quantity++ },
		"scale":          func(f *billing.UsageFact) { f.QuantityScale++ },
		"unit":           func(f *billing.UsageFact) { f.Unit = "requests" },
		"start":          func(f *billing.UsageFact) { f.WindowStart = f.WindowStart.Add(-time.Second) },
		"end":            func(f *billing.UsageFact) { f.WindowEnd = f.WindowEnd.Add(time.Second) },
		"source":         func(f *billing.UsageFact) { f.Source = "other-meter" },
		"digest":         func(f *billing.UsageFact) { f.SourceSHA256 = strings.Repeat("b", 64) },
		"invalid-digest": func(f *billing.UsageFact) { f.SourceSHA256 = strings.Repeat("g", 64) },
		"invalid-cloud":  func(f *billing.UsageFact) { f.OrganizationID = "not-a-uuid" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := fact
			mutate(&changed)
			if _, _, err := s.PutUsageFact(ctx, changed); !errors.Is(err, ErrConflict) {
				t.Fatal("changed fact acknowledged", err)
			}
		})
	}
	for _, query := range []string{`UPDATE billing_usage_facts SET quantity=13 WHERE usage_id=$1`, `DELETE FROM billing_usage_facts WHERE usage_id=$1`} {
		if _, err := db.Exec(ctx, query, fact.UsageID); err == nil {
			t.Fatal("immutable evidence changed")
		}
	}
	// Same upstream hash with concurrent conflicting payloads can select only
	// one immutable fact, never acknowledge both as equivalent.
	a, b := fact, fact
	a.UsageID = "concurrent-usage"
	b.UsageID = a.UsageID
	b.Quantity++
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, f := range []billing.UsageFact{a, b} {
		wg.Add(1)
		go func(f billing.UsageFact) { defer wg.Done(); _, _, err := s.PutUsageFact(ctx, f); results <- err }(f)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("conflicting writes were acknowledged", success, conflict)
	}
	tiny := fact
	tiny.UsageID = "submicro-window"
	tiny.WindowEnd = tiny.WindowStart.Add(time.Nanosecond)
	if _, _, err := s.PutUsageFact(ctx, tiny); !errors.Is(err, ErrConflict) {
		t.Fatal("collapsed timestamp window", err)
	}
}

func TestOTAUsageFactPreservesOriginalProductGrantEvidence(t *testing.T) {
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
	s := New(db)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	grant := &billing.OTAGrantEvidence{ProductServiceRevision: 3,
		ServiceGrantSHA256: strings.Repeat("a", 64), AuthorizedAt: start.Add(-time.Hour)}
	fact := billing.UsageFact{UsageID: "ota-grant-" + testutil.OrganizationID(t.Name()),
		OrganizationID: testutil.OrganizationID("ota-grant-org-" + t.Name()),
		ProductID:      testutil.OrganizationID("ota-grant-product-" + t.Name()),
		ServiceCode:    billing.ServiceOTA, MetricCode: billing.MetricOTADeviceTask,
		Quantity: 1, Unit: billing.UnitOTADeviceTask,
		WindowStart: start, WindowEnd: start.Add(time.Minute),
		Source: "ota-producer", SourceSHA256: strings.Repeat("b", 64), OTAGrant: grant}
	stored, created, err := s.PutUsageFact(ctx, fact)
	if err != nil || !created || !sameOTAGrant(stored.OTAGrant, grant) {
		t.Fatalf("stored OTA grant: %+v created=%v err=%v", stored, created, err)
	}
	listed, err := s.ListUsageFacts(ctx, fact.OrganizationID, start, start.Add(time.Minute))
	if err != nil || len(listed) != 1 || !sameOTAGrant(listed[0].OTAGrant, grant) {
		t.Fatalf("listed OTA grant: %+v err=%v", listed, err)
	}
	if _, duplicate, err := s.PutUsageFact(ctx, fact); err != nil || duplicate {
		t.Fatalf("exact OTA replay: duplicate=%v err=%v", duplicate, err)
	}
	for name, change := range map[string]func(*billing.OTAGrantEvidence){
		"revision": func(g *billing.OTAGrantEvidence) { g.ProductServiceRevision++ },
		"digest":   func(g *billing.OTAGrantEvidence) { g.ServiceGrantSHA256 = strings.Repeat("c", 64) },
		"time":     func(g *billing.OTAGrantEvidence) { g.AuthorizedAt = g.AuthorizedAt.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := fact
			copyGrant := *grant
			change(&copyGrant)
			changed.OTAGrant = &copyGrant
			if _, _, err := s.PutUsageFact(ctx, changed); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed OTA grant was accepted: %v", err)
			}
		})
	}
	if _, err := db.Exec(ctx, `UPDATE billing_usage_facts SET ota_grant_revision=4 WHERE usage_id=$1`, fact.UsageID); err == nil {
		t.Fatal("stored OTA grant was mutable")
	}
	if _, err := db.Exec(ctx, `INSERT INTO billing_usage_facts
		(usage_id,organization_id,product_id,service_code,metric_code,quantity,quantity_scale,unit,
		 window_start,window_end,source,source_sha256,ota_grant_revision)
		VALUES($1,$2,$3,'ota','device_task',1,0,'tasks',$4,$5,'ota-producer',$6,3)`,
		fact.UsageID+"-partial", fact.OrganizationID, fact.ProductID, start, start.Add(time.Minute), fact.SourceSHA256); err == nil {
		t.Fatal("database accepted a partial OTA grant witness")
	}
}
