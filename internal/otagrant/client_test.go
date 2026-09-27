package otagrant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

func TestHistoricalOTAGrantChecksOriginalEnabledRevisionAndInterval(t *testing.T) {
	cloudID := "11111111-1111-4111-8111-111111111111"
	productID := "22222222-2222-4222-8222-222222222222"
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := from.Add(time.Hour)
	witness := billing.OTAGrantEvidence{ProductServiceRevision: 7,
		ServiceGrantSHA256: strings.Repeat("a", 64), AuthorizedAt: from.Add(time.Minute)}
	grant := historicalGrant{BrandCloudID: cloudID, ProductID: productID, ServiceCode: "ota",
		Enabled: true, ProductServiceRevision: 7, ServiceGrantSHA256: witness.ServiceGrantSHA256,
		ValidFrom: from, ValidUntil: &until}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/internal/brand-clouds/"+cloudID+"/products/"+productID+"/ota-grants/7" ||
			r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			t.Error("historical grant request was not scoped and authenticated")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(grant)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, strings.Repeat("t", 32), nil)
	if err != nil || client.VerifyOTAGrant(context.Background(), cloudID, productID, witness) != nil {
		t.Fatal("valid historical Product grant was rejected", err)
	}
	for name, changed := range map[string]billing.OTAGrantEvidence{
		"before": {ProductServiceRevision: 7, ServiceGrantSHA256: witness.ServiceGrantSHA256, AuthorizedAt: from.Add(-time.Nanosecond)},
		"after":  {ProductServiceRevision: 7, ServiceGrantSHA256: witness.ServiceGrantSHA256, AuthorizedAt: until},
		"digest": {ProductServiceRevision: 7, ServiceGrantSHA256: strings.Repeat("b", 64), AuthorizedAt: witness.AuthorizedAt},
	} {
		t.Run(name, func(t *testing.T) {
			if err := client.VerifyOTAGrant(context.Background(), cloudID, productID, changed); !errors.Is(err, ErrUnverified) {
				t.Fatalf("unqualified historical grant was accepted: %v", err)
			}
		})
	}
	grant.Enabled = false
	if err := client.VerifyOTAGrant(context.Background(), cloudID, productID, witness); !errors.Is(err, ErrUnverified) {
		t.Fatalf("disabled historical revision was accepted: %v", err)
	}
}

func TestHistoricalOTAGrantClientRefusesUnsafeOrUnavailableSource(t *testing.T) {
	for _, origin := range []string{"http://example.com", "https://user@example.com", "http://localhost/?token=x", "http://localhost.evil"} {
		if _, err := NewClient(origin, strings.Repeat("t", 32), nil); !errors.Is(err, ErrUnverified) {
			t.Fatalf("unsafe Account Manager origin %q accepted: %v", origin, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, strings.Repeat("t", 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyOTAGrant(context.Background(), "cloud", "product", billing.OTAGrantEvidence{
		ProductServiceRevision: 1, ServiceGrantSHA256: strings.Repeat("a", 64), AuthorizedAt: time.Now().UTC(),
	}); !errors.Is(err, ErrUnverified) {
		t.Fatalf("unavailable history was accepted: %v", err)
	}
}

func TestCommercialMonthRequiresCompleteHistoricalEvidence(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	cloudID := "11111111-1111-4111-8111-111111111111"
	covered := start.AddDate(0, -1, 0)
	period := billingTierPeriod{BrandCloudID: cloudID, Tier: "commercial", CoveredFrom: &covered,
		HistoryCoversStart: true, CommercialForFullPeriod: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/internal/brand-clouds/"+cloudID+"/billing-tier" ||
			r.URL.Query().Get("period_start") != start.Format(time.RFC3339Nano) ||
			r.URL.Query().Get("period_end") != end.Format(time.RFC3339Nano) ||
			r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			t.Error("tier request was not scoped to the authenticated UTC month")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(period)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, strings.Repeat("t", 32), nil)
	if err != nil || client.VerifyCommercialMonth(context.Background(), cloudID, start, end) != nil {
		t.Fatal("complete commercial month rejected", err)
	}
	for name, changed := range map[string]billingTierPeriod{
		"evaluation":  {BrandCloudID: cloudID, Tier: "evaluation", CoveredFrom: &covered, HistoryCoversStart: true},
		"changed":     {BrandCloudID: cloudID, Tier: "commercial", CoveredFrom: &covered, HistoryCoversStart: true, ChangedWithinPeriod: true},
		"no history":  {BrandCloudID: cloudID, Tier: "commercial", CommercialForFullPeriod: true},
		"wrong cloud": {BrandCloudID: "other", Tier: "commercial", CoveredFrom: &covered, HistoryCoversStart: true, CommercialForFullPeriod: true},
	} {
		t.Run(name, func(t *testing.T) {
			period = changed
			if err := client.VerifyCommercialMonth(context.Background(), cloudID, start, end); !errors.Is(err, ErrUnverified) {
				t.Fatalf("invalid tier history accepted: %v", err)
			}
		})
	}
	if err := client.VerifyCommercialMonth(context.Background(), cloudID, start.Add(time.Hour), end); !errors.Is(err, ErrUnverified) {
		t.Fatalf("partial month accepted: %v", err)
	}
}
