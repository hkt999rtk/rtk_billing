package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
)

type fakeLoggerSealStore struct {
	seal       *billingstore.LoggerPeriodSeal
	incomplete bool
}

func (s *fakeLoggerSealStore) PutLoggerPeriodSeal(_ context.Context, seal billingstore.LoggerPeriodSeal) (billingstore.LoggerPeriodSeal, bool, error) {
	if s.incomplete {
		return billingstore.LoggerPeriodSeal{}, false, billingstore.ErrIncomplete
	}
	if s.seal != nil {
		if s.seal.SourceSHA256 != seal.SourceSHA256 {
			return billingstore.LoggerPeriodSeal{}, false, billingstore.ErrConflict
		}
		return *s.seal, false, nil
	}
	s.seal = &seal
	return seal, true, nil
}

func TestLoggerPeriodSealRouteRequiresDedicatedProducerAndExactReplay(t *testing.T) {
	server, err := New(Options{ServiceToken: strings.Repeat("s", 32), InternalToken: strings.Repeat("i", 32), Audit: testAudit{}, Access: &testAccess{}, Ownership: testOwnership{}})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeLoggerSealStore{}
	if err := server.ConfigureLoggerPeriodSeals(LoggerPeriodSealAPIOptions{ProducerToken: strings.Repeat("i", 32), Store: store}); err == nil {
		t.Fatal("internal credential enabled seals")
	}
	if err := server.ConfigureOTAPeriodSeals(OTAPeriodSealAPIOptions{PlatformToken: strings.Repeat("p", 32), ProducerToken: strings.Repeat("o", 32), Store: &fakeOTAPeriodSealStore{}}); err != nil {
		t.Fatal(err)
	}
	if err := server.ConfigureLoggerPeriodSeals(LoggerPeriodSealAPIOptions{ProducerToken: strings.Repeat("o", 32), Store: store}); err == nil {
		t.Fatal("OTA credential enabled Logger seals")
	}
	token := strings.Repeat("l", 32)
	if err := server.ConfigureLoggerPeriodSeals(LoggerPeriodSealAPIOptions{ProducerToken: token, Store: store}); err != nil {
		t.Fatal(err)
	}
	seal := billingstore.LoggerPeriodSeal{IssuerKind: billingstore.LoggerIssuerProducer, SourceSHA256: strings.Repeat("a", 64)}
	request := func(credential string, input billingstore.LoggerPeriodSeal, extra string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(input)
		raw = append(raw, []byte(extra)...)
		req := httptest.NewRequest(http.MethodPost, "/v1/internal/billing/logger-period-seals", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, req)
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("seal response cacheable")
		}
		return response
	}
	for _, other := range []string{strings.Repeat("i", 32), strings.Repeat("s", 32), strings.Repeat("p", 32), strings.Repeat("o", 32)} {
		if result := request(other, seal, ""); result.Code != http.StatusUnauthorized {
			t.Fatal("wrong credential crossed boundary", result.Code)
		}
	}
	wrongIssuer := seal
	wrongIssuer.IssuerKind = billingstore.OTAIssuerProducer
	if result := request(token, wrongIssuer, ""); result.Code != http.StatusBadRequest {
		t.Fatal("wrong issuer", result.Code)
	}
	if result := request(token, seal, "{}"); result.Code != http.StatusBadRequest {
		t.Fatal("trailing JSON accepted", result.Code)
	}
	store.incomplete = true
	if result := request(token, seal, ""); result.Code != http.StatusServiceUnavailable {
		t.Fatal("incomplete factset accepted", result.Code)
	}
	store.incomplete = false
	for _, want := range []int{http.StatusCreated, http.StatusOK} {
		result := request(token, seal, "")
		if result.Code != want {
			t.Fatal("seal status", result.Code, result.Body.String())
		}
		var body struct {
			Seal      billingstore.LoggerPeriodSeal `json:"logger_period_seal"`
			Duplicate bool                          `json:"duplicate"`
		}
		if err := json.Unmarshal(result.Body.Bytes(), &body); err != nil || body.Seal.SourceSHA256 != seal.SourceSHA256 || body.Duplicate != (want == http.StatusOK) {
			t.Fatal("seal acknowledgement mismatch", body, err)
		}
	}
	seal.SourceSHA256 = strings.Repeat("b", 64)
	if result := request(token, seal, ""); result.Code != http.StatusConflict {
		t.Fatal("changed seal accepted", result.Code)
	}
}
