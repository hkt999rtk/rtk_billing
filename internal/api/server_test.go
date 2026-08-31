package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/hkt999rtk/rtk_billing/internal/accessstore"
	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
)

type testAudit struct{}

// Only an API wiring stub; database-backed authorization is tested separately.
type testOwnership struct{}

func (testOwnership) AuthorizeOwner(_ context.Context, org, user string, version int64) (billingidentity.Scope, error) {
	return billingidentity.Scope{OrganizationID: org, UserID: user, OwnershipVersion: version}, nil
}

func TestTenantContextRequiresGlobalUserIdentity(t *testing.T) {
	server := &Server{}
	for _, tc := range []struct {
		name, actorType, actorID, requestID string
		want                                int
	}{
		{"global user", "user", "global-user-1", "request-1", http.StatusNoContent},
		{"retired tenant identity", "brand_cloud_user", "legacy-user-1", "request-1", http.StatusBadRequest},
		{"app end user is separate", "end_user", "app-user-1", "request-1", http.StatusBadRequest},
		{"missing type", "", "global-user-1", "request-1", http.StatusBadRequest},
		{"missing user", "user", "", "request-1", http.StatusBadRequest},
		{"missing correlation", "user", "global-user-1", "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/context", server.requireTenantContext(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
			req := httptest.NewRequest(http.MethodGet, "/context", nil)
			req.Header.Set("X-Billing-Actor-Type", tc.actorType)
			req.Header.Set("X-Billing-Actor-ID", tc.actorID)
			req.Header.Set("X-Request-ID", tc.requestID)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status=%d want=%d", res.Code, tc.want)
			}
		})
	}
}

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
	server, err := New(Options{ServiceToken: strings.Repeat("s", 32), InternalToken: strings.Repeat("i", 32), Audit: testAudit{}, Access: access, Ownership: testOwnership{}})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, permission string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if permission != "unauthenticated" {
			req.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
			req.Header.Set("X-Billing-Permissions", permission)
			req.Header.Set("X-Billing-Actor-Type", "user")
			req.Header.Set("X-Billing-Actor-ID", "test-user")
			req.Header.Set("X-Request-ID", "test-request")
			req.Header.Set("X-Billing-Ownership-Version", "1")
		}
		res := httptest.NewRecorder()
		server.Router().ServeHTTP(res, req)
		return res
	}
	if got := request("GET", "/v1/orgs/00000000-0000-0000-0000-000000000001/billing/account", "unauthenticated").Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", got)
	}
	missingContext := httptest.NewRequest("GET", "/v1/orgs/00000000-0000-0000-0000-000000000001/billing/account", nil)
	missingContext.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
	missingContext.Header.Set("X-Billing-Permissions", "billing_account.read")
	missingContextResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(missingContextResponse, missingContext)
	if missingContextResponse.Code != http.StatusBadRequest {
		t.Fatalf("missing tenant context status=%d", missingContextResponse.Code)
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
	server, err := New(Options{ServiceToken: strings.Repeat("s", 32), InternalToken: strings.Repeat("i", 32), Audit: testAudit{}, Access: access, Ownership: testOwnership{}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/internal/billing/access/00000000-0000-0000-0000-000000000001", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("i", 32))
	res := httptest.NewRecorder()
	server.Router().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"state":"active"`) {
		t.Fatalf("access response=%d %s", res.Code, res.Body.String())
	}
	tenantCredential := httptest.NewRequest("GET", "/v1/internal/billing/access/00000000-0000-0000-0000-000000000001", nil)
	tenantCredential.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
	tenantCredentialResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(tenantCredentialResponse, tenantCredential)
	if tenantCredentialResponse.Code != http.StatusUnauthorized {
		t.Fatalf("tenant credential crossed internal boundary: %d", tenantCredentialResponse.Code)
	}
	internalCredential := httptest.NewRequest("GET", "/v1/orgs/00000000-0000-0000-0000-000000000001/billing/account", nil)
	internalCredential.Header.Set("Authorization", "Bearer "+strings.Repeat("i", 32))
	internalCredential.Header.Set("X-Billing-Permissions", "billing_account.read")
	internalCredential.Header.Set("X-Billing-Actor-Type", "user")
	internalCredential.Header.Set("X-Billing-Actor-ID", "test-user")
	internalCredential.Header.Set("X-Request-ID", "test-request")
	internalCredentialResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(internalCredentialResponse, internalCredential)
	if internalCredentialResponse.Code != http.StatusUnauthorized {
		t.Fatalf("internal credential crossed tenant boundary: %d", internalCredentialResponse.Code)
	}
}

func TestServerRejectsMissingOrReusedCredentialBoundaries(t *testing.T) {
	base := Options{ServiceToken: strings.Repeat("s", 32), InternalToken: strings.Repeat("i", 32), Audit: testAudit{}, Access: &testAccess{}, Ownership: testOwnership{}}
	if _, err := New(base); err != nil {
		t.Fatal(err)
	}
	withoutOwner := base
	withoutOwner.Ownership = nil
	if _, err := New(withoutOwner); err == nil {
		t.Fatal("missing owner verification passed")
	}
	base.InternalToken = ""
	if _, err := New(base); err == nil {
		t.Fatal("missing internal credential passed")
	}
	base.InternalToken = base.ServiceToken
	if _, err := New(base); err == nil {
		t.Fatal("reused tenant/internal credential passed")
	}
}

func TestOpenAPIPreservesTenantInternalAndDebitSecuritySchemes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	for operation, scheme := range map[string]string{
		"getBillingAccount":           "billingServiceAuth",
		"createBillingPricingVersion": "billingInternalAuth",
		"getBillingAccess":            "billingInternalAuth",
		"postInternalBillingDebit":    "billingDebitAuth",
	} {
		start := strings.Index(document, "operationId: "+operation)
		if start < 0 {
			t.Fatalf("OpenAPI operation %s is missing", operation)
		}
		end := start + 500
		if end > len(document) {
			end = len(document)
		}
		if !strings.Contains(document[start:end], "- "+scheme+": []") {
			t.Fatalf("OpenAPI operation %s does not use %s", operation, scheme)
		}
	}
}
