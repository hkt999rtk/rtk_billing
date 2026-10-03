package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/rawretention"
)

type rawRetentionAPIStub struct {
	*rawretention.Store
	resolved      bool
	abortedIntent *rawretention.Plan
	err           error
}

func (s *rawRetentionAPIStub) PutPolicy(_ context.Context, p rawretention.Policy) (rawretention.PolicyState, error) {
	return rawretention.PolicyState{Policy: p}, s.err
}
func (s *rawRetentionAPIStub) Policy(_ context.Context, id string, version int64) (rawretention.PolicyState, error) {
	return rawretention.PolicyState{Policy: rawretention.Policy{PolicyID: id, Version: version}}, s.err
}
func (s *rawRetentionAPIStub) ApproveRecovery(_ context.Context, id string, version int64, sha, ref string) (rawretention.PolicyState, error) {
	return rawretention.PolicyState{Policy: rawretention.Policy{PolicyID: id, Version: version}, PolicySHA256: sha, RecoveryApprovalRef: ref}, s.err
}
func (s *rawRetentionAPIStub) SetActive(_ context.Context, id string, version int64, sha string, active bool) (rawretention.PolicyState, error) {
	return rawretention.PolicyState{Policy: rawretention.Policy{PolicyID: id, Version: version}, PolicySHA256: sha, Active: active}, s.err
}
func (s *rawRetentionAPIStub) PutClearance(_ context.Context, p rawretention.Clearance) (rawretention.Clearance, error) {
	return p, s.err
}
func (s *rawRetentionAPIStub) RevokeClearance(context.Context, string) error { return s.err }
func (s *rawRetentionAPIStub) PutHold(_ context.Context, p rawretention.Hold) (rawretention.HoldState, error) {
	return rawretention.HoldState{Hold: p, Status: "ACTIVE"}, s.err
}
func (s *rawRetentionAPIStub) Hold(_ context.Context, id string) (rawretention.HoldState, error) {
	return rawretention.HoldState{Hold: rawretention.Hold{HoldID: id}, Status: "ACTIVE"}, s.err
}
func (s *rawRetentionAPIStub) ReleaseHold(_ context.Context, id string) (rawretention.HoldState, error) {
	return rawretention.HoldState{Hold: rawretention.Hold{HoldID: id}, Status: "RELEASED"}, s.err
}
func (s *rawRetentionAPIStub) RequestOperation(_ context.Context, p rawretention.Plan) (rawretention.Operation, error) {
	return rawretention.Operation{Plan: p, Status: "ACTIVE"}, s.err
}
func (s *rawRetentionAPIStub) Operations(_ context.Context, scope rawretention.Scope) ([]rawretention.Operation, error) {
	return []rawretention.Operation{{Plan: rawretention.Plan{Scope: scope}, Status: "ACTIVE"}}, s.err
}
func (s *rawRetentionAPIStub) RequestAbort(_ context.Context, id string) (rawretention.Operation, error) {
	return rawretention.Operation{Plan: rawretention.Plan{OperationID: id}, Status: "ABORT_REQUESTED"}, s.err
}

func (s *rawRetentionAPIStub) RequestAbortPlan(_ context.Context, plan rawretention.Plan) (rawretention.Operation, error) {
	s.abortedIntent = &plan
	return rawretention.Operation{Plan: plan, Status: "ABORTED", DecisionOrigin: "cancelled-before-acceptance"}, s.err
}

func (s *rawRetentionAPIStub) Operation(_ context.Context, id string) (rawretention.Operation, error) {
	return rawretention.Operation{Plan: rawretention.Plan{OperationID: id}, Status: "ACTIVE"}, s.err
}
func (s *rawRetentionAPIStub) Resolve(_ context.Context, id string) (rawretention.Operation, error) {
	s.resolved = true
	return s.Operation(context.Background(), id)
}

func rawAPIFixture(t *testing.T) (*Server, *rawRetentionAPIStub, RawRetentionAPIOptions) {
	t.Helper()
	server, err := New(Options{ServiceToken: strings.Repeat("s", 32), InternalToken: strings.Repeat("i", 32), Audit: testAudit{}, Access: &testAccess{}, Ownership: testOwnership{}})
	if err != nil {
		t.Fatal(err)
	}
	store := &rawRetentionAPIStub{}
	opts := RawRetentionAPIOptions{FinancialToken: strings.Repeat("f", 32), RecoveryToken: strings.Repeat("r", 32), ControllerToken: strings.Repeat("c", 32), AuthorityReadToken: strings.Repeat("a", 32), Store: store}
	return server, store, opts
}

