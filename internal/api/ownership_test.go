package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/fake"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

const allBillingPermissions = "billing_account.read,billing_summary.read,billing_usage.read,invoice.read,invoice_document.read,billing_activity.read,billing_profile.read,billing_profile.manage,billing_statement.export,billing_ledger.read,payment_method.read,payment_method.manage,auto_topup.read,auto_topup.manage,payment_intent.create,payment_intent.read"

func ownerRequest(env integrationAPI, method, path, user, version string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+integrationServiceToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Billing-Actor-Type", "user")
	req.Header.Set("X-Billing-Actor-ID", user)
	req.Header.Set("X-Billing-Ownership-Version", version)
	req.Header.Set("X-Billing-Permissions", allBillingPermissions)
	req.Header.Set("X-Request-ID", "owner-boundary-test")
	// Neither a claimed role nor platform capability upgrades the tenant actor.
	req.Header.Set("X-Billing-Role", "owner")
	req.Header.Set("X-Platform-Admin", "true")
	res := httptest.NewRecorder()
	env.server.Router().ServeHTTP(res, req)
	return res
}

func TestEveryTenantRouteRequiresCurrentOwnerRegardlessOfPermissions(t *testing.T) {
	env := newIntegrationAPI(t)
	org := testutil.OrganizationID("owner-route-cloud")
	env.provisionOwner(t, org)
	for _, role := range []string{"admin", "member", "viewer", "other-cloud-owner", "transfer-target", "platform-only"} {
		actor := testutil.OrganizationID(role)
		covered := 0
		for _, route := range env.server.router.Routes() {
			if !strings.HasPrefix(route.Path, "/v1/orgs/:orgId/") {
				continue
			}
			path := strings.NewReplacer(":orgId", org, ":invoiceId", testutil.OrganizationID("invoice"), ":activityId", testutil.OrganizationID("activity"), ":methodId", testutil.OrganizationID("method"), ":intentId", testutil.OrganizationID("intent")).Replace(route.Path)
			res := ownerRequest(env, route.Method, path, actor, "1")
			if res.Code != http.StatusForbidden || !strings.Contains(res.Body.String(), "BILLING_OWNER_REQUIRED") {
				t.Fatalf("%s %s actor=%s status=%d body=%s", route.Method, path, role, res.Code, res.Body.String())
			}
			covered++
		}
		if covered < 22 {
			t.Fatalf("tenant route inventory unexpectedly small: %d", covered)
		}
	}
	if res := ownerRequest(env, "GET", "/v1/orgs/"+org+"/billing/account", testutil.OrganizationID("integration-owner"), "1"); res.Code != http.StatusOK || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("owner rejected=%d %s", res.Code, res.Body.String())
	}
}

