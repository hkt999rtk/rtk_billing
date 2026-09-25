package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
)

type fakeOTAPeriodSealStore struct {
	seals map[string]billingstore.OTAPeriodSeal
}

func (s *fakeOTAPeriodSealStore) PutOTAPeriodSeal(_ context.Context, seal billingstore.OTAPeriodSeal) (billingstore.OTAPeriodSeal, bool, error) {
	if s.seals == nil {
		s.seals = map[string]billingstore.OTAPeriodSeal{}
	}
	old, exists := s.seals[seal.IssuerKind]
	if exists {
		if old.SealID != seal.SealID || old.SourceSHA256 != seal.SourceSHA256 {
			return billingstore.OTAPeriodSeal{}, false, billingstore.ErrConflict
		}
		return old, false, nil
	}
	s.seals[seal.IssuerKind] = seal
	return seal, true, nil
}

func TestOTAPeriodSealRouteRequiresIssuerCredential(t *testing.T) {
	platformToken := strings.Repeat("p", 32)
	producerToken := strings.Repeat("o", 32)
	server, err := New(Options{ServiceToken: strings.Repeat("s", 32), InternalToken: strings.Repeat("i", 32), Audit: testAudit{}, Access: &testAccess{}, Ownership: testOwnership{}})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeOTAPeriodSealStore{}
	if err := server.ConfigureOTAPeriodSeals(OTAPeriodSealAPIOptions{PlatformToken: platformToken, ProducerToken: producerToken, Store: store}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seal := billingstore.OTAPeriodSeal{OrganizationID: "11111111-1111-4111-8111-111111111111", PeriodStart: start,
		PeriodEnd: start.AddDate(0, 1, 0), IssuerKind: billingstore.OTAIssuerPlatformGrants,
		SealID: "22222222-2222-4222-8222-222222222222", SourceHighWater: json.RawMessage(`{"revision":1}`),
		ProductIDs: []string{}, MetricCounts: map[string]int64{}, SourceSHA256: strings.Repeat("a", 64), SealedAt: start.AddDate(0, 1, 0)}
	request := func(token string, input billingstore.OTAPeriodSeal) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/internal/billing/ota-period-seals", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, req)
		return response
	}
	if result := request(strings.Repeat("i", 32), seal); result.Code != http.StatusUnauthorized {
		t.Fatalf("general internal token crossed seal boundary: %d", result.Code)
	}
	if result := request(producerToken, seal); result.Code != http.StatusBadRequest {
		t.Fatalf("producer impersonated Platform issuer: %d", result.Code)
	}
	if result := request(platformToken, seal); result.Code != http.StatusCreated {
		t.Fatalf("new seal status=%d body=%s", result.Code, result.Body.String())
	}
	if result := request(platformToken, seal); result.Code != http.StatusOK {
		t.Fatalf("exact replay status=%d body=%s", result.Code, result.Body.String())
	}
	changed := seal
	digest := sha256.Sum256([]byte("changed"))
	changed.SourceSHA256 = hex.EncodeToString(digest[:])
	if result := request(platformToken, changed); result.Code != http.StatusConflict {
		t.Fatalf("changed replay status=%d body=%s", result.Code, result.Body.String())
	}
}