func TestRawRetentionDefaultAbsentAndRolesSeparated(t *testing.T) {
	server, _, opts := rawAPIFixture(t)
	request := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		server.RawRetentionRouter().ServeHTTP(res, req)
		return res
	}
	path := "/v1/internal/billing/raw-retention/operations/retire-1"
	if got := request(http.MethodGet, path, opts.ControllerToken, "").Code; got != 404 {
		t.Fatalf("default route present: %d", got)
	}
	if err := server.ConfigureRawRetention(opts); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{opts.FinancialToken, opts.RecoveryToken, opts.ControllerToken, opts.AuthorityReadToken, ""} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req := httptest.NewRequest(method, path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+token)
			res := httptest.NewRecorder()
			server.Router().ServeHTTP(res, req)
			if res.Code != http.StatusNotFound {
				t.Fatalf("public listener exposed retention route for %s: %d", method, res.Code)
			}
		}
	}
	for _, token := range []string{strings.Repeat("i", 32), strings.Repeat("s", 32), opts.FinancialToken, opts.RecoveryToken} {
		if got := request(http.MethodGet, path, token, "").Code; got != 401 {
			t.Fatalf("other authority read decision: %d", got)
		}
	}
	for _, token := range []string{opts.ControllerToken, opts.AuthorityReadToken} {
		res := request(http.MethodGet, path, token, "")
		if res.Code != 200 || res.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("dedicated authority read: %d", res.Code)
		}
	}
	if got := request(http.MethodPost, "/v1/internal/billing/raw-retention/operations", opts.AuthorityReadToken, "{}").Code; got != 401 {
		t.Fatalf("Logger read token minted decision: %d", got)
	}
	if got := request(http.MethodPost, "/v1/internal/billing/raw-retention/policies", opts.ControllerToken, "{}").Code; got != 401 {
		t.Fatalf("controller approved policy: %d", got)
	}
}

func TestRawRetentionResolveRefusesForgedTerminalJSON(t *testing.T) {
	server, store, opts := rawAPIFixture(t)
	if err := server.ConfigureRawRetention(opts); err != nil {
		t.Fatal(err)
	}
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/internal/billing/raw-retention/operations/retire-1/resolve", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+opts.ControllerToken)
		res := httptest.NewRecorder()
		server.RawRetentionRouter().ServeHTTP(res, req)
		return res
	}
	if res := request(`{"status":"completed","receipt_sha256":"forged"}`); res.Code != 400 || store.resolved {
		t.Fatal("caller forged terminal receipt accepted")
	}
	if res := request(`{}`); res.Code != 200 || !store.resolved {
		t.Fatal("empty resolve command did not delegate authoritative fetch")
	}
}

func TestRawRetentionCredentialReuseRejected(t *testing.T) {
	server, _, opts := rawAPIFixture(t)
	opts.AuthorityReadToken = opts.ControllerToken
	if err := server.ConfigureRawRetention(opts); err == nil {
		t.Fatal("writer/controller credentials collapsed")
	}
	_, _, opts = rawAPIFixture(t)
	opts.ControllerToken = strings.Repeat("i", 32)
	if err := server.ConfigureRawRetention(opts); err == nil {
		t.Fatal("general internal credential accepted")
	}
}

func TestRawRetentionUnknownAbortRequiresMatchingFullPlanAndController(t *testing.T) {
	server, store, opts := rawAPIFixture(t)
	if err := server.ConfigureRawRetention(opts); err != nil {
		t.Fatal(err)
	}
	request := func(token, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/internal/billing/raw-retention/operations/unknown-intent/abort", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		server.RawRetentionRouter().ServeHTTP(res, req)
		return res.Code
	}
	if request(opts.ControllerToken, `{"operation_id":"different-intent"}`) != 400 || store.abortedIntent != nil {
		t.Fatal("unknown abort path/body identity mismatch accepted")
	}
	if request(opts.AuthorityReadToken, `{"operation_id":"unknown-intent"}`) != 401 || store.abortedIntent != nil {
		t.Fatal("Logger read credential cancelled an unknown authority intent")
	}
	if request(opts.ControllerToken, `{"operation_id":"unknown-intent","status":"completed"}`) != 400 || store.abortedIntent != nil {
		t.Fatal("unknown cancellation accepted forged outcome fields")
	}
	if request(opts.ControllerToken, `{"operation_id":"unknown-intent"}`) != 200 || store.abortedIntent == nil {
		t.Fatal("full-plan abort was not delegated for store validation")
	}
}

