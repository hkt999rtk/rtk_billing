package usagecheckpoint

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testScope() Scope {
	return Scope{CloudID: "11111111-1111-4111-8111-111111111111", OwnerUserID: "22222222-2222-4222-8222-222222222222", OwnershipVersion: 3, CoveredThrough: time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)}
}

func TestClientValidatesBoundCheckpoint(t *testing.T) {
	scope := testScope()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/internal/mqtt-usage-settlement/checkpoint" || request.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 32) {
			t.Fatalf("request path/auth = %s/%q", request.URL.Path, request.Header.Get("Authorization"))
		}
		var got Scope
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil || got != scope {
			t.Fatalf("scope = %+v, %v", got, err)
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		evidence := Evidence{Scope: got, Complete: true, ReceiptID: "33333333-3333-4333-8333-333333333333", AppliedEvents: 2, LegacyWindows: 1,
			SourceCheckpointSHA256: strings.Repeat("a", 64), ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
		evidence.CheckpointSHA256 = evidence.digest()
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(evidence)
	}))
	defer server.Close()
	client, err := New(server.URL, strings.Repeat("s", 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := client.Checkpoint(context.Background(), scope)
	if err != nil || evidence.AppliedEvents != 2 || evidence.Scope != scope {
		t.Fatalf("checkpoint = %+v, %v", evidence, err)
	}
}

func TestClientCanonicalizesEquivalentUTCZone(t *testing.T) {
	scope := testScope()
	scope.CoveredThrough = time.Date(2026, time.September, 2, 4, 40, 0, 123456000, time.FixedZone("database-utc", 0))
	canonical := scope
	canonical.CoveredThrough = scope.CoveredThrough.UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var got Scope
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil || got != canonical {
			t.Fatalf("scope = %+v, %v", got, err)
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		evidence := Evidence{Scope: got, Complete: true, ReceiptID: "33333333-3333-4333-8333-333333333333",
			SourceCheckpointSHA256: strings.Repeat("a", 64), ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
		evidence.CheckpointSHA256 = evidence.digest()
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(evidence)
	}))
	defer server.Close()
	client, err := New(server.URL, strings.Repeat("s", 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := client.Checkpoint(context.Background(), scope)
	if err != nil || evidence.Scope != canonical {
		t.Fatalf("checkpoint = %+v, %v", evidence, err)
	}
}

func TestClientAllowsBoundedProducerClockSkew(t *testing.T) {
	scope := testScope()
	for name, skew := range map[string]time.Duration{
		"within bound": maximumClockSkew - time.Second,
		"past bound":   maximumClockSkew + time.Second,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				now := time.Now().UTC().Add(skew).Truncate(time.Microsecond)
				evidence := Evidence{Scope: scope, Complete: true, ReceiptID: "33333333-3333-4333-8333-333333333333",
					SourceCheckpointSHA256: strings.Repeat("a", 64), ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
				evidence.CheckpointSHA256 = evidence.digest()
				w.Header().Set("Cache-Control", "no-store")
				_ = json.NewEncoder(w).Encode(evidence)
			}))
			defer server.Close()
			client, err := New(server.URL, strings.Repeat("s", 32), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Checkpoint(context.Background(), scope)
			if (err == nil) != (skew < maximumClockSkew) {
				t.Fatalf("skew %s checkpoint error = %v", skew, err)
			}
		})
	}
}

func TestClientFailsClosedOnTransportOrEvidenceMismatch(t *testing.T) {
	scope := testScope()
	for name, mutate := range map[string]func(http.ResponseWriter, *Evidence){
		"cacheable": func(w http.ResponseWriter, _ *Evidence) { w.Header().Del("Cache-Control") },
		"wrong cloud": func(_ http.ResponseWriter, evidence *Evidence) {
			evidence.CloudID = "44444444-4444-4444-8444-444444444444"
			evidence.CheckpointSHA256 = evidence.digest()
		},
		"wrong digest": func(_ http.ResponseWriter, evidence *Evidence) { evidence.CheckpointSHA256 = strings.Repeat("f", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				now := time.Now().UTC().Truncate(time.Microsecond)
				evidence := Evidence{Scope: scope, Complete: true, ReceiptID: "33333333-3333-4333-8333-333333333333", SourceCheckpointSHA256: strings.Repeat("a", 64), ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
				evidence.CheckpointSHA256 = evidence.digest()
				w.Header().Set("Cache-Control", "no-store")
				mutate(w, &evidence)
				_ = json.NewEncoder(w).Encode(evidence)
			}))
			defer server.Close()
			client, err := New(server.URL, strings.Repeat("s", 32), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Checkpoint(context.Background(), scope); err == nil {
				t.Fatal("invalid checkpoint accepted")
			}
		})
	}
	for _, rawURL := range []string{"http://example.test", "ftp://localhost", "https://user@example.test"} {
		if _, err := New(rawURL, strings.Repeat("s", 32), nil); err == nil {
			t.Fatalf("untrusted origin accepted: %s", rawURL)
		}
	}
}
