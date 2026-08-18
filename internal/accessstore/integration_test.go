package accessstore

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestBillingAccessStateIsVersionedAndFailClosed(t *testing.T) {
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
	if _, err := db.Exec(ctx, `TRUNCATE billing_access_states`); err != nil {
		t.Fatal(err)
	}
	store := New(db)
	organizationID := testutil.OrganizationID("access-state")
	initial, err := store.GetOrCreate(ctx, organizationID)
	if err != nil || initial.State != "active" || initial.Version != 1 {
		t.Fatalf("initial=%+v err=%v", initial, err)
	}
	updated, err := store.Put(ctx, organizationID, "read_only", "payment_review", "finance-operator", 1)
	if err != nil || updated.Version != 2 {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	if _, err := store.Put(ctx, organizationID, "active", "", "finance-operator", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version err=%v", err)
	}
}
