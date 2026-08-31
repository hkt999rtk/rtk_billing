package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func closureHTTPRequest(t *testing.T, f handoffHTTPFixture, method, suffix, token, owner, version, body string, want int) []byte {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/internal/billing/clouds/"+f.scope.OrganizationID+"/closures/"+f.scope.OperationID+suffix, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Billing-Owner-User-ID", owner)
	req.Header.Set("X-Billing-Ownership-Version", version)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	f.env.server.Router().ServeHTTP(res, req)
	if res.Code != want || want != 404 && res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%s %s: %d want %d: %s", method, suffix, res.Code, want, res.Body.String())
	}
	if want == 200 {
		var out struct {
			paymentstore.CloudPreflightScope
			OperationID string `json:"operation_id"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil || out.OrganizationID != f.scope.OrganizationID || out.OwnerUserID != owner || out.OwnershipVersion != f.scope.OwnershipVersion || out.OperationID != f.scope.OperationID {
			t.Fatalf("lost closure scope: %s %v", res.Body.String(), err)
		}
	}
	return res.Body.Bytes()
}

func TestCloudClosureHTTPBoundary(t *testing.T) {
	f := newHandoffHTTPFixture(t, 0)
	for _, suffix := range []string{"/prepare", "/status", "/close", "/cancel"} {
		method := http.MethodPost
		if suffix == "/status" {
			method = http.MethodGet
		}
		for _, token := range []string{"", integrationServiceToken, integrationInternalToken, integrationDebitToken, "user-jwt"} {
			closureHTTPRequest(t, f, method, suffix, token, f.source, "1", `{}`, 401)
		}
	}
	for _, version := range []string{"", "0", "-1", "01", "+1", "9223372036854775808"} {
		closureHTTPRequest(t, f, "GET", "/status", testHandoffToken, f.source, version, "", 400)
	}
	for _, owner := range []string{"", "not-a-uuid", strings.ToUpper(f.source)} {
		closureHTTPRequest(t, f, "GET", "/status", testHandoffToken, owner, "1", "", 400)
	}
	for _, suffix := range []string{"/prepare", "/close", "/cancel"} {
		for _, body := range []string{`null`, `{}`, `{} {}`, `{"financial_evidence":{"UsageSettled":true}}`, strings.Repeat(" ", 16*1024) + `{}`} {
			closureHTTPRequest(t, f, "POST", suffix, testHandoffToken, f.source, "1", body, 400)
		}
	}
	// Coordinator commands never confer collector, provider or release authority.
	for _, suffix := range []string{"/settlement", "/revocation", "/release", "/complete-cancel", "/collect"} {
		closureHTTPRequest(t, f, "POST", suffix, testHandoffToken, f.source, "1", `{}`, 404)
	}
	var count int
	if err := f.env.db.QueryRow(context.Background(), `SELECT count(*) FROM billing_cloud_closures`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid command mutated closure: %d %v", count, err)
	}
}

func TestCloudClosureRoutesAbsentWithoutConfiguration(t *testing.T) {
	s := newHandoffTestServer(t)
	for _, suffix := range []string{"prepare", "status", "close", "cancel"} {
		method := "POST"
		if suffix == "status" {
			method = "GET"
		}
		req := httptest.NewRequest(method, "/v1/internal/billing/clouds/11111111-1111-1111-1111-111111111111/closures/22222222-2222-2222-2222-222222222222/"+suffix, nil)
		req.Header.Set("Authorization", "Bearer "+testHandoffToken)
		res := httptest.NewRecorder()
		s.Router().ServeHTTP(res, req)
		if res.Code != 404 {
			t.Fatalf("unconfigured closure route exposed: %s %d", suffix, res.Code)
		}
	}
}

func TestCloudClosureHTTPSettlementAndReplay(t *testing.T) {
	for _, balance := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprint(balance), func(t *testing.T) {
			f := newHandoffHTTPFixture(t, balance)
			request := func(method, suffix, body string, status int) []byte {
				return closureHTTPRequest(t, f, method, suffix, testHandoffToken, f.source, "1", body, status)
			}
			prepare, _ := json.Marshal(map[string]any{"cutoff": f.cutoff, "am_request_sha256": strings.Repeat("a", 64)})
			for i := 0; i < 2; i++ {
				request("POST", "/prepare", string(prepare), 200)
			}
			closureHTTPRequest(t, f, "GET", "/status", testHandoffToken, f.target, "1", "", 409)
			closureHTTPRequest(t, f, "GET", "/status", testHandoffToken, f.source, "2", "", 409)
			other := f
			other.scope.OrganizationID = testutil.OrganizationID("different-cloud")
			closureHTTPRequest(t, other, "GET", "/status", testHandoffToken, f.source, "1", "", 404)
			status := request("GET", "/status", "", 200)
			if !strings.Contains(string(status), "evidence_unavailable") || strings.Contains(string(status), `"ready":true`) {
				t.Fatalf("fabricated readiness: %s", status)
			}
			id := testutil.OrganizationID("closure-http-receipt")
			closeBody, _ := json.Marshal(map[string]any{"settlement_id": id, "am_readiness_sha256": strings.Repeat("b", 64)})
			request("POST", "/close", string(closeBody), 409)
			scope := paymentstore.CloudClosureScope{CloudPreflightScope: paymentstore.CloudPreflightScope{OrganizationID: f.scope.OrganizationID, OwnerUserID: f.source, OwnershipVersion: 1}, OperationID: f.scope.OperationID}
			state, err := f.store.CaptureCloudClosureState(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			// Synthetic collector checkpoints, not production reconciliation evidence.
			state.Financial.UsageSettled, state.Financial.InvoicesReconciled, state.Financial.ProviderWorkReconciled = true, true, true
			err = f.store.RecordCloudClosureSettlement(context.Background(), paymentstore.RecordCloudClosureSettlementInput{State: state, ReceiptID: id, CoveredThrough: state.Cutoff, ExpiresAt: state.ObservedAt.Add(time.Minute), UsageCheckpointSHA256: strings.Repeat("a", 64), InvoiceCheckpointSHA256: strings.Repeat("b", 64), ProviderCheckpointSHA256: strings.Repeat("c", 64)})
			if err != nil {
				t.Fatal(err)
			}
			if balance != 0 {
				request("POST", "/close", string(closeBody), 409)
				cancel, _ := json.Marshal(map[string]any{"cancellation_id": testutil.OrganizationID("cancel-http"), "am_cancellation_sha256": strings.Repeat("c", 64)})
				for i := 0; i < 2; i++ {
					raw := request("POST", "/cancel", string(cancel), 200)
					if !strings.Contains(string(raw), `"phase":"canceling"`) {
						t.Fatalf("cancel prematurely released hold: %s", raw)
					}
				}
				return
			}
			first := request("POST", "/close", string(closeBody), 200)
			second := request("POST", "/close", string(closeBody), 200)
			if string(first) != string(second) || !strings.Contains(string(first), `"phase":"closed"`) {
				t.Fatalf("lost-reply replay differs: %s %s", first, second)
			}
			request("POST", "/prepare", string(prepare), 200)
			request("GET", "/status", "", 200)
		})
	}
}
