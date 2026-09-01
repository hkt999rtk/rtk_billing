package api

import (
	"context"
	"encoding/json"
	"io"
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

// Real global-session AM APIs call the real Billing router and both databases.
// Initial eligibility, producer preparation and collector checkpoints remain
// synthetic fixtures. The commit variant consumes a real Billing grant and
// persists an actual AM owner decision before real Billing finalization.
func TestHandoffAccountManagerPublicAPIContract(t *testing.T) {
	for _, mode := range []string{"consent", "commit", "worker"} {
		t.Run(mode, func(t *testing.T) { exerciseAccountManagerPublicAPIContract(t, mode) })
	}
}

func exerciseAccountManagerPublicAPIContract(t *testing.T, mode string) {
	dir, amDB := os.Getenv("ACCOUNT_MANAGER_HANDOFF_CLIENT_DIR"), os.Getenv("ACCOUNT_MANAGER_HANDOFF_DATABASE_URL")
	if dir == "" || amDB == "" {
		t.Skip("requires isolated Account Manager checkout and separate fixture database")
	}
	parsed, err := url.Parse(amDB)
	if err != nil || parsed.Scheme != "postgres" || parsed.Host != "127.0.0.1:63229" || parsed.Path != "/multicloud_am_public_http_test" {
		t.Fatal("AM fixture requires the dedicated local disposable database")
	}
	if amDB == os.Getenv("TEST_DATABASE_URL") {
		t.Fatal("AM and Billing must use separate databases")
	}
	env := newIntegrationAPI(t)
	store := paymentstore.New(env.db)
	if err := env.server.ConfigureHandoff(HandoffAPIOptions{Token: testHandoffToken, Store: store}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var operationID string
	router := http.NewServeMux()
	// This bootstrap hook exists ONLY in the test wrapper, never production.
	router.HandleFunc("/test-fixture/bind", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+testHandoffToken {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var binding struct {
			CloudID, OperationID, SourceUserID, TargetUserID string
			OwnershipVersion                                 int64
			Cutoff                                           time.Time
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&binding); err != nil {
			t.Errorf("fixture binding: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if operationID != "" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		fail := func(err error) bool {
			if err == nil {
				return false
			}
			t.Errorf("Billing fixture bootstrap: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		ctx := r.Context()
		account, _, err := store.EnsureCommercialAccount(ctx, binding.CloudID, payment.CurrencyTWD)
		if fail(err) {
			return
		}
		if fail(store.InitializeResponsibility(ctx, paymentstore.InitialResponsibilityInput{AccountID: account.ID, OwnerUserID: binding.SourceUserID, OwnershipVersion: binding.OwnershipVersion,
			EffectiveFrom: binding.Cutoff.Add(-time.Hour), SourceEvidenceSHA256: strings.Repeat("a", 64)})) {
			return
		}
		_, err = store.PrepareOwnershipHandoff(ctx, paymentstore.PrepareOwnershipHandoffInput{OrganizationID: binding.CloudID, OperationID: binding.OperationID,
			SourceUserID: binding.SourceUserID, TargetUserID: binding.TargetUserID, OwnershipVersion: binding.OwnershipVersion, Cutoff: binding.Cutoff})
		if fail(err) {
			return
		}
		scope := paymentstore.HandoffScope{OrganizationID: binding.CloudID, OperationID: binding.OperationID, OwnershipVersion: binding.OwnershipVersion}
		state, err := store.CaptureHandoffSettlementState(ctx, scope)
		if fail(err) {
			return
		}
		state.Financial.UsageSettled, state.Financial.InvoicesReconciled, state.Financial.ProviderWorkReconciled = true, true, true
		_, err = store.RecordHandoffSettlement(ctx, paymentstore.RecordSettlementInput{Scope: scope, ReceiptID: testutil.OrganizationID("public-api-fixture"), StateSHA256: state.SHA256, Financial: state.Financial,
			UsageCheckpointSHA256: strings.Repeat("a", 64), InvoiceCheckpointSHA256: strings.Repeat("b", 64), ProviderCheckpointSHA256: strings.Repeat("c", 64)})
		if fail(err) {
			return
		}
		operationID = binding.OperationID
		w.WriteHeader(http.StatusNoContent)
	})
	router.Handle("/", env.server.Router())
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "./internal/api", "-run", "^TestLiveOwnerHandoffPublicAPIWithBilling$", "-count=1")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TEST_DATABASE_URL="+amDB, "TEST_BILLING_HANDOFF_URL="+server.URL, "TEST_BILLING_HANDOFF_TOKEN="+testHandoffToken, "TEST_HANDOFF_MODE="+mode)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("AM public API contract: %v\n%s", err, output)
	}
	mu.Lock()
	defer mu.Unlock()
	var phase string
	var confirmations int
	wantPhase := "prepared"
	if mode != "consent" {
		wantPhase = "finalized"
	}
	if err := env.db.QueryRow(context.Background(), `SELECT phase,(SELECT count(*) FROM billing_handoff_confirmations WHERE operation_id=h.id)
		FROM billing_ownership_handoffs h WHERE id=$1`, operationID).Scan(&phase, &confirmations); err != nil || phase != wantPhase || confirmations != 2 {
		t.Fatalf("consent lost or advanced ownership: phase=%s confirmations=%d err=%v", phase, confirmations, err)
	}
	if mode != "consent" {
		var periods, currentTarget int
		if err := env.db.QueryRow(context.Background(), `SELECT count(*),count(*) FILTER(WHERE r.effective_until IS NULL AND r.owner_user_id=h.target_user_id AND r.ownership_version=h.ownership_version+1)
			FROM billing_responsibility_periods r JOIN billing_ownership_handoffs h ON h.account_id=r.account_id WHERE h.id=$1`, operationID).Scan(&periods, &currentTarget); err != nil || periods != 2 || currentTarget != 1 {
			t.Fatalf("real AM commit missing Billing responsibility handoff: %d %d %v", periods, currentTarget, err)
		}
	}
}