func TestRawRetentionPrivateRoutesDelegateOnlyDedicatedRoles(t *testing.T) {
	server, store, opts := rawAPIFixture(t)
	if err := server.ConfigureRawRetention(opts); err != nil {
		t.Fatal(err)
	}
	const base = "/v1/internal/billing/raw-retention"
	for _, route := range []struct{ method, path, token, body, expected string }{
		{http.MethodPost, "/policies", opts.FinancialToken, `{"policy_id":"policy"}`, `"policy_id":"policy"`},
		{http.MethodPost, "/policies/policy/1/activate", opts.FinancialToken, `{"policy_sha256":"hash"}`, `"active":true`},
		{http.MethodPost, "/policies/policy/1/deactivate", opts.FinancialToken, `{"policy_sha256":"hash"}`, `"active":false`},
		{http.MethodPost, "/policies/policy/1/approve", opts.RecoveryToken, `{"policy_sha256":"hash","recovery_approval_ref":"approval"}`, `"recovery_approval_ref":"approval"`},
		{http.MethodGet, "/policies/policy/1", opts.ControllerToken, "", `"version":1`},
		{http.MethodPost, "/clearances", opts.FinancialToken, `{"clearance_id":"clearance"}`, `"clearance_id":"clearance"`},
		{http.MethodPost, "/clearances/clearance/revoke", opts.FinancialToken, "", `"revoked":true`},
		{http.MethodPost, "/holds", opts.FinancialToken, `{"hold_id":"hold"}`, `"hold_id":"hold"`},
		{http.MethodGet, "/holds/hold", opts.FinancialToken, "", `"status":"ACTIVE"`},
		{http.MethodPost, "/holds/hold/release", opts.FinancialToken, `{}`, `"status":"RELEASED"`},
		{http.MethodPost, "/operations", opts.ControllerToken, `{"operation_id":"operation"}`, `"operation_id":"operation"`},
		{http.MethodGet, "/operations?environment=dev&store_id=store", opts.ControllerToken, "", `"environment":"dev"`},
		{http.MethodPost, "/operations/operation/abort", opts.ControllerToken, "", `"status":"ABORT_REQUESTED"`},
	} {
		t.Run(route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, base+route.path, strings.NewReader(route.body))
			req.Header.Set("Authorization", "Bearer "+route.token)
			req.Header.Set("Content-Type", "application/json")
			res := httptest.NewRecorder()
			server.RawRetentionRouter().ServeHTTP(res, req)
			if res.Code != 200 || !strings.Contains(res.Body.String(), route.expected) || res.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("route did not preserve typed command or private response", res.Code, res.Body.String())
			}
			// Logger's writer credential cannot use any of these financial,
			// recovery, list or mutation capabilities.
			req = httptest.NewRequest(route.method, base+route.path, strings.NewReader(route.body))
			req.Header.Set("Authorization", "Bearer "+opts.AuthorityReadToken)
			req.Header.Set("Content-Type", "application/json")
			res = httptest.NewRecorder()
			server.RawRetentionRouter().ServeHTTP(res, req)
			if res.Code != 401 {
				t.Fatal("read-only authority acquired another role", res.Code)
			}
		})
	}
	for _, failure := range []struct {
		err    error
		status int
	}{{rawretention.ErrInvalid, 400}, {rawretention.ErrNotFound, 404}, {rawretention.ErrConflict, 409}, {rawretention.ErrBlocked, 409}, {errors.New("private database failure detail"), 503}} {
		store.err = failure.err
		req := httptest.NewRequest(http.MethodGet, base+"/operations/operation", nil)
		req.Header.Set("Authorization", "Bearer "+opts.ControllerToken)
		res := httptest.NewRecorder()
		server.RawRetentionRouter().ServeHTTP(res, req)
		if res.Code != failure.status || strings.Contains(res.Body.String(), "private database") {
			t.Fatal("unsafe authority error mapping", res.Code, res.Body.String())
		}
	}
}

func TestRawRetentionStrictVersionsJSONAndConfiguration(t *testing.T) {
	server, _, opts := rawAPIFixture(t)
	if err := server.ConfigureRawRetention(RawRetentionAPIOptions{}); err == nil {
		t.Fatal("nil authority store accepted")
	}
	bad := opts
	bad.FinancialToken = "short"
	if err := server.ConfigureRawRetention(bad); err == nil {
		t.Fatal("short authority credential accepted")
	}
	if err := server.ConfigureRawRetention(opts); err != nil {
		t.Fatal(err)
	}
	if err := server.ConfigureRawRetention(opts); err == nil {
		t.Fatal("configured authority replaced")
	}
	const base = "/v1/internal/billing/raw-retention"
	for _, tc := range []struct{ path, token, body, contentType string }{
		{"/policies/policy/01/activate", opts.FinancialToken, `{}`, "application/json"},
		{"/policies/policy/0/deactivate", opts.FinancialToken, `{}`, "application/json"},
		{"/policies/policy/no/approve", opts.RecoveryToken, `{}`, "application/json"},
		{"/policies", opts.FinancialToken, `{}`, "text/plain"},
		{"/policies", opts.FinancialToken, `{} {}`, "application/json"},
		{"/policies/policy/1/activate", opts.FinancialToken, `{"untrusted":true}`, "application/json"},
		{"/operations/operation/abort", opts.ControllerToken, `{"policy_id":"without-operation"}`, "application/json"},
		{"/holds/hold/release", opts.FinancialToken, `{"status":"released"}`, "application/json"},
		{"/clearances/clearance/revoke", opts.FinancialToken, `{"revoked":true}`, "application/json"},
	} {
		req := httptest.NewRequest(http.MethodPost, base+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("Content-Type", tc.contentType)
		res := httptest.NewRecorder()
		server.RawRetentionRouter().ServeHTTP(res, req)
		if res.Code != 400 {
			t.Fatal("noncanonical version or untrusted command fields accepted", tc.path, res.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, base+"/policies/policy/01", nil)
	req.Header.Set("Authorization", "Bearer "+opts.ControllerToken)
	res := httptest.NewRecorder()
	server.RawRetentionRouter().ServeHTTP(res, req)
	if res.Code != 400 {
		t.Fatal("noncanonical read version accepted")
	}
}
