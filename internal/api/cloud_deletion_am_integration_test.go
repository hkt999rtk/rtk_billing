package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

// Actual AM global API, persistence/job recovery and Billing HTTP/persistence.
// Resource inventory/holds and collector checkpoints are explicitly synthetic.
// Test-only hooks below are never registered by the production server.
func TestCloudDeletionAccountManagerPublicAPIContract(t *testing.T) {
	for _, mode := range []string{"lost_close", "retirement", "cancel", "close_wins"} {
		t.Run(mode, func(t *testing.T) { exerciseCloudDeletionRecovery(t, mode) })
	}
}
func exerciseCloudDeletionRecovery(t *testing.T, mode string) {
	dir, amDB := os.Getenv("ACCOUNT_MANAGER_HANDOFF_CLIENT_DIR"), os.Getenv("ACCOUNT_MANAGER_HANDOFF_DATABASE_URL")
	if dedicated := os.Getenv("ACCOUNT_MANAGER_DELETION_DATABASE_URL"); dedicated != "" {
		amDB = dedicated
	}
	if dir == "" || amDB == "" {
		t.Skip("requires isolated AM checkout and database")
	}
	u, err := url.Parse(amDB)
	if err != nil || u.Scheme != "postgres" || u.Hostname() != "127.0.0.1" || strings.TrimPrefix(u.Path, "/") == "" || samePostgresEndpoint(amDB, os.Getenv("TEST_DATABASE_URL")) {
		t.Fatal("requires a separate named AM PostgreSQL database on literal loopback")
	}
	env := newIntegrationAPI(t)
	s := paymentstore.New(env.db)
	if err := env.server.ConfigureHandoff(HandoffAPIOptions{Token: testHandoffToken, Store: s}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var scope paymentstore.CloudPreflightScope
	lost := false
	lostRetirement := false
	settlements, cancelCalls := 0, 0
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fail := func(err error) bool {
			if err == nil {
				return false
			}
			t.Errorf("closure fixture: %v", err)
			w.WriteHeader(500)
			return true
		}
		if r.URL.Path == "/test-fixture/bind-deletion" {
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+testHandoffToken || scope.OrganizationID != "" {
				w.WriteHeader(403)
				return
			}
			var in struct {
				CloudID string `json:"cloud_id"`
				OwnerID string `json:"owner_user_id"`
			}
			if fail(json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in)) {
				return
			}
			scope = paymentstore.CloudPreflightScope{OrganizationID: in.CloudID, OwnerUserID: in.OwnerID, OwnershipVersion: 1}
			account, _, err := s.EnsureCommercialAccount(r.Context(), in.CloudID, payment.CurrencyTWD)
			if fail(err) {
				return
			}
			if fail(s.InitializeResponsibility(r.Context(), paymentstore.InitialResponsibilityInput{AccountID: account.ID, OwnerUserID: in.OwnerID, OwnershipVersion: 1, EffectiveFrom: time.Now().Add(-time.Hour), SourceEvidenceSHA256: strings.Repeat("a", 64)})) {
				return
			}
			state, err := s.CaptureCloudPreflightState(r.Context(), scope)
			if fail(err) {
				return
			}
			state.Financial.UsageSettled = true
			state.Financial.InvoicesReconciled = true
			state.Financial.ProviderWorkReconciled = true
			if fail(s.RecordCloudPreflightEvidence(r.Context(), paymentstore.RecordCloudPreflightInput{State: state, ReceiptID: testutil.OrganizationID("live-delete-preflight"), ExpiresAt: state.ObservedAt.Add(time.Minute), UsageCheckpointSHA256: strings.Repeat("a", 64), InvoiceCheckpointSHA256: strings.Repeat("b", 64), ProviderCheckpointSHA256: strings.Repeat("c", 64)})) {
				return
			}
			w.WriteHeader(204)
			return
		}
		if strings.Contains(r.URL.Path, "/closures/") && strings.HasSuffix(r.URL.Path, "/status") && r.Header.Get("Authorization") == "Bearer "+testHandoffToken {
			parts := strings.Split(r.URL.Path, "/")
			operation := parts[len(parts)-2]
			binding := paymentstore.CloudClosureScope{CloudPreflightScope: scope, OperationID: operation}
			state, err := s.CaptureCloudClosureState(r.Context(), binding)
			if fail(err) {
				return
			}
			state.Financial.UsageSettled = true
			state.Financial.InvoicesReconciled = true
			state.Financial.ProviderWorkReconciled = true
			settlements++
			if fail(s.RecordCloudClosureSettlement(r.Context(), paymentstore.RecordCloudClosureSettlementInput{State: state, ReceiptID: testutil.OrganizationID(fmt.Sprint("live-delete-settlement-", settlements)), CoveredThrough: state.Cutoff, ExpiresAt: state.ObservedAt.Add(time.Minute), UsageCheckpointSHA256: strings.Repeat("a", 64), InvoiceCheckpointSHA256: strings.Repeat("b", 64), ProviderCheckpointSHA256: strings.Repeat("c", 64)})) {
				return
			}
		}
		if strings.Contains(r.URL.Path, "/closures/") && strings.HasSuffix(r.URL.Path, "/close") && !lost {
			if mode == "retirement" {
				_, err := env.db.Exec(r.Context(), `UPDATE commercial_accounts SET version=version+1 WHERE organization_id=$1`, scope.OrganizationID)
				if fail(err) {
					return
				}
				lost = true
				env.server.Router().ServeHTTP(w, r)
				return
			}
			recorder := httptest.NewRecorder()
			env.server.Router().ServeHTTP(recorder, r)
			if recorder.Code != 200 {
				t.Errorf("real close failed %d %s", recorder.Code, recorder.Body.String())
				w.WriteHeader(500)
				return
			}
			lost = true
			w.WriteHeader(503)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/retire-command") && mode == "retirement" && !lostRetirement {
			recorder := httptest.NewRecorder()
			env.server.Router().ServeHTTP(recorder, r)
			if recorder.Code != 200 {
				t.Errorf("retirement failed %d %s", recorder.Code, recorder.Body.String())
				w.WriteHeader(500)
				return
			}
			lostRetirement = true
			w.WriteHeader(503)
			return
		}
		if strings.Contains(r.URL.Path, "/closures/") && strings.HasSuffix(r.URL.Path, "/cancel") && mode == "cancel" {
			cancelCalls++
			if cancelCalls == 2 {
				parts := strings.Split(r.URL.Path, "/")
				operation := parts[len(parts)-2]
				binding := paymentstore.CloudClosureScope{CloudPreflightScope: scope, OperationID: operation}
				var id string
				if fail(env.db.QueryRow(r.Context(), `SELECT cancellation_id::text FROM billing_cloud_closure_cancellations WHERE operation_id=$1`, operation).Scan(&id)) {
					return
				}
				// Explicit synthetic provider release, never a coordinator endpoint.
				_, err := s.CompleteCloudClosureCancellation(r.Context(), binding, id, strings.Repeat("e", 64))
				if fail(err) {
					return
				}
			}
		}
		env.server.Router().ServeHTTP(w, r)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "./internal/api", "-run", "^TestLiveCloudDeletionPublicAPIWithBilling$", "-count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TEST_DATABASE_URL="+amDB, "TEST_BILLING_HANDOFF_URL="+server.URL, "TEST_BILLING_HANDOFF_TOKEN="+testHandoffToken, "TEST_CLOUD_DELETION_RECOVERY_MODE="+mode)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("AM deletion: %v\n%s", err, output)
	}
	var state, phase string
	wantState, wantPhase := "closed", "closed"
	if mode == "cancel" {
		wantState, wantPhase = "active", "canceled"
	}
	if err := env.db.QueryRow(context.Background(), `SELECT a.state,c.phase FROM commercial_accounts a JOIN billing_cloud_closures c ON c.account_id=a.id WHERE a.organization_id=$1`, scope.OrganizationID).Scan(&state, &phase); err != nil || state != wantState || phase != wantPhase || (mode != "cancel" && !lost) || (mode == "retirement" && !lostRetirement) {
		t.Fatalf("closure not proven: %s %s %v lost=%v", state, phase, err, lost)
	}
}
