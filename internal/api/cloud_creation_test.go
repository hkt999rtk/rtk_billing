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
