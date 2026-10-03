package rawretention

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEvidenceEndpointAndRegistryRejectUnsafeOrigins(t *testing.T) {
	for _, origin := range []string{"https://logger.example", "http://localhost:8081", "http://127.0.0.1:8081", "http://[::1]:8081", "http://logger.ns.svc.cluster.local"} {
		if _, err := endpoint(origin, "/proof"); err != nil {
			t.Fatalf("qualified private origin rejected: %s: %v", origin, err)
		}
	}
	for _, origin := range []string{"http://logger.example", "https://user:password@logger.example", "https://logger.example/path", "https://logger.example?", "https://logger.example?token=x", "https://logger.example#fragment", "https://logger.example/%70ath", "//logger.example", "://bad"} {
		if _, err := endpoint(origin, "/proof"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe origin accepted: %s: %v", origin, err)
		}
	}
	valid := func() *EvidenceClient {
		return &EvidenceClient{Consumers: map[string]Consumer{"consumer": {ID: "consumer", BaseURL: "https://consumer.example", Token: strings.Repeat("v", 32)}}, LoggerBaseURL: "https://logger.example", LoggerToken: strings.Repeat("l", 32)}
	}
	for _, mutate := range []func(*EvidenceClient){
		func(c *EvidenceClient) { c.Consumers = nil },
		func(c *EvidenceClient) { c.LoggerToken = "short" },
		func(c *EvidenceClient) { c.LoggerBaseURL = "http://external.example" },
		func(c *EvidenceClient) {
			c.Consumers["consumer"] = Consumer{"other", "https://consumer.example", strings.Repeat("v", 32)}
		},
		func(c *EvidenceClient) {
			c.Consumers["consumer"] = Consumer{"consumer", "https://consumer.example", "short"}
		},
		func(c *EvidenceClient) {
			c.Consumers["consumer"] = Consumer{"consumer", "http://external.example", strings.Repeat("v", 32)}
		},
	} {
		client := valid()
		mutate(client)
		if err := client.Validate(); err == nil {
			t.Fatal("unsafe registry accepted")
		}
	}
	if err := (*EvidenceClient)(nil).Validate(); err == nil {
		t.Fatal("nil registry accepted")
	}
}

func TestAuthoritativeHTTPRefusesRedirectCacheAndAmbiguousJSON(t *testing.T) {
	for _, tc := range []struct {
		name, body, cache string
		status            int
	}{
		{"redirect", `{}`, "no-store", http.StatusTemporaryRedirect},
		{"cached", `{}`, "public", http.StatusOK},
		{"unknown-field", `{"forged":true}`, "no-store", http.StatusOK},
		{"trailing-json", `{} {}`, "no-store", http.StatusOK},
		{"invalid-json", `{`, "no-store", http.StatusOK},
		{"oversized", strings.Repeat(" ", 128<<10+1), "no-store", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("c", 32) || r.Header.Get("Content-Type") != "application/json" {
					t.Error("dedicated request credentials or body type missing")
				}
				w.Header().Set("Cache-Control", tc.cache)
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := &EvidenceClient{HTTP: &http.Client{}}
			var result struct{}
			if err := client.request(context.Background(), http.MethodPost, server.URL, strings.Repeat("c", 32), struct{}{}, &result); err == nil {
				t.Fatal("non-authoritative or ambiguous response accepted")
			}
		})
	}
	client := &EvidenceClient{}
	if err := client.request(context.Background(), http.MethodPost, "https://logger.example", "token", make(chan int), new(any)); !errors.Is(err, ErrInvalid) {
		t.Fatal("unserializable evidence command accepted", err)
	}
	if err := client.request(context.Background(), http.MethodGet, "://bad", "token", nil, new(any)); err == nil {
		t.Fatal("invalid transport URL accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.request(ctx, http.MethodGet, "https://logger.example", "token", nil, new(any)); err == nil {
		t.Fatal("cancelled evidence transport accepted")
	}
}

func TestDirectEvidenceBindsConsumerAndLoggerOutcome(t *testing.T) {
	_, plan, _, _, _, _ := evidenceFixture(t)
	request := plan.reconcileRequest()
	proof := ConsumerProof{ConsumerID: "consumer", Environment: plan.Environment, StoreID: plan.StoreID, FromSequence: plan.FromSequence, ThroughSequence: plan.ThroughSequence, ArchiveManifestSHA256: plan.ArchiveManifestSHA256, RequestSHA256: digest(request), RecordCount: len(plan.Records), FactCount: len(plan.Records), LastSequence: plan.ThroughSequence, HighWater: plan.ThroughSequence, VerifiedAt: time.Now().UTC()}
	proof.ProofSHA256 = digest(proof)
	op := Operation{Plan: plan}
	terminal := TerminalReceipt{OperationID: plan.OperationID, Scope: plan.Scope, FromSequence: plan.FromSequence, ThroughSequence: plan.ThroughSequence, PlanSHA256: plan.PlanSHA256, Status: "completed", CompletedAt: time.Now().UTC(), RetiredThrough: plan.ThroughSequence, SetID: plan.SetID}
	terminal.ReceiptSHA256 = digest(terminal)
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(proof)
		} else {
			_ = json.NewEncoder(w).Encode(terminal)
		}
	}))
	defer server.Close()
	client := &EvidenceClient{Consumers: map[string]Consumer{"consumer": {"consumer", server.URL, strings.Repeat("c", 32)}}, LoggerBaseURL: server.URL, LoggerToken: strings.Repeat("l", 32)}
	if _, err := client.Proofs(context.Background(), []string{"missing"}, request); !errors.Is(err, ErrBlocked) {
		t.Fatal("unknown required consumer accepted", err)
	}
	if _, err := client.Proofs(context.Background(), []string{"consumer"}, request); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	proof.RequestSHA256 = strings.Repeat("0", 64)
	mu.Unlock()
	if _, err := client.Proofs(context.Background(), []string{"consumer"}, request); !errors.Is(err, ErrBlocked) {
		t.Fatal("foreign consumer bindings accepted", err)
	}
	if _, err := client.Terminal(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	terminal.ReceiptSHA256 = strings.Repeat("0", 64)
	mu.Unlock()
	if _, err := client.Terminal(context.Background(), op); !errors.Is(err, ErrBlocked) {
		t.Fatal("unsealed Logger outcome accepted", err)
	}
	mu.Lock()
	terminal.Status = "pending"
	mu.Unlock()
	if _, err := client.Terminal(context.Background(), op); !errors.Is(err, ErrBlocked) {
		t.Fatal("pending Logger outcome released a fence", err)
	}
}
