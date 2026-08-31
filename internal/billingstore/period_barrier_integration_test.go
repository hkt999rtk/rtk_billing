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
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pause only the timing of a real PostgreSQL query. No persistence or financial
// outcome is mocked; the competing connections use the actual account locks.
type pausedInvoiceConnection struct {
	database.Connection
	reading chan struct{}
	resume  chan struct{}
	once    sync.Once
}
type pausedInvoiceTx struct {
	pgx.Tx
	owner *pausedInvoiceConnection
}

func (c *pausedInvoiceConnection) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := c.Connection.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &pausedInvoiceTx{Tx: tx, owner: c}, nil
}
func (tx *pausedInvoiceTx) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if strings.Contains(query, "FROM billing_usage_facts") {
		tx.owner.once.Do(func() { close(tx.owner.reading) })
		select {
		case <-tx.owner.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return tx.Tx.Query(ctx, query, args...)
}

func periodBarrierFixture(t *testing.T) (context.Context, *pgxpool.Pool, *Store, PrepareInvoiceInput, billing.UsageFact) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	db, err := database.Connect(ctx, dsn)
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
		pricing_plan_versions, billing_profiles, balance_ledger_entries, commercial_accounts RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	org := testutil.OrganizationID(t.Name())
	account, _, err := paymentstore.New(db).EnsureCommercialAccount(ctx, org, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	s := New(db)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	p, err := s.CreatePricingVersion(ctx, CreatePricingVersionInput{PlanKey: "barrier", Version: 1, Currency: billing.CurrencyTWD, EffectiveFrom: start, CreatedBy: "test", Now: start,
		Rates: []billing.PricingRate{{ServiceCode: "mqtt", MetricCode: "publish_bytes", Description: "MQTT", Unit: "bytes", UnitPriceMinor: 2, RoundingMode: billing.RoundingHalfUp}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivatePricingVersion(ctx, p.ID, start); err != nil {
		t.Fatal(err)
	}
	in := PrepareInvoiceInput{OrganizationID: org, AccountID: account.ID, Currency: billing.CurrencyTWD, PeriodStart: start, PeriodEnd: start.Add(24 * time.Hour), Now: start.Add(25 * time.Hour)}
	fact := billing.UsageFact{UsageID: "barrier-initial", OrganizationID: org, ServiceCode: "mqtt", MetricCode: "publish_bytes", Quantity: 10, Unit: "bytes", WindowStart: start, WindowEnd: start.Add(time.Hour), Source: "test", SourceSHA256: strings.Repeat("a", 64)}
	return ctx, db, s, in, fact
}

func awaitBlockedInvoiceConnection(t *testing.T, ctx context.Context, db *pgxpool.Pool, pid uint32) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := db.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("connection never reached database lock", ctx.Err())
		}
	}
}

func TestInvoiceCloseFencesConcurrentUsageAndRechecksIncompleteRetry(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "first-close"
		if retry {
			name = "incomplete-retry"
		}
		t.Run(name, func(t *testing.T) {
			ctx, db, s, in, fact := periodBarrierFixture(t)
			if retry {
				if _, _, err := s.PrepareInvoice(ctx, in); !errors.Is(err, ErrIncomplete) {
					t.Fatal("missing usage", err)
				}
				var state string
				if err := db.QueryRow(ctx, `SELECT state FROM billing_periods WHERE organization_id=$1`, in.OrganizationID).Scan(&state); err != nil || state != "incomplete" {
					t.Fatal(state, err)
				}
			}
			if _, _, err := s.PutUsageFact(ctx, fact); err != nil {
				t.Fatal(err)
			}
			pause := &pausedInvoiceConnection{Connection: db, reading: make(chan struct{}), resume: make(chan struct{})}
			var release sync.Once
			resume := func() { release.Do(func() { close(pause.resume) }) }
			t.Cleanup(resume)
			type result struct {
				invoice billing.Invoice
				created bool
				err     error
			}
			closed := make(chan result, 1)
			go func() {
				invoice, created, err := (&Store{db: pause}).PrepareInvoice(ctx, in)
				closed <- result{invoice, created, err}
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
			pid := writer.Conn().PgConn().PID()
			late := fact
			late.UsageID = "late-concurrent"
			written := make(chan error, 1)
			go func() {
				defer writer.Release()
				_, _, err := (&Store{db: writer.Conn()}).PutUsageFact(ctx, late)
				written <- err
			}()
			awaitBlockedInvoiceConnection(t, ctx, db, pid)
			resume()
			got := <-closed
			if got.err != nil || !got.created || got.invoice.TotalMinor != 20 {
				t.Fatal("close", got)
			}
			if err := <-written; !errors.Is(err, ErrInvoiceImmutable) {
				t.Fatal("late usage acknowledged", err)
			}
			if _, err := s.GetUsageFact(ctx, late.UsageID); !errors.Is(err, ErrNotFound) {
				t.Fatal("late fact persisted", err)
			}
			if _, created, err := s.PutUsageFact(ctx, fact); err != nil || created {
				t.Fatal("exact replay", created, err)
			}
			if again, created, err := s.PrepareInvoice(ctx, in); err != nil || created || again.ID != got.invoice.ID {
				t.Fatal("invoice retry", again, created, err)
			}
		})
	}
}

func TestInvoiceCloseWaitsForCommittedUsageAndSerializesCompetingCloses(t *testing.T) {
	_, db, s, in, fact := periodBarrierFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	writer, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if _, _, err := (&Store{db: database.TransactionConnection{Tx: writer}}).PutUsageFact(ctx, fact); err != nil {
		t.Fatal(err)
	}
	type result struct {
		invoice billing.Invoice
		created bool
		err     error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		conn, err := db.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		pid := conn.Conn().PgConn().PID()
		go func() {
			defer conn.Release()
			invoice, created, err := (&Store{db: conn.Conn()}).PrepareInvoice(ctx, in)
			results <- result{invoice, created, err}
		}()
		awaitBlockedInvoiceConnection(t, ctx, db, pid)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.invoice.ID != b.invoice.ID || a.created == b.created || a.invoice.TotalMinor != 20 {
		t.Fatal(a, b)
	}
	if _, created, err := s.PutUsageFact(ctx, fact); err != nil || created {
		t.Fatal(created, err)
	}
}

func TestInvoiceCloseFailureRollsBackPeriodAndRejectsWrongAccount(t *testing.T) {
	ctx, db, s, in, fact := periodBarrierFixture(t)
	if _, _, err := s.PutUsageFact(ctx, fact); err != nil {
		t.Fatal(err)
	}
	wrong := in
	wrong.OrganizationID = testutil.OrganizationID("wrong-cloud")
	if _, _, err := s.PrepareInvoice(ctx, wrong); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-cloud account", err)
	}
	if _, err := db.Exec(ctx, `CREATE FUNCTION test_fail_invoice_line() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected invoice line failure'; END $$;
		CREATE TRIGGER test_fail_invoice_line BEFORE INSERT ON billing_invoice_lines FOR EACH ROW EXECUTE FUNCTION test_fail_invoice_line()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_fail_invoice_line ON billing_invoice_lines; DROP FUNCTION IF EXISTS test_fail_invoice_line()`)
	})
	if _, _, err := s.PrepareInvoice(ctx, in); err == nil {
		t.Fatal("failure accepted")
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM billing_periods)+(SELECT count(*) FROM billing_invoices)+(SELECT count(*) FROM billing_invoice_lines)`).Scan(&n); err != nil || n != 0 {
		t.Fatal("partial invoice committed", n, err)
	}
	if _, err := db.Exec(ctx, `DROP TRIGGER test_fail_invoice_line ON billing_invoice_lines; DROP FUNCTION test_fail_invoice_line()`); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.PrepareInvoice(ctx, in); err != nil || !created {
		t.Fatal("retry", created, err)
	}
}
