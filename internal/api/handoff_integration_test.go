package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

const testHandoffToken = "billing-handoff-coordinator-test-only-0001"

type handoffHTTPFixture struct {
	env            integrationAPI
	store          *paymentstore.Store
	server         *httptest.Server
	scope          paymentstore.HandoffScope
	source, target string
	cutoff         time.Time
}

func newHandoffHTTPFixture(t *testing.T, balance int64) handoffHTTPFixture {
	t.Helper()
	env := newIntegrationAPI(t)
	store := paymentstore.New(env.db)
	if err := env.server.ConfigureHandoff(HandoffAPIOptions{Token: testHandoffToken, Store: store}); err != nil {
		t.Fatal(err)
	}
	f := handoffHTTPFixture{env: env, store: store, source: testutil.OrganizationID("http-source"), target: testutil.OrganizationID("http-target"), cutoff: time.Now().UTC().Truncate(time.Microsecond)}
	f.scope = paymentstore.HandoffScope{OrganizationID: testutil.OrganizationID(t.Name()), OperationID: testutil.OrganizationID(t.Name() + "operation"), OwnershipVersion: 1}
	ctx := context.Background()
	account, _, err := store.EnsureCommercialAccount(ctx, f.scope.OrganizationID, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 0 {
		direction, reason, amount := payment.LedgerDirectionCredit, payment.LedgerReasonManualAdjustmentCredit, balance
		if balance < 0 {
			direction, reason, amount = payment.LedgerDirectionDebit, payment.LedgerReasonManualAdjustmentDebit, -balance
		}
		if _, err := store.PostLedgerEntry(ctx, paymentstore.PostLedgerEntryInput{AccountID: account.ID, Direction: direction, Reason: reason, AmountMinor: amount, Currency: payment.CurrencyTWD,
			IdempotencyScope: "test", IdempotencyKey: "opening", ActorType: "service", ActorID: "test", RequestID: "opening"}); err != nil {
			t.Fatal(err)
		}
	}
	// Reviewed responsibility and collector evidence are synthetic fixtures,
	// not proof of a real Account Manager commit or provider reconciliation.
	if err := store.InitializeResponsibility(ctx, paymentstore.InitialResponsibilityInput{AccountID: account.ID, OwnerUserID: f.source, OwnershipVersion: 1,
		EffectiveFrom: f.cutoff.Add(-time.Hour), SourceEvidenceSHA256: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(env.server.Router())
	t.Cleanup(f.server.Close)
	return f
}

func (f handoffHTTPFixture) request(t *testing.T, method, suffix, token, version string, payload any, want int) []byte {
	t.Helper()
	var body []byte
	if raw, ok := payload.(string); ok {
		body = []byte(raw)
	} else if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	url := f.server.URL + "/v1/internal/billing/clouds/" + f.scope.OrganizationID + "/ownership-handoffs/" + f.scope.OperationID + suffix
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Billing-Ownership-Version", version)
	req.Header.Set("Content-Type", "application/json")
	res, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, suffix, res.StatusCode, want, raw)
	}
	if want != http.StatusNotFound && res.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("handoff response can be cached")
	}
	if want == http.StatusOK {
		var scope struct {
			CloudID          string `json:"cloud_id"`
			OperationID      string `json:"operation_id"`
			OwnershipVersion int64  `json:"ownership_version"`
		}
		if err := json.Unmarshal(raw, &scope); err != nil || scope.CloudID != f.scope.OrganizationID || scope.OperationID != f.scope.OperationID || scope.OwnershipVersion != f.scope.OwnershipVersion {
			t.Fatalf("response lost scope: %s %v", raw, err)
		}
	}
	return raw
}

func (f handoffHTTPFixture) prepare(t *testing.T) {
	t.Helper()
	body := map[string]any{"source_user_id": f.source, "target_user_id": f.target, "cutoff": f.cutoff}
	for i := 0; i < 2; i++ {
		f.request(t, http.MethodPost, "/prepare", testHandoffToken, "1", body, http.StatusOK)
	}
}