func TestTenantOwnershipVersionAndMissingProjectionFailClosed(t *testing.T) {
	env := newIntegrationAPI(t)
	org := testutil.OrganizationID("owner-context-cloud")
	env.provisionOwner(t, org)
	path := "/v1/orgs/" + org + "/billing/account"
	owner := testutil.OrganizationID("integration-owner")
	for _, version := range []string{"", "0", "-1", "01", "+1", " 1", "1 ", "1.0", "9223372036854775808"} {
		res := ownerRequest(env, "GET", path, owner, version)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("malformed version %q status=%d body=%s", version, res.Code, res.Body.String())
		}
	}
	if res := ownerRequest(env, "GET", path, owner, "2"); res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "BILLING_OWNERSHIP_VERSION_CONFLICT") {
		t.Fatalf("stale version=%d %s", res.Code, res.Body.String())
	}
	if res := ownerRequest(env, "GET", path, "legacy-local-id", "1"); res.Code != http.StatusBadRequest {
		t.Fatalf("nonglobal actor=%d %s", res.Code, res.Body.String())
	}
	unknown := testutil.OrganizationID("unknown-owner-cloud")
	if res := ownerRequest(env, "GET", "/v1/orgs/"+unknown+"/billing/account", owner, "1"); res.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown ownership=%d %s", res.Code, res.Body.String())
	}
	var count int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM commercial_accounts WHERE organization_id=$1`, unknown).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unauthorized GET provisioned account=%d err=%v", count, err)
	}
	store := paymentstore.New(env.db)
	if _, _, err := store.EnsureCommercialAccount(context.Background(), unknown, payment.CurrencyTWD); err != nil {
		t.Fatal(err)
	}
	if res := ownerRequest(env, "GET", "/v1/orgs/"+unknown+"/billing/account", owner, "1"); res.Code != http.StatusServiceUnavailable {
		t.Fatalf("legacy account inferred owner=%d %s", res.Code, res.Body.String())
	}
}

type handoffDuringSetupProvider struct {
	payment.PaymentProvider
	during func()
}

func (p *handoffDuringSetupProvider) CreateSetup(ctx context.Context, in payment.SetupRequest) (payment.SetupResult, error) {
	p.during()
	return p.PaymentProvider.CreateSetup(ctx, in)
}

func TestOwnershipChangeDuringProviderSetupRetainsEvidenceWithoutBrowserAccess(t *testing.T) {
	base := fake.New("owner-handoff-provider-secret")
	base.QueueSetup(fake.SetupOutcome{Result: payment.SetupResult{State: payment.PaymentIntentStateRequiresAction, RequiresUserAction: true, HostedURL: "https://provider.invalid/old-owner-action", ProviderCode: "pending"}})
	provider := &handoffDuringSetupProvider{PaymentProvider: base}
	env := newIntegrationAPI(t, provider)
	org := testutil.OrganizationID("inflight-setup-owner")
	env.provisionOwner(t, org)
	store := paymentstore.New(env.db)
	ctx := context.Background()
	provider.during = func() {
		// Synthetic collector and AM decisions prove local race handling only.
		prepare := paymentstore.PrepareOwnershipHandoffInput{OrganizationID: org, OperationID: testutil.OrganizationID("inflight-setup-transfer"), SourceUserID: testutil.OrganizationID("integration-owner"), TargetUserID: testutil.OrganizationID("new-setup-owner"), OwnershipVersion: 1, Cutoff: time.Now().UTC()}
		if _, err := store.PrepareOwnershipHandoff(ctx, prepare); err != nil {
			t.Fatal(err)
		}
		scope := paymentstore.HandoffScope{OrganizationID: org, OperationID: prepare.OperationID, OwnershipVersion: 1}
		state, err := store.CaptureHandoffSettlementState(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		state.Financial.UsageSettled, state.Financial.InvoicesReconciled, state.Financial.ProviderWorkReconciled = true, true, true
		status, err := store.RecordHandoffSettlement(ctx, paymentstore.RecordSettlementInput{Scope: scope, ReceiptID: testutil.OrganizationID("inflight-setup-receipt"), StateSHA256: state.SHA256, Financial: state.Financial, UsageCheckpointSHA256: strings.Repeat("a", 64), InvoiceCheckpointSHA256: strings.Repeat("b", 64), ProviderCheckpointSHA256: strings.Repeat("c", 64)})
		if err != nil || status.Snapshot == nil {
			t.Fatalf("settlement=%+v err=%v", status, err)
		}
		for _, user := range []string{prepare.SourceUserID, prepare.TargetUserID} {
			if _, err := store.ConfirmHandoffSnapshot(ctx, paymentstore.ConfirmHandoffSnapshotInput{Scope: scope, UserID: user, SnapshotVersion: status.Snapshot.Version, BalanceMinor: status.Snapshot.BalanceMinor, Currency: status.Snapshot.Currency}); err != nil {
				t.Fatal(err)
			}
		}
		grant, err := store.AuthorizeHandoffCommit(ctx, paymentstore.AuthorizeHandoffCommitInput{Scope: scope, AuthorizationID: testutil.OrganizationID("inflight-setup-grant"), SnapshotVersion: status.Snapshot.Version})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.FinalizeOwnershipHandoff(ctx, paymentstore.FinalizeHandoffInput{Scope: scope, AuthorizationID: grant.AuthorizationID, CommittedOwnerUserID: prepare.TargetUserID, CommittedOwnershipVersion: 2, CommittedAt: grant.CreatedAt.Add(time.Microsecond), AMCommitSHA256: strings.Repeat("d", 64)}); err != nil {
			t.Fatal(err)
		}
	}
	res := env.request(t, "POST", "/v1/orgs/"+org+"/payment-methods/setup", "payment_method.manage", "inflight-setup", map[string]any{"provider": "fake", "consent": map[string]any{"accepted": true, "text_version": "v1", "text_sha256": strings.Repeat("a", 64), "locale": "zh-TW"}})
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "PAYMENT_SETUP_INVALIDATED") || strings.Contains(res.Body.String(), "old-owner-action") {
		t.Fatalf("old browser result=%d %s", res.Code, res.Body.String())
	}
	var observations, active int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM billing_handoff_setup_observations),(SELECT count(*) FROM payment_methods WHERE status='active')`).Scan(&observations, &active); err != nil || observations != 1 || active != 0 {
		t.Fatalf("late result dropped or activated: observations=%d active=%d err=%v", observations, active, err)
	}
}
