package billingstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

func loggerSealFixture(t *testing.T, mixed bool) (context.Context, *pgxpool.Pool, *Store, PrepareInvoiceInput, []billing.UsageFact) {
	t.Helper()
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
	t.Cleanup(cancel)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `TRUNCATE logger_period_seals,ota_period_seals,billing_activity_events,invoice_settlement_links,billing_invoice_documents,billing_invoice_lines,billing_invoices,billing_periods,billing_usage_facts,pricing_rates,pricing_plan_versions,billing_profiles,balance_ledger_entries,commercial_accounts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	org := testutil.OrganizationID(t.Name())
	account, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, org, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	store := New(db)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	putOTATestCurrentOwner(t, ctx, db, account.ID, start)
	scale := 9
	rates := []billing.PricingRate{{ServiceCode: billing.ServiceLogger, MetricCode: billing.MetricLoggerIngest, Unit: "GiB", Description: "Logger ingest", QuantityScale: &scale, UnitPriceMinor: 10, RoundingMode: billing.RoundingHalfUp},
		{ServiceCode: billing.ServiceLogger, MetricCode: billing.MetricLoggerRetained, Unit: "GiB-month", Description: "Logger retention", QuantityScale: &scale, UnitPriceMinor: 4, RoundingMode: billing.RoundingHalfUp}}
	if mixed {
		rates = append(rates, billing.PricingRate{ServiceCode: "mqtt", MetricCode: "publish_count", Unit: "requests", Description: "MQTT", UnitPriceMinor: 2, RoundingMode: billing.RoundingHalfUp})
	}
	pricing, err := store.CreatePricingVersion(ctx, CreatePricingVersionInput{PlanKey: "logger-seals", Version: 1, Currency: billing.CurrencyTWD, EffectiveFrom: start, CreatedBy: "test", Now: start, Rates: rates})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePricingVersion(ctx, pricing.ID, start); err != nil {
		t.Fatal(err)
	}
	in := PrepareInvoiceInput{OrganizationID: org, AccountID: account.ID, Currency: billing.CurrencyTWD, PeriodStart: start, PeriodEnd: end, Now: end.Add(25 * time.Hour)}
	product := "33333333-3333-4333-8333-333333333333"
	facts := []billing.UsageFact{{UsageID: "logger-ingest", OrganizationID: org, ProductID: product, ServiceCode: billing.ServiceLogger, MetricCode: billing.MetricLoggerIngest, Quantity: 1_000_000_000, QuantityScale: 9, Unit: "GiB", WindowStart: start, WindowEnd: end, Source: "video-cloud-logger-receipts", SourceSHA256: strings.Repeat("a", 64)},
		{UsageID: "logger-retained", OrganizationID: org, ProductID: product, ServiceCode: billing.ServiceLogger, MetricCode: billing.MetricLoggerRetained, Quantity: 1_000_000_000, QuantityScale: 9, Unit: "GiB-month", WindowStart: start, WindowEnd: end, Source: "video-cloud-logger-receipts", SourceSHA256: strings.Repeat("b", 64)}}
	return ctx, db, store, in, facts
}

func loggerSealForFacts(in PrepareInvoiceInput, facts []billing.UsageFact) LoggerPeriodSeal {
	seal := testLoggerSeal()
	seal.OrganizationID = in.OrganizationID
	seal.ProductIDs = []string{}
	entries := [][]any{}
	ordered := append([]billing.UsageFact{}, facts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].UsageID < ordered[j].UsageID })
	products := map[string]bool{}
	for _, fact := range ordered {
		products[fact.ProductID] = true
		entries = append(entries, []any{fact.UsageID, fact.ProductID, fact.MetricCode, fact.Quantity, fact.QuantityScale, fact.Unit, fact.WindowStart.UTC().Format(time.RFC3339Nano), fact.WindowEnd.UTC().Format(time.RFC3339Nano), fact.SourceSHA256})
	}
	for product := range products {
		seal.ProductIDs = append(seal.ProductIDs, product)
	}
	sort.Strings(seal.ProductIDs)
	seal.MetricCounts = map[string]int64{billing.MetricLoggerIngest: int64(len(products)), billing.MetricLoggerRetained: int64(len(products))}
	var highWater LoggerSourceHighWater
	_ = json.Unmarshal(seal.SourceHighWater, &highWater)
	highWater.ReceiptCount, highWater.MaxReceiptSequence = int64(len(products)), int64(len(products))
	seal.SourceHighWater, _ = json.Marshal(highWater)
	if len(products) > 0 {
		seal.SourceSHA256 = strings.Repeat("b", 64)
	}
	raw, _ := json.Marshal(entries)
	digest := sha256.Sum256(raw)
	seal.FactSetSHA256 = hex.EncodeToString(digest[:])
	return seal
}

func TestLoggerSourceSealGatesMixedInvoiceAndRejectsPartialChangedLateEvidence(t *testing.T) {
	ctx, db, store, in, facts := loggerSealFixture(t, true)
	if _, _, err := store.PutUsageFact(ctx, billing.UsageFact{UsageID: "mqtt-first", OrganizationID: in.OrganizationID, ServiceCode: "mqtt", MetricCode: "publish_count", Quantity: 10, Unit: "requests", WindowStart: in.PeriodStart, WindowEnd: in.PeriodStart.Add(time.Minute), Source: "mqtt", SourceSHA256: strings.Repeat("c", 64)}); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.PrepareInvoice(ctx, in); !errors.Is(err, ErrIncomplete) || created {
		t.Fatal("single other-service fact closed Logger month", created, err)
	}
	seal := loggerSealForFacts(in, facts)
	if _, _, err := store.PutUsageFact(ctx, facts[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PutLoggerPeriodSeal(ctx, seal); !errors.Is(err, ErrIncomplete) {
		t.Fatal("one metric certified complete", err)
	}
	if _, created, err := store.PrepareInvoice(ctx, in); !errors.Is(err, ErrIncomplete) || created {
		t.Fatal("partial Logger facts issued", created, err)
	}
	if _, _, err := store.PutUsageFact(ctx, facts[1]); err != nil {
		t.Fatal(err)
	}
	wrong := seal
	wrong.FactSetSHA256 = strings.Repeat("d", 64)
	if _, _, err := store.PutLoggerPeriodSeal(ctx, wrong); !errors.Is(err, ErrIncomplete) {
		t.Fatal("mismatched fact digest accepted", err)
	}
	if _, created, err := store.PutLoggerPeriodSeal(ctx, seal); err != nil || !created {
		t.Fatal("complete source rejected", created, err)
	}
	if _, created, err := store.PutLoggerPeriodSeal(ctx, seal); err != nil || created {
		t.Fatal("exact seal replay", created, err)
	}
	wrong = seal
	wrong.SourceSHA256 = strings.Repeat("e", 64)
	if _, _, err := store.PutLoggerPeriodSeal(ctx, wrong); !errors.Is(err, ErrConflict) {
		t.Fatal("changed immutable seal accepted", err)
	}
	for _, sql := range []string{`UPDATE logger_period_seals SET source_sha256=repeat('f',64)`, `DELETE FROM logger_period_seals`} {
		if _, err := db.Exec(ctx, sql); err == nil {
			t.Fatal("seal mutation accepted", sql)
		}
	}
	late := facts[0]
	late.UsageID = "logger-late"
	if _, _, err := store.PutUsageFact(ctx, late); !errors.Is(err, ErrInvoiceImmutable) {
		t.Fatal("late source fact accepted after seal", err)
	}
	if _, created, err := store.PutUsageFact(ctx, facts[0]); err != nil || created {
		t.Fatal("exact fact replay after seal", created, err)
	}
	invoice, created, err := store.PrepareInvoice(ctx, in)
	if err != nil || !created || invoice.TotalMinor != 34 || len(invoice.Lines) != 3 {
		t.Fatalf("invoice=%+v created=%v err=%v", invoice, created, err)
	}
	if replay, created, err := store.PrepareInvoice(ctx, in); err != nil || created || replay.ID != invoice.ID {
		t.Fatal("issued replay changed", replay, created, err)
	}
}

func TestLoggerSourceSealProvesZeroMonthAndRetainedOnlyProduct(t *testing.T) {
	for _, retainedOnly := range []bool{false, true} {
		name := "zero-source"
		if retainedOnly {
			name = "retained-only"
		}
		t.Run(name, func(t *testing.T) {
			ctx, _, store, in, facts := loggerSealFixture(t, false)
			if retainedOnly {
				facts[0].Quantity = 0
				for _, fact := range facts {
					if _, _, err := store.PutUsageFact(ctx, fact); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				facts = nil
			}
			if _, created, err := store.PrepareInvoice(ctx, in); !errors.Is(err, ErrIncomplete) || created {
				t.Fatal("unsealed month issued", created, err)
			}
			seal := loggerSealForFacts(in, facts)
			if _, _, err := store.PutLoggerPeriodSeal(ctx, seal); err != nil {
				t.Fatal(err)
			}
			invoice, created, err := store.PrepareInvoice(ctx, in)
			want := int64(0)
			if retainedOnly {
				want = 4
			}
			if err != nil || !created || invoice.TotalMinor != want {
				t.Fatal("verified monthly result", invoice, created, err)
			}
		})
	}
}

func TestLoggerSourceSealSerializesLateFactAndInvoiceClose(t *testing.T) {
	ctx, db, store, in, facts := loggerSealFixture(t, false)
	for _, fact := range facts {
		if _, _, err := store.PutUsageFact(ctx, fact); err != nil {
			t.Fatal(err)
		}
	}
	pause := &pausedInvoiceConnection{Connection: db, reading: make(chan struct{}), resume: make(chan struct{})}
	var release sync.Once
	resume := func() { release.Do(func() { close(pause.resume) }) }
	t.Cleanup(resume)
	sealDone := make(chan error, 1)
	go func() {
		_, _, err := (&Store{db: pause}).PutLoggerPeriodSeal(ctx, loggerSealForFacts(in, facts))
		sealDone <- err
	}()
	select {
	case <-pause.reading:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	writer, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writerPID := writer.Conn().PgConn().PID()
	late := facts[0]
	late.UsageID = "racing-late"
	lateDone := make(chan error, 1)
	go func() {
		defer writer.Release()
		_, _, err := (&Store{db: writer.Conn()}).PutUsageFact(ctx, late)
		lateDone <- err
	}()
	awaitBlockedInvoiceConnection(t, ctx, db, writerPID)
	closer, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closerPID := closer.Conn().PgConn().PID()
	closeDone := make(chan error, 1)
	go func() {
		defer closer.Release()
		_, created, err := (&Store{db: closer.Conn()}).PrepareInvoice(ctx, in)
		if err == nil && !created {
			err = errors.New("invoice not created")
		}
		closeDone <- err
	}()
	awaitBlockedInvoiceConnection(t, ctx, db, closerPID)
	resume()
	if err := <-sealDone; err != nil {
		t.Fatal("seal", err)
	}
	if err := <-lateDone; !errors.Is(err, ErrInvoiceImmutable) {
		t.Fatal("late fact crossed seal lock", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal("close", err)
	}
}

func TestLoggerSourceSealRejectsWrongScopeMetricWindowAndUnit(t *testing.T) {
	cases := map[string]func([]billing.UsageFact) []billing.UsageFact{
		"partial window": func(f []billing.UsageFact) []billing.UsageFact {
			f[0].WindowStart = f[0].WindowStart.Add(time.Hour)
			return f
		},
		"wrong unit":    func(f []billing.UsageFact) []billing.UsageFact { f[1].Unit = "bytes"; return f },
		"wrong scale":   func(f []billing.UsageFact) []billing.UsageFact { f[1].QuantityScale = 8; return f },
		"legacy metric": func(f []billing.UsageFact) []billing.UsageFact { f[1].MetricCode = "retention_gib_month"; return f },
		"extra product": func(f []billing.UsageFact) []billing.UsageFact {
			extra := f[0]
			extra.UsageID = "unexpected-product"
			extra.ProductID = "44444444-4444-4444-8444-444444444444"
			return append(f, extra)
		},
		"duplicate metric": func(f []billing.UsageFact) []billing.UsageFact {
			extra := f[1]
			extra.UsageID = "duplicate-retained"
			return append(f, extra)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, _, store, in, facts := loggerSealFixture(t, false)
			seal := loggerSealForFacts(in, facts)
			for _, fact := range mutate(facts) {
				if _, _, err := store.PutUsageFact(ctx, fact); err != nil {
					t.Fatal(err)
				}
			}
			if _, created, err := store.PutLoggerPeriodSeal(ctx, seal); !errors.Is(err, ErrIncomplete) || created {
				t.Fatal("invalid canonical fact set sealed", created, err)
			}
			if _, created, err := store.PrepareInvoice(ctx, in); !errors.Is(err, ErrIncomplete) || created {
				t.Fatal("invalid source closed", created, err)
			}
		})
	}
}