func (f handoffHTTPFixture) settle(t *testing.T, name string) paymentstore.HandoffSettlementStatus {
	t.Helper()
	ctx := context.Background()
	state, err := f.store.CaptureHandoffSettlementState(ctx, f.scope)
	if err != nil {
		t.Fatal(err)
	}
	state.Financial.UsageSettled, state.Financial.InvoicesReconciled, state.Financial.ProviderWorkReconciled = true, true, true
	status, err := f.store.RecordHandoffSettlement(ctx, paymentstore.RecordSettlementInput{Scope: f.scope, ReceiptID: testutil.OrganizationID(name), StateSHA256: state.SHA256, Financial: state.Financial,
		UsageCheckpointSHA256: strings.Repeat("a", 64), InvoiceCheckpointSHA256: strings.Repeat("b", 64), ProviderCheckpointSHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func TestHandoffHTTPBoundaryRejectsOtherCredentialsAndForgedEvidence(t *testing.T) {
	f := newHandoffHTTPFixture(t, 0)
	for _, suffix := range []string{"/prepare", "/confirm", "/authorize-commit", "/finalize", "/abort", "/settlement"} {
		method := http.MethodPost
		if suffix == "/settlement" {
			method = http.MethodGet
		}
		for _, token := range []string{"", integrationServiceToken, integrationInternalToken, integrationDebitToken, "user-jwt"} {
			f.request(t, method, suffix, token, "1", `{}`, http.StatusUnauthorized)
		}
	}
	for _, version := range []string{"", "0", "-1", "01", "+1", "9223372036854775808"} {
		f.request(t, http.MethodPost, "/prepare", testHandoffToken, version, map[string]any{"source_user_id": f.source, "target_user_id": f.target, "cutoff": f.cutoff}, http.StatusBadRequest)
	}
	for _, body := range []string{`null`, `{} {}`, `{"source_confirmed":true}`, `{"financial_evidence":{"UsageSettled":true}}`, strings.Repeat(" ", 16*1024) + `{}`} {
		f.request(t, http.MethodPost, "/prepare", testHandoffToken, "1", body, http.StatusBadRequest)
	}
	for _, suffix := range []string{"/settle", "/initialize", "/complete-abort", "/release", "/collect"} {
		f.request(t, http.MethodPost, suffix, testHandoffToken, "1", `{}`, http.StatusNotFound)
	}
	var count int
	if err := f.env.db.QueryRow(context.Background(), `SELECT count(*) FROM billing_ownership_handoffs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unauthorized handoff write: %d %v", count, err)
	}
	f.prepare(t)
	f.request(t, http.MethodGet, "/settlement", testHandoffToken, "2", nil, http.StatusConflict)
	other := f
	other.scope.OrganizationID = testutil.OrganizationID("another-cloud")
	other.request(t, http.MethodGet, "/settlement", testHandoffToken, "1", nil, http.StatusNotFound)
	for _, path := range []string{"/v1/orgs/" + f.scope.OrganizationID + "/billing/account", "/v1/internal/billing/access/" + f.scope.OrganizationID} {
		req, _ := http.NewRequest(http.MethodGet, f.server.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+testHandoffToken)
		res, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("coordinator acquired other authority: %s %d", path, res.StatusCode)
		}
	}
}

func TestHandoffHTTPUsesStoredSettlementAndBothParticipantConfirmations(t *testing.T) {
	for _, balance := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprint(balance), func(t *testing.T) {
			f := newHandoffHTTPFixture(t, balance)
			f.prepare(t)
			raw := f.request(t, http.MethodGet, "/settlement", testHandoffToken, "1", nil, http.StatusOK)
			if !strings.Contains(string(raw), "settlement_evidence_missing") || strings.Contains(string(raw), `"snapshot":`) {
				t.Fatalf("prepare fabricated settlement: %s", raw)
			}
			status := f.settle(t, "settled")
			grantID := testutil.OrganizationID("http-grant")
			if balance < 0 {
				raw = f.request(t, http.MethodGet, "/settlement", testHandoffToken, "1", nil, http.StatusOK)
				if !strings.Contains(string(raw), "balance_negative") || strings.Contains(string(raw), `"snapshot":`) {
					t.Fatalf("negative became confirmable: %s", raw)
				}
				f.request(t, http.MethodPost, "/authorize-commit", testHandoffToken, "1", map[string]any{"authorization_id": grantID, "snapshot_version": 2}, http.StatusConflict)
				return
			}
			if status.Snapshot == nil {
				t.Fatal("missing fixture snapshot")
			}
			authorize := map[string]any{"authorization_id": grantID, "snapshot_version": status.Snapshot.Version}
			confirmation := map[string]any{"user_id": f.source, "snapshot_version": status.Snapshot.Version, "balance_minor": balance, "currency": "TWD"}
			f.request(t, http.MethodPost, "/authorize-commit", testHandoffToken, "1", authorize, http.StatusConflict)
			confirmation["user_id"] = testutil.OrganizationID("outsider")
			f.request(t, http.MethodPost, "/confirm", testHandoffToken, "1", confirmation, http.StatusForbidden)
			confirmation["user_id"] = f.source
			confirmation["balance_minor"] = balance + 1
			f.request(t, http.MethodPost, "/confirm", testHandoffToken, "1", confirmation, http.StatusConflict)
			delete(confirmation, "balance_minor")
			f.request(t, http.MethodPost, "/confirm", testHandoffToken, "1", confirmation, http.StatusBadRequest)
			confirmation["balance_minor"] = balance
			for i := 0; i < 2; i++ {
				f.request(t, http.MethodPost, "/confirm", testHandoffToken, "1", confirmation, http.StatusOK)
			}
			f.request(t, http.MethodPost, "/authorize-commit", testHandoffToken, "1", authorize, http.StatusConflict)
			confirmation["user_id"] = f.target
			f.request(t, http.MethodPost, "/confirm", testHandoffToken, "1", confirmation, http.StatusOK)
			for i := 0; i < 2; i++ {
				f.request(t, http.MethodPost, "/authorize-commit", testHandoffToken, "1", authorize, http.StatusOK)
			}
			finalize := map[string]any{"authorization_id": grantID, "committed_owner_user_id": f.target, "committed_ownership_version": 2, "committed_at": f.cutoff.Add(time.Second), "am_commit_sha256": strings.Repeat("d", 64)}
			abort := map[string]any{"authorization_id": grantID, "cancellation_id": testutil.OrganizationID("cancel"), "am_cancellation_sha256": strings.Repeat("e", 64)}
			if balance == 0 {
				if _, err := f.env.db.Exec(context.Background(), `ALTER TABLE billing_audit_events ADD CONSTRAINT http_finalize_audit_failure CHECK(event_type<>'billing.ownership_handoff.finalize') NOT VALID`); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_, _ = f.env.db.Exec(context.Background(), `ALTER TABLE billing_audit_events DROP CONSTRAINT IF EXISTS http_finalize_audit_failure`)
				})
				f.request(t, http.MethodPost, "/finalize", testHandoffToken, "1", finalize, http.StatusServiceUnavailable)
				var phase string
				if err := f.env.db.QueryRow(context.Background(), `SELECT phase FROM billing_ownership_handoffs WHERE id=$1`, f.scope.OperationID).Scan(&phase); err != nil || phase != "finalizing" {
					t.Fatalf("HTTP failure lost known AM decision: %s %v", phase, err)
				}
				f.request(t, http.MethodPost, "/abort", testHandoffToken, "1", abort, http.StatusConflict)
				if _, err := f.env.db.Exec(context.Background(), `ALTER TABLE billing_audit_events DROP CONSTRAINT http_finalize_audit_failure`); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				raw = f.request(t, http.MethodPost, "/finalize", testHandoffToken, "1", finalize, http.StatusOK)
				if !strings.Contains(string(raw), `"phase":"finalized"`) {
					t.Fatalf("finalize response: %s", raw)
				}
			}
			f.request(t, http.MethodPost, "/abort", testHandoffToken, "1", abort, http.StatusConflict)
			var owner string
			if err := f.env.db.QueryRow(context.Background(), `SELECT owner_user_id::text FROM billing_responsibility_periods WHERE ownership_version=2 AND effective_until IS NULL`).Scan(&owner); err != nil || owner != f.target {
				t.Fatalf("responsibility not finalized: %s %v", owner, err)
			}
		})
	}
}

func TestHandoffHTTPStaleConfirmationAndCancellationRetainFence(t *testing.T) {
	f := newHandoffHTTPFixture(t, 1)
	f.prepare(t)
	status := f.settle(t, "before-change")
	confirmation := map[string]any{"user_id": f.source, "snapshot_version": status.Snapshot.Version, "balance_minor": 1, "currency": "TWD"}
	f.request(t, http.MethodPost, "/confirm", testHandoffToken, "1", confirmation, http.StatusOK)
	// Synthetic producer drift changes the captured local digest. It is not
	// permission for a tenant or coordinator to inject settlement evidence.
	if _, err := f.env.db.Exec(context.Background(), `INSERT INTO billing_usage_facts(usage_id,organization_id,service_code,metric_code,quantity,unit,window_start,window_end,source,source_sha256)
		VALUES('handoff-drift',$1,'test','test',1,'unit',$2::timestamptz-interval '1 hour',$2,'test',repeat('a',64))`, f.scope.OrganizationID, f.cutoff); err != nil {
		t.Fatal(err)
	}
	f.request(t, http.MethodPost, "/confirm", testHandoffToken, "1", confirmation, http.StatusConflict)
	abort := map[string]any{"cancellation_id": testutil.OrganizationID("http-cancel"), "am_cancellation_sha256": strings.Repeat("e", 64)}
	for i := 0; i < 2; i++ {
		raw := f.request(t, http.MethodPost, "/abort", testHandoffToken, "1", abort, http.StatusOK)
		if !strings.Contains(string(raw), `"phase":"abort_pending"`) {
			t.Fatalf("abort released prematurely: %s", raw)
		}
	}
	var phase string
	if err := f.env.db.QueryRow(context.Background(), `SELECT phase FROM billing_ownership_handoffs WHERE id=$1`, f.scope.OperationID).Scan(&phase); err != nil || phase != "abort_pending" {
		t.Fatalf("abort phase=%s %v", phase, err)
	}
}
