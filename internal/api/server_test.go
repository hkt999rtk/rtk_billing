package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/accessstore"
)

type testAudit struct{}

func (testAudit) CreateAuditEvent(context.Context, AuditEventInput) error { return nil }

type testAccess struct{ state accessstore.State }

func (a *testAccess) GetOrCreate(_ context.Context, organizationID string) (accessstore.State, error) {
	out := a.state
	out.OrganizationID = organizationID
	if out.State == "" {
		out.State = "active"
		out.Version = 1
	}
	return out, nil
}
func (a *testAccess) Put(_ context.Context, organizationID, state, reason, actor string, version int64) (accessstore.State, error) {
	a.state = accessstore.State{OrganizationID: organizationID, State: state, ReasonCode: reason, UpdatedBy: actor, Version: version + 1}
	return a.state, nil
}

func TestServiceAuthenticationPermissionAndAccessStateFailClosed(t *testing.T) {
	access := &testAccess{}
	server, err := New(Options{ServiceToken: strings.Repeat("s", 32), Audit: testAudit{}, Access: access})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, permission string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if permission != "unauthenticated" {
			req.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
			req.Header.Set("X-Billing-Permissions", permission)
		}
		res := httptest.NewRecorder()
		server.Router().ServeHTTP(res, req)
		return res
	}
	if got := request("GET", "/v1/orgs/00000000-0000-0000-0000-000000000001/billing/account", "unauthenticated").Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", got)
	}
	if got := request("GET", "/v1/orgs/00000000-0000-0000-0000-000000000001/billing/account", "invoice.read").Code; got != http.StatusForbidden {
		t.Fatalf("wrong permission status=%d", got)
	}
	access.state = accessstore.State{State: "read_only", Version: 1}
	if got := request("POST", "/v1/orgs/00000000-0000-0000-0000-000000000001/topups", "payment_intent.create").Code; got != http.StatusForbidden {
		t.Fatalf("read-only write status=%d", got)
	}
	access.state = accessstore.State{State: "suspended", Version: 1}
	if got := request("GET", "/v1/orgs/00000000-0000-0000-0000-000000000001/billing/account", "billing_account.read").Code; got != http.StatusForbidden {
		t.Fatalf("suspended read status=%d", got)
	}
}

func TestInternalAccessStateIsOwnedByBilling(t *testing.T) {
	access := &testAccess{}
	server, err := New(Options{ServiceToken: strings.Repeat("s", 32), Audit: testAudit{}, Access: access})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/internal/billing/access/00000000-0000-0000-0000-000000000001", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
	res := httptest.NewRecorder()
	server.Router().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"state":"active"`) {
		t.Fatalf("access response=%d %s", res.Code, res.Body.String())
	}
}
