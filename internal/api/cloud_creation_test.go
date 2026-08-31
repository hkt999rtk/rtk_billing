package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

const creationToken = "isolated-billing-cloud-creation-credential"

func TestBillingWritersWaitForCreationInsteadOfOrphaningAccounts(t *testing.T) {
	env := newIntegrationAPI(t)
	ctx := context.Background()
	cloudID := testutil.OrganizationID("bootstrap-before-debit")
	body := map[string]any{"organization_id": cloudID, "amount_minor": 10, "currency": "TWD", "reason": "invoice_debit", "external_id": "bootstrap-invoice"}
	debit := func() *httptest.ResponseRecorder {
		return env.request(t, "POST", "/v1/internal/billing/debits", "", "bootstrap-debit", body)
	}
	for range 2 {
		if out := debit(); out.Code != 503 || !strings.Contains(out.Body.String(), "BILLING_ACCOUNT_NOT_READY") {
			t.Fatal("debit created an account without owner evidence", out.Code, out.Body.String())
		}
	}
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	out := env.request(t, "POST", "/v1/internal/billing/periods/close", "", "", map[string]any{"organization_id": cloudID, "period_start": start, "period_end": start.AddDate(0, 1, 0)})
	if out.Code != 503 || !strings.Contains(out.Body.String(), "BILLING_ACCOUNT_NOT_READY") {
		t.Fatal("period close did not wait for account provisioning", out.Code, out.Body.String())
	}
	var writes int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM commercial_accounts)+(SELECT count(*) FROM billing_invoices)+(SELECT count(*) FROM billing_periods)+(SELECT count(*) FROM balance_ledger_entries)+(SELECT count(*) FROM billing_responsibility_periods)`).Scan(&writes); err != nil || writes != 0 {
		t.Fatal("unprovisioned requests leaked writes", writes, err)
	}
	if err := env.server.ConfigureCloudCreation(CloudCreationAPIOptions{Token: creationToken, Store: paymentstore.New(env.db)}); err != nil {
		t.Fatal(err)
	}
	event := paymentstore.CloudCreation{EventID: testutil.OrganizationID("bootstrap-before-debit-event"), OrganizationID: cloudID, OwnerUserID: testutil.OrganizationID("integration-owner"), OwnershipVersion: 1, OccurredAt: start}
	event.EvidenceSHA256 = event.EvidenceDigest()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/internal/billing/cloud-creations", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+creationToken)
	created := httptest.NewRecorder()
	env.server.Router().ServeHTTP(created, req)
	if created.Code != 200 {
		t.Fatal("creation event failed after deferred debit", created.Code, created.Body.String())
	}
	if out := debit(); out.Code != 201 {
		t.Fatal("retry debit after provisioning", out.Code, out.Body.String())
	}
	if out := debit(); out.Code != 200 || !strings.Contains(out.Body.String(), `"duplicate":true`) {
		t.Fatal("duplicate debit", out.Code, out.Body.String())
	}
	var accounts, periods, entries int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM commercial_accounts), (SELECT count(*) FROM billing_responsibility_periods), (SELECT count(*) FROM balance_ledger_entries)`).Scan(&accounts, &periods, &entries); err != nil || accounts != 1 || periods != 1 || entries != 1 {
		t.Fatal("bootstrap/debit replay lost exactly-once invariants", accounts, periods, entries, err)
	}
}

type creationStoreFixture struct {
	err   error
	calls int
}

func (s *creationStoreFixture) BootstrapBrandCloud(ctx context.Context, e paymentstore.CloudCreation) (paymentstore.CloudCreationReceipt, error) {
	s.calls++
	if _, ok := ctx.Deadline(); !ok {
		panic("missing bounded context")
	}
	return paymentstore.CloudCreationReceipt{CloudCreation: e, AccountID: e.OrganizationID}, s.err
}
func TestCloudCreationRequiresSeparateCredentialAndBoundedEvidence(t *testing.T) {
	s := newHandoffTestServer(t)
	p := &creationStoreFixture{}
	path := "/v1/internal/billing/cloud-creations"
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, httptest.NewRequest("POST", path, nil))
	if w.Code != 404 {
		t.Fatal("disabled route exposed")
	}
	for _, token := range []string{"", "short", integrationServiceToken, integrationInternalToken, creationToken + " x"} {
		if err := s.ConfigureCloudCreation(CloudCreationAPIOptions{Token: token, Store: p}); err == nil {
			t.Fatal("unsafe credential")
		}
	}
	if err := s.ConfigureCloudCreation(CloudCreationAPIOptions{Token: creationToken}); err == nil {
		t.Fatal("missing store")
	}
	if err := s.ConfigureCloudCreation(CloudCreationAPIOptions{Token: creationToken, Store: p}); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureCloudCreation(CloudCreationAPIOptions{Token: creationToken, Store: p}); err == nil {
		t.Fatal("reconfigured")
	}
	if err := s.ConfigureHandoff(HandoffAPIOptions{Token: creationToken, Store: paymentstore.New(nil)}); err == nil {
		t.Fatal("handoff credential reused")
	}
	if err := s.ConfigurePayments(PaymentAPIOptions{Store: paymentstore.New(nil), BillingDebitToken: creationToken, BillingDebitSource: "test"}); err == nil {
		t.Fatal("debit credential reused")
	}
	e := paymentstore.CloudCreation{EventID: testutil.OrganizationID("api-create-event"), OrganizationID: testutil.OrganizationID("api-create-cloud"), OwnerUserID: testutil.OrganizationID("api-create-owner"), OwnershipVersion: 1, OccurredAt: time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)}
	e.EvidenceSHA256 = e.EvidenceDigest()
	raw, _ := json.Marshal(e)
	for _, tc := range []struct {
		token, body string
		err         error
		code        int
	}{{"", string(raw), nil, 401}, {integrationServiceToken, string(raw), nil, 401}, {testHandoffToken, string(raw), nil, 401}, {creationToken, `{}`, nil, 400}, {creationToken, string(raw) + ` {}`, nil, 400}, {creationToken, strings.Repeat(" ", 16<<10) + string(raw), nil, 400}, {creationToken, string(raw), paymentstore.ErrConflict, 409}, {creationToken, string(raw), paymentstore.ErrOwnershipEvidenceMissing, 409}, {creationToken, string(raw), paymentstore.ErrIdempotencyConflict, 409}, {creationToken, string(raw), errors.New("private connection details"), 503}, {creationToken, string(raw), nil, 200}} {
		p.err = tc.err
		req := httptest.NewRequest("POST", path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, req)
		if w.Code != tc.code || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "private") {
			t.Fatalf("response %d want %d", w.Code, tc.code)
		}
	}
	for _, token := range []string{testHandoffToken, integrationDebitToken} {
		server := newHandoffTestServer(t)
		if err := server.ConfigurePayments(PaymentAPIOptions{Store: paymentstore.New(nil), BillingDebitToken: integrationDebitToken, BillingDebitSource: "test"}); err != nil {
			t.Fatal(err)
		}
		if err := server.ConfigureHandoff(HandoffAPIOptions{Token: testHandoffToken, Store: paymentstore.New(nil)}); err != nil {
			t.Fatal(err)
		}
		if err := server.ConfigureCloudCreation(CloudCreationAPIOptions{Token: token, Store: p}); err == nil {
			t.Fatal("reverse credential reuse")
		}
	}
}
