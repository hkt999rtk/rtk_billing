package paymentstore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func creationFixture(seed string) CloudCreation {
	e := CloudCreation{EventID: testutil.OrganizationID(seed + "event"), OrganizationID: testutil.OrganizationID(seed + "cloud"), OwnerUserID: testutil.OrganizationID(seed + "owner"), OwnershipVersion: 1, OccurredAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)}
	e.EvidenceSHA256 = e.EvidenceDigest()
	return e
}

func TestCloudCreationBootstrapIsAtomicIdempotentAndNeverAdoptsLegacyAccounts(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	e := creationFixture(t.Name())
	var wg sync.WaitGroup
	out := make(chan CloudCreationReceipt, 10)
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := env.store.BootstrapBrandCloud(ctx, e)
			if err != nil {
				errs <- err
			} else {
				out <- r
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(out)
	for err := range errs {
		t.Fatal(err)
	}
	var accountID string
	for r := range out {
		if accountID != "" && r.AccountID != accountID {
			t.Fatal("duplicate account")
		}
		accountID = r.AccountID
		if !r.Valid() || r.EvidenceSHA256 != e.EvidenceSHA256 {
			t.Fatal("changed receipt")
		}
	}
	var owner string
	var from time.Time
	var count, balance int64
	if err := env.db.QueryRow(ctx, `SELECT p.owner_user_id::text,p.effective_from,a.available_balance_minor,(SELECT count(*) FROM billing_audit_events WHERE request_id=$2) FROM billing_responsibility_periods p JOIN commercial_accounts a ON a.id=p.account_id WHERE a.id=$1`, accountID, e.EventID).Scan(&owner, &from, &balance, &count); err != nil || owner != e.OwnerUserID || !from.Equal(e.OccurredAt) || balance != 0 || count != 1 {
		t.Fatal("initial responsibility", err)
	}
	for _, mutate := range []func(*CloudCreation){func(e *CloudCreation) { e.OwnerUserID = testutil.OrganizationID("different") }, func(e *CloudCreation) { e.OrganizationID = testutil.OrganizationID("different") }, func(e *CloudCreation) { e.EventID = testutil.OrganizationID("different") }, func(e *CloudCreation) { e.OccurredAt = e.OccurredAt.Add(time.Second) }} {
		bad := e
		mutate(&bad)
		bad.EvidenceSHA256 = bad.EvidenceDigest()
		if _, err := env.store.BootstrapBrandCloud(ctx, bad); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatal("changed creation adopted", err)
		}
	}
	legacy := creationFixture("legacy")
	if _, _, err := env.store.EnsureCommercialAccount(ctx, legacy.OrganizationID, payment.CurrencyTWD); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.BootstrapBrandCloud(ctx, legacy); !errors.Is(err, ErrOwnershipEvidenceMissing) {
		t.Fatal("legacy account reattributed", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE commercial_accounts SET state='closed' WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	if r, err := New(env.db).BootstrapBrandCloud(ctx, e); err != nil || r.AccountID != accountID {
		t.Fatal("historical replay", err)
	}
	var state string
	if err := env.db.QueryRow(ctx, `SELECT state FROM commercial_accounts WHERE id=$1`, accountID).Scan(&state); err != nil || state != "closed" {
		t.Fatal("replay reopened account", err)
	}
	for _, q := range []string{`DELETE FROM billing_cloud_creation_receipts WHERE event_id=$1`, `UPDATE billing_cloud_creation_receipts SET owner_user_id=organization_id WHERE event_id=$1`} {
		if _, err := env.db.Exec(ctx, q, e.EventID); err == nil {
			t.Fatal("receipt history mutated")
		}
	}
}

func TestCloudCreationBootstrapRejectsUnprovenEventsAndRollsBack(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	e := creationFixture(t.Name())
	for _, mutate := range []func(*CloudCreation){func(e *CloudCreation) { e.OwnerUserID = "bad" }, func(e *CloudCreation) { e.OwnershipVersion = 2 }, func(e *CloudCreation) { e.OccurredAt = e.OccurredAt.Add(time.Nanosecond) }, func(e *CloudCreation) { e.EvidenceSHA256 = "bad" }, func(e *CloudCreation) {
		e.OccurredAt = time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		e.EvidenceSHA256 = e.EvidenceDigest()
	}} {
		bad := e
		mutate(&bad)
		if _, err := env.store.BootstrapBrandCloud(ctx, bad); !errors.Is(err, ErrConflict) {
			t.Fatal("invalid creation accepted", err)
		}
	}
	// Audit failure must roll back the new account, responsibility and receipt.
	if _, err := env.db.Exec(ctx, `CREATE FUNCTION reject_creation_audit_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='billing.cloud_creation.bootstrap' THEN RAISE EXCEPTION 'isolated audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_creation_audit_test BEFORE INSERT ON billing_audit_events FOR EACH ROW EXECUTE FUNCTION reject_creation_audit_test()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		env.db.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_creation_audit_test ON billing_audit_events; DROP FUNCTION IF EXISTS reject_creation_audit_test()`)
	})
	if _, err := env.store.BootstrapBrandCloud(ctx, e); err == nil {
		t.Fatal("audit failure committed")
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM commercial_accounts WHERE organization_id=$1`, e.OrganizationID).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial bootstrap survived", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := env.store.BootstrapBrandCloud(canceled, e); err == nil {
		t.Fatal("unavailable storage accepted")
	}
}
