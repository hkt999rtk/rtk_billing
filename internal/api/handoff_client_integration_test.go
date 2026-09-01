package api

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This is a real TCP/JSON exchange with the separately compiled AM client and
// Billing's router/store. AM commit/collector evidence is still synthetic: this
// does not prove the complete coordinator, global session or producer workflow.
func TestHandoffAccountManagerClientContract(t *testing.T) {
	dir := os.Getenv("ACCOUNT_MANAGER_HANDOFF_CLIENT_DIR")
	if dir == "" {
		t.Skip("requires isolated Account Manager client checkout")
	}
	f := newHandoffHTTPFixture(t, 0)
	f.prepare(t)
	f.settle(t, "cross-repository")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "./internal/billinghandoff", "-run", "^TestLiveBillingTransportContract$", "-count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TEST_BILLING_HANDOFF_URL="+f.server.URL, "TEST_BILLING_HANDOFF_TOKEN="+testHandoffToken,
		"TEST_HANDOFF_CLOUD="+f.scope.OrganizationID, "TEST_HANDOFF_OPERATION="+f.scope.OperationID, "TEST_HANDOFF_SOURCE="+f.source,
		"TEST_HANDOFF_TARGET="+f.target, "TEST_HANDOFF_CUTOFF="+f.cutoff.Format(time.RFC3339Nano))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Account Manager client contract: %v\n%s", err, output)
	}
	var phase string
	if err := f.env.db.QueryRow(context.Background(), `SELECT phase FROM billing_ownership_handoffs WHERE id=$1`, f.scope.OperationID).Scan(&phase); err != nil || phase != "finalized" {
		t.Fatalf("client did not persist finalization: %s %v", phase, err)
	}
}
