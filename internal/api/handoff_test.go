package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

func newHandoffTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Options{ServiceToken: integrationServiceToken, InternalToken: integrationInternalToken,
		Audit: testAudit{}, Access: &testAccess{}, Ownership: testOwnership{}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHandoffRoutesAreAbsentUntilDedicatedConfiguration(t *testing.T) {
	s := newHandoffTestServer(t)
	path := "/v1/internal/billing/clouds/11111111-1111-1111-1111-111111111111/ownership-handoffs/22222222-2222-2222-2222-222222222222/prepare"
	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+testHandoffToken)
	s.Router().ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("unconfigured handoff exposed: %d", res.Code)
	}
	for _, token := range []string{"", "short", integrationServiceToken, integrationInternalToken} {
		if err := s.ConfigureHandoff(HandoffAPIOptions{Token: token, Store: paymentstore.New(nil)}); err == nil {
			t.Fatal("invalid boundary configured")
		}
	}
	if err := s.ConfigureHandoff(HandoffAPIOptions{Token: testHandoffToken}); err == nil {
		t.Fatal("nil store configured")
	}
	if err := s.ConfigurePayments(PaymentAPIOptions{Store: paymentstore.New(nil), BillingDebitToken: integrationDebitToken, BillingDebitSource: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureHandoff(HandoffAPIOptions{Token: integrationDebitToken, Store: paymentstore.New(nil)}); err == nil {
		t.Fatal("debit boundary reused")
	}
	if err := s.ConfigureHandoff(HandoffAPIOptions{Token: testHandoffToken, Store: paymentstore.New(nil)}); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureHandoff(HandoffAPIOptions{Token: strings.Repeat("z", 32), Store: paymentstore.New(nil)}); err == nil {
		t.Fatal("live runtime reconfigured")
	}
	if err := s.ConfigurePayments(PaymentAPIOptions{Store: paymentstore.New(nil), BillingDebitToken: testHandoffToken, BillingDebitSource: "test"}); err == nil {
		t.Fatal("reverse configuration order reused handoff credential")
	}
}

type unavailableHandoff struct {
	handoffPersistence
	t *testing.T
}

func (s unavailableHandoff) PrepareOwnershipHandoff(ctx context.Context, _ paymentstore.PrepareOwnershipHandoffInput) (paymentstore.OwnershipHandoff, error) {
	if _, ok := ctx.Deadline(); !ok {
		s.t.Fatal("remote request has no deadline")
	}
	return paymentstore.OwnershipHandoff{}, errors.New("sensitive database connection diagnostics")
}

func TestHandoffUnavailableNeverApprovesOrDisclosesDiagnostics(t *testing.T) {
	s := newHandoffTestServer(t)
	if err := s.ConfigureHandoff(HandoffAPIOptions{Token: testHandoffToken, Store: unavailableHandoff{t: t}}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/internal/billing/clouds/11111111-1111-1111-1111-111111111111/ownership-handoffs/22222222-2222-2222-2222-222222222222/prepare"
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"source_user_id":"33333333-3333-3333-3333-333333333333","target_user_id":"44444444-4444-4444-4444-444444444444","cutoff":"2026-01-01T00:00:00Z"}`))
	req.Header.Set("Authorization", "Bearer "+testHandoffToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Billing-Ownership-Version", "1")
	res := httptest.NewRecorder()
	s.Router().ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable || strings.Contains(res.Body.String(), "sensitive") || strings.Contains(res.Body.String(), "diagnostics") || !strings.Contains(res.Body.String(), "BILLING_HANDOFF_UNAVAILABLE") {
		t.Fatalf("unsafe unavailable response: %d %s", res.Code, res.Body.String())
	}
}
