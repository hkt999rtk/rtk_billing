package paymentsimulator

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProductionConfigurationFailsClosed(t *testing.T) {
	_, err := New(nil, Config{Environment: "production"})
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("production must fail before startup: %v", err)
	}
}

func TestScenarioNormalizationAndOutcomes(t *testing.T) {
	if normalizeScenario("") != "success" || !validScenario("success") || validScenario("real-charge") {
		t.Fatal("unexpected scenario validation")
	}
	state, code := scenarioResult("declined")
	if state != "failed" || code != "simulator_declined" {
		t.Fatalf("state=%s code=%s", state, code)
	}
}

func TestRunIDValidationRejectsCrossScopeOrPathSyntax(t *testing.T) {
	for _, valid := range []string{"gh-123-1", "local.20260817_001", "staging"} {
		if !validRunID(valid) {
			t.Fatalf("valid run ID rejected: %q", valid)
		}
	}
	for _, invalid := range []string{"", "-leading", "contains/slash", "contains space", strings.Repeat("a", 129)} {
		if validRunID(invalid) {
			t.Fatalf("invalid run ID accepted: %q", invalid)
		}
	}
}

func TestAuthenticationRejectsMissingOrInvalidSignature(t *testing.T) {
	s := &Server{sharedSecret: []byte("0123456789abcdef0123456789abcdef")}
	handler := s.authenticated(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, signature := range []string{"", "00"} {
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/charges", bytes.NewBufferString(`{"intent_id":"x"}`))
		request.Header.Set(signatureHeader, signature)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("signature=%q status=%d", signature, response.Code)
		}
	}
}

func TestCompletionPageContainsNoPaymentFields(t *testing.T) {
	response := httptest.NewRecorder()
	writeCompletionPage(response, true)
	body := response.Body.String()
	if !strings.Contains(body, "Payment method connected") || strings.Contains(strings.ToLower(body), "card number") || strings.Contains(strings.ToLower(body), "cvv") {
		t.Fatalf("unsafe completion page: %s", body)
	}
}
