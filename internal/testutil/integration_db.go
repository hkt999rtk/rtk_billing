package testutil

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OrganizationID returns a stable opaque UUID without requiring Account
// Manager tables in the Billing integration database.
func OrganizationID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

const integrationDBLockKey int64 = 104729

// LockIntegrationDatabase serializes tests that share TEST_DATABASE_URL so
// package-level go test parallelism does not make them clobber each other's
// fixtures and truncation.
func LockIntegrationDatabase(t *testing.T, db *pgxpool.Pool) {
	t.Helper()

	ctx := context.Background()
	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, integrationDBLockKey); err != nil {
		conn.Release()
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, integrationDBLockKey); err != nil {
			t.Errorf("unlock integration database: %v", err)
		}
		conn.Release()
	})
}
