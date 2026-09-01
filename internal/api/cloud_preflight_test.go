package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func certifyCloudPreflightFixture(t *testing.T, f handoffHTTPFixture) paymentstore.CloudPreflightScope {
	t.Helper()
	scope := paymentstore.CloudPreflightScope{OrganizationID: f.scope.OrganizationID, OwnerUserID: f.source, OwnershipVersion: 1}
	state, err := f.store.CaptureCloudPreflightState(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	state.Financial.UsageSettled = true
	state.Financial.InvoicesReconciled = true
	state.Financial.ProviderWorkReconciled = true
	if err := f.store.RecordCloudPreflightEvidence(context.Background(), paymentstore.RecordCloudPreflightInput{State: state, ReceiptID: testutil.OrganizationID(t.Name() + "-receipt"),
		UsageCheckpointSHA256: strings.Repeat("a", 64), InvoiceCheckpointSHA256: strings.Repeat("b", 64), ProviderCheckpointSHA256: strings.Repeat("c", 64), ExpiresAt: state.ObservedAt.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestBillingCloudDeletionPreflightHTTP(t *testing.T) {
	f := newHandoffHTTPFixture(t, 0)
	scope := certifyCloudPreflightFixture(t, f)
	path := "/v1/internal/billing/clouds/" + scope.OrganizationID + "/deletion-preflight"
	for _, tc := range []struct {
		token, owner, version string
		status                int
	}{{testHandoffToken, f.source, "1", 200}, {"", f.source, "1", 401}, {integrationDebitToken, f.source, "1", 401}, {testHandoffToken, f.target, "1", 409}, {testHandoffToken, f.source, "2", 409}, {testHandoffToken, "", "1", 400}, {testHandoffToken, f.source, "01", 400}} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("X-Billing-Owner-User-ID", tc.owner)
		req.Header.Set("X-Billing-Ownership-Version", tc.version)
		res := httptest.NewRecorder()
		f.env.server.Router().ServeHTTP(res, req)
		if res.Code != tc.status || res.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("preflight %d want %d: %s", res.Code, tc.status, res.Body.String())
		}
		if tc.status == 200 {
			var out paymentstore.CloudDeletionPreflight
			if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil || !out.Eligible || out.CloudPreflightScope != scope {
				t.Fatalf("response %+v %v", out, err)
			}
		}
	}
	var handoffs, receipts int
	if err := f.env.db.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM billing_ownership_handoffs),(SELECT count(*) FROM billing_cloud_preflight_receipts)`).Scan(&handoffs, &receipts); err != nil || handoffs != 0 || receipts != 1 {
		t.Fatalf("preflight created hold/evidence: %d %d %v", handoffs, receipts, err)
	}
	// Dedicated coordinator cannot manufacture collector evidence or close an account.
	for _, suffix := range []string{"/preflight-evidence", "/close", "/deletion-preflight"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/internal/billing/clouds/"+scope.OrganizationID+suffix, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testHandoffToken)
		res := httptest.NewRecorder()
		f.env.server.Router().ServeHTTP(res, req)
		if res.Code != 404 {
			t.Fatalf("unexpected mutation route %s: %d", suffix, res.Code)
		}
	}
}

func TestBillingCloudDeletionPreflightAccountManagerClient(t *testing.T) {
	dir := os.Getenv("ACCOUNT_MANAGER_HANDOFF_CLIENT_DIR")
	if dir == "" {
		t.Skip("requires isolated Account Manager checkout")
	}
	f := newHandoffHTTPFixture(t, 0)
	certifyCloudPreflightFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "./internal/billinghandoff", "-run", "^TestLiveCloudDeletionPreflightTransport$", "-count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TEST_BILLING_HANDOFF_URL="+f.server.URL, "TEST_BILLING_HANDOFF_TOKEN="+testHandoffToken, "TEST_HANDOFF_CLOUD="+f.scope.OrganizationID, "TEST_HANDOFF_SOURCE="+f.source)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("AM preflight transport: %v\n%s", err, output)
	}
}
