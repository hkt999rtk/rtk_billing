package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

func TestOwnershipEligibilityHTTPBoundary(t *testing.T) {
	f := newHandoffHTTPFixture(t, 1)
	certifyCloudPreflightFixture(t, f)
	path := "/v1/internal/billing/clouds/" + f.scope.OrganizationID + "/ownership-eligibility"
	valid := `{"target_user_id":"` + f.target + `","action":"request"}`
	for _, tc := range []struct {
		name, token, owner, version, body string
		status                            int
	}{
		{"valid", testHandoffToken, f.source, "1", valid, 200},
		{"no auth", "", f.source, "1", valid, 401},
		{"debit token", integrationDebitToken, f.source, "1", valid, 401},
		{"service token", integrationServiceToken, f.source, "1", valid, 401},
		{"wrong owner", testHandoffToken, f.target, "1", `{"target_user_id":"` + f.source + `","action":"request"}`, 409},
		{"wrong version", testHandoffToken, f.source, "2", valid, 409},
		{"noncanonical version", testHandoffToken, f.source, "01", valid, 400},
		{"unknown fields", testHandoffToken, f.source, "1", `{"target_user_id":"` + f.target + `","action":"request","balance_minor":0}`, 400},
		{"missing acceptance id", testHandoffToken, f.source, "1", `{"target_user_id":"` + f.target + `","action":"accept"}`, 400},
		{"trailing document", testHandoffToken, f.source, "1", valid + `{}`, 400},
		{"null", testHandoffToken, f.source, "1", `null`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Billing-Owner-User-ID", tc.owner)
			req.Header.Set("X-Billing-Ownership-Version", tc.version)
			res := httptest.NewRecorder()
			f.env.server.Router().ServeHTTP(res, req)
			if res.Code != tc.status || res.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("boundary: %d %s", res.Code, res.Body.String())
			}
			if tc.status == 200 {
				var out paymentstore.OwnershipEligibility
				if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil || !out.Complete || out.BalanceMinor != 1 || len(out.Blockers) != 0 || out.Request.TargetUserID != f.target {
					t.Fatalf("response: %+v %v", out, err)
				}
				for _, private := range []string{"provider_reference", "invoice", "payment_method", "legal_name", "tax_id"} {
					if strings.Contains(res.Body.String(), private) {
						t.Fatalf("private financial data exposed: %s", private)
					}
				}
			}
		})
	}
	unconfigured := newHandoffTestServer(t)
	res := httptest.NewRecorder()
	unconfigured.Router().ServeHTTP(res, httptest.NewRequest(http.MethodPost, path, strings.NewReader(valid)))
	if res.Code != 404 {
		t.Fatalf("unconfigured route: %d", res.Code)
	}
}

func TestOwnershipEligibilityAccountManagerTransport(t *testing.T) {
	dir := os.Getenv("ACCOUNT_MANAGER_HANDOFF_CLIENT_DIR")
	if dir == "" {
		t.Skip("requires isolated AM checkout")
	}
	for _, balance := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprint(balance), func(t *testing.T) {
			f := newHandoffHTTPFixture(t, balance)
			certifyCloudPreflightFixture(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "go", "test", "./internal/billinghandoff", "-run", "^TestLiveOwnershipEligibilityTransport$", "-count=1")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOWORK=off", "TEST_BILLING_HANDOFF_URL="+f.server.URL, "TEST_BILLING_HANDOFF_TOKEN="+testHandoffToken, "TEST_HANDOFF_CLOUD="+f.scope.OrganizationID, "TEST_HANDOFF_SOURCE="+f.source, "TEST_HANDOFF_TARGET="+f.target, fmt.Sprintf("TEST_HANDOFF_BALANCE=%d", balance))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("real AM transport: %v\n%s", err, output)
			}
		})
	}
}
