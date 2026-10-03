//go:build billing_logger_integration

package api

// Run in an isolated Go workspace containing this Billing module and the
// sibling Logger module, with TEST_DATABASE_URL pointing to a disposable Pg.
// This is a protocol fixture, not deployment/restore qualification. Only its
// newly created bbolt fixture's receipt clock is aged; no real data is touched.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/rawretention"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
	logger "github.com/hkt999rtk/rtk_cloud_logger"
	"github.com/hkt999rtk/rtk_cloud_logger/billingarchive"
	bolt "go.etcd.io/bbolt"
)

type loggerWireArchive struct{}

func (loggerWireArchive) PutImmutable(_ context.Context, _, file string, size int64, sha string) error {
	b, err := os.ReadFile(file)
	if err != nil || int64(len(b)) != size || billingarchive.Digest(b) != sha {
		return errors.New("fixture archive bytes mismatch")
	}
	return nil
}

func TestRawRetentionActualLoggerPgPrivateWire(t *testing.T) {
	for _, outcome := range []string{"completed", "aborted", "recovery-fenced"} {
		t.Run(outcome, func(t *testing.T) { testRawRetentionActualLoggerPgPrivateWire(t, outcome) })
	}
}

func testRawRetentionActualLoggerPgPrivateWire(t *testing.T, outcome string) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err = database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `TRUNCATE billing_raw_retention_audit,billing_raw_retention_holds,billing_raw_retention_operations,billing_raw_retention_clearances,billing_raw_retention_policies`); err != nil {
		t.Fatal(err)
	}
	publicKey, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	inboxPath := filepath.Join(dir, "inbox.db")
	inbox, err := logger.OpenBillingInbox(inboxPath, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inbox.Close() })
	if _, err = inbox.Migrate(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		id := "wire-usage-" + strconv.Itoa(i)
		at := time.Now().UTC().Add(-92 * 24 * time.Hour)
		event := logger.LogEvent{EventID: id, Time: at, Level: "info", Message: "billing usage", Service: "video-cloud", Env: "staging", Version: "wire-test", Host: "fixture", Unit: "mqttusage", Source: "billing_usage", Stream: "billing_usage",
			Fields: map[string]any{"usage_event": map[string]any{"usage_id": id, "service_code": "mqtt", "brand_cloud_id": "wire-brand", "event_time": at, "window_start": at.Add(-time.Minute), "window_end": at, "meter_epoch": "wire-epoch", "sequence": json.Number(strconv.Itoa(i)), "source": "meter", "measurements": []any{map[string]any{"metric_code": "publish_bytes", "unit": "bytes", "quantity": json.Number("9007199254740993")}}}}}
		if err = inbox.InsertEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err = inbox.Close(); err != nil {
		t.Fatal(err)
	}
	fixtureDB, err := bolt.Open(inboxPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-91 * 24 * time.Hour)
	err = fixtureDB.Update(func(tx *bolt.Tx) error {
		for seq := uint64(1); seq <= 2; seq++ {
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, seq)
			if err := tx.Bucket([]byte("receipt_times")).Put(key, []byte(old.Format(time.RFC3339Nano))); err != nil {
				return err
			}
		}
		return nil
	})
	if closeErr := fixtureDB.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	inbox, err = logger.OpenBillingInbox(inboxPath, false)
	if err != nil {
		t.Fatal(err)
	}
	apiServer, _, options := rawAPIFixture(t)
	private := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { apiServer.RawRetentionRouter().ServeHTTP(w, r) }))
	t.Cleanup(private.Close)
	// Logger's normal authenticated HTTP client uses the default transport. Trust
	// only this generated httptest certificate for the duration of this test.
	priorTransport := http.DefaultTransport
	http.DefaultTransport = private.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = priorTransport })
	configuration := logger.LifecycleConfig{Environment: "staging", Stack: "rtk", ScratchDir: dir, ScratchCapacityBytes: 8 << 30, EncryptionKeyID: "age-key-1", Recipients: []string{identity.Recipient().String()},
		VerifierKeys: map[string]string{"verifier-1": base64.StdEncoding.EncodeToString(publicKey)}, AuthorityURL: private.URL, AuthorityToken: options.AuthorityReadToken, BackupEnabled: true, RetirementEnabled: true, PartBytes: 1 << 20, ObjectStore: loggerWireArchive{}}
	if err = inbox.ConfigureLifecycle(configuration); err != nil {
		t.Fatal(err)
	}
	loggerHTTPConfig := logger.IngestConfig{BillingInbox: inbox, Token: strings.Repeat("t", 32), BillingToken: strings.Repeat("b", 32), LifecycleToken: strings.Repeat("w", 32), LifecycleReadToken: strings.Repeat("l", 32)}
	local := httptest.NewServer(logger.LifecycleHandler(loggerHTTPConfig))
	t.Cleanup(local.Close)
	consumerID := "video-cloud-mqttusage/staging"
	consumer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/internal/billing-raw-lifecycle/reconcile" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("v", 32) {
			w.WriteHeader(401)
			return
		}
		var req rawretention.ReconcileRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		raw, _ := json.Marshal(req)
		proof := rawretention.ConsumerProof{ConsumerID: consumerID, Environment: req.Environment, StoreID: req.StoreID, FromSequence: req.FromSequence, ThroughSequence: req.ThroughSequence, ArchiveManifestSHA256: req.ArchiveManifestSHA256, RequestSHA256: billingarchive.Digest(raw), RecordCount: len(req.Records), FactCount: len(req.Records), LastSequence: req.ThroughSequence, HighWater: req.ThroughSequence, VerifiedAt: time.Now().UTC()}
		raw, _ = json.Marshal(proof)
		proof.ProofSHA256 = billingarchive.Digest(raw)
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(proof)
	}))
	t.Cleanup(consumer.Close)
	authority, err := rawretention.New(db, &rawretention.EvidenceClient{Consumers: map[string]rawretention.Consumer{consumerID: {ID: consumerID, BaseURL: consumer.URL, Token: strings.Repeat("v", 32)}}, LoggerBaseURL: local.URL, LoggerToken: strings.Repeat("l", 32)}, map[string]ed25519.PublicKey{"verifier-1": publicKey}, "staging")
	if err != nil {
		t.Fatal(err)
	}
	options.Store = authority
	if err = apiServer.ConfigureRawRetention(options); err != nil {
		t.Fatal(err)
	}
	request := func(client *http.Client, base, token, method, path string, body, result any, expected int) {
		t.Helper()
		var input []byte
		if body != nil {
			input, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequest(method, base+path, bytes.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != expected {
			t.Fatalf("%s %s: HTTP %d want %d: %s", method, path, res.StatusCode, expected, data)
		}
		if expected == 200 && result != nil && json.Unmarshal(data, result) != nil {
			t.Fatalf("invalid actual response: %s", data)
		}
	}
	capture, err := inbox.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := base64.StdEncoding.DecodeString(capture.ManifestB64)
	if err != nil {
		t.Fatal(err)
	}
	complete, err := billingarchive.SignCompletion(billingarchive.PayloadFor(capture.Manifest, manifestRaw, "verifier-1", "billing-raw-v1", "wire-test", time.Now().UTC()), signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = inbox.VerifyCatalog(ctx, capture.SetID, complete); err != nil {
		t.Fatal(err)
	}
	page, err := inbox.Page(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]rawretention.RecordBinding, 0, 2)
	for _, record := range page.Records {
		usage, _ := json.Marshal(record.Event.Fields["usage_event"])
		ref, err := billingarchive.CanonicalUsageBinding(record.Sequence, record.ContentSHA256, usage)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, rawretention.RecordBinding{Sequence: ref.Sequence, LoggerContentSHA256: ref.LoggerContentSHA256, UsageID: ref.UsageID, EventSHA256: ref.EventSHA256})
	}
	scope := rawretention.Scope{Environment: "staging", StoreID: capture.Manifest.StoreID}
	policy := rawretention.Policy{PolicyID: "wire-policy", Version: 1, Scope: scope, HotRetentionDays: 90, RequiredConsumers: []string{consumerID}, FinancialApprovalRef: "financial-approval"}
	var policyState rawretention.PolicyState
	base := "/v1/internal/billing/raw-retention"
	request(private.Client(), private.URL, options.FinancialToken, http.MethodPost, base+"/policies", policy, &policyState, 200)
	request(private.Client(), private.URL, options.RecoveryToken, http.MethodPost, base+"/policies/wire-policy/1/approve", map[string]string{"policy_sha256": policyState.PolicySHA256, "recovery_approval_ref": "recovery-approved"}, nil, 200)
	request(private.Client(), private.URL, options.FinancialToken, http.MethodPost, base+"/policies/wire-policy/1/activate", map[string]string{"policy_sha256": policyState.PolicySHA256}, nil, 200)
	periodStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	org := testutil.OrganizationID("raw-retention-wire")
	if _, err = db.Exec(ctx, `INSERT INTO billing_periods(organization_id,currency,period_start,period_end,state) VALUES($1,'TWD',$2,$3,'closed') ON CONFLICT(organization_id,currency,period_start,period_end) DO UPDATE SET state='closed'`, org, periodStart, periodEnd); err != nil {
		t.Fatal(err)
	}
	clearance := rawretention.Clearance{ClearanceID: "wire-clearance", PolicyID: policy.PolicyID, PolicyVersion: 1, Scope: scope, FromSequence: 1, ThroughSequence: 2, Periods: []rawretention.Period{{OrganizationID: org, PeriodStart: periodStart, PeriodEnd: periodEnd, SourceCheckpointSHA256: strings.Repeat("a", 64), SourceComplete: true, Reconciled: true}}, FinancialApprovalRef: "clearance-approved", ReconciliationSHA256: strings.Repeat("b", 64)}
	request(private.Client(), private.URL, options.FinancialToken, http.MethodPost, base+"/clearances", clearance, nil, 200)
	bindings, _ := json.Marshal(refs)
	proof, err := billingarchive.SignRangeProof(billingarchive.RangeProof{Version: 1, Purpose: "billing-raw-reconciliation", VerifierKeyID: "verifier-1", Environment: scope.Environment, Stack: capture.Manifest.Stack, StoreID: scope.StoreID, SetID: capture.SetID, ManifestSHA256: billingarchive.Digest(manifestRaw), HighWater: 2, FromSequence: 1, ThroughSequence: 2, RecordCount: 2, MaxReceivedAt: old, RecordsBindingsSHA256: billingarchive.Digest(bindings), VerifiedAt: time.Now().UTC(), PolicyVersion: "1", VerifierVersion: "wire-test"}, signer)
	if err != nil {
		t.Fatal(err)
	}
	plan := rawretention.Plan{OperationID: "wire-operation", PolicyID: policy.PolicyID, PolicyVersion: 1, ClearanceID: clearance.ClearanceID, Scope: scope, FromSequence: 1, ThroughSequence: 2, SetID: capture.SetID, ArchiveManifestSHA256: billingarchive.Digest(manifestRaw), Records: refs,
		ArchiveCompletion: rawretention.SignedCompletion{VerifierKeyID: complete.VerifierKeyID, PayloadB64: complete.PayloadB64, SignatureB64: complete.SignatureB64}, RangeProof: rawretention.SignedCompletion{VerifierKeyID: proof.VerifierKeyID, PayloadB64: proof.PayloadB64, SignatureB64: proof.SignatureB64}}
	plan.PlanSHA256 = rawretention.PlanDigest(plan)
	var operation rawretention.Operation
	request(private.Client(), private.URL, options.ControllerToken, http.MethodPost, base+"/operations", plan, &operation, 200)
	if operation.Status != "ACTIVE" {
		t.Fatal("decision not active", operation.Status)
	}
	// Logger's read credential cannot issue mutation commands.
	request(http.DefaultClient, local.URL, strings.Repeat("l", 32), http.MethodPost, logger.LifecyclePath+"retire/apply", map[string]string{"operation_id": plan.OperationID}, nil, 401)
	loggerPlan := logger.RetirementPlan{OperationID: plan.OperationID, Environment: scope.Environment, StoreID: scope.StoreID, FromSequence: 1, ThroughSequence: 2, PlanSHA256: plan.PlanSHA256, SetID: plan.SetID}
	if outcome != "aborted" {
		request(http.DefaultClient, local.URL, strings.Repeat("w", 32), http.MethodPost, logger.LifecyclePath+"retire/plan", loggerPlan, nil, 200)
		// Actual durable pending JSON must never release the Pg fence.
		request(private.Client(), private.URL, options.ControllerToken, http.MethodPost, base+"/operations/"+plan.OperationID+"/resolve", map[string]string{}, nil, 409)
	} else {
		// The operation was never planned at Logger: a 404 is not an abort proof.
		request(private.Client(), private.URL, options.ControllerToken, http.MethodPost, base+"/operations/"+plan.OperationID+"/resolve", map[string]string{}, nil, 409)
		request(private.Client(), private.URL, options.ControllerToken, http.MethodPost, base+"/operations/"+plan.OperationID+"/abort", map[string]string{}, nil, 200)
	}
	var hold rawretention.HoldState
	request(private.Client(), private.URL, options.FinancialToken, http.MethodPost, base+"/holds", rawretention.Hold{HoldID: "wire-late-hold", Scope: scope, FromSequence: 1, ThroughSequence: 2, Reason: "dispute", FinancialApprovalRef: "case-approved"}, &hold, 200)
	if hold.Status != "PENDING_FENCED" || !hold.ProtectArchiveAndKeys {
		t.Fatal("late hold protection lost", hold)
	}
	if outcome == "recovery-fenced" {
		if err = inbox.SetRecoveryMode(ctx); err != nil {
			t.Fatal(err)
		}
		request(http.DefaultClient, local.URL, strings.Repeat("w", 32), http.MethodPost, logger.LifecyclePath+"retire/apply", map[string]string{"operation_id": plan.OperationID}, nil, 503)
		request(private.Client(), private.URL, options.ControllerToken, http.MethodPost, base+"/operations/"+plan.OperationID+"/resolve", map[string]string{}, nil, 409)
		request(private.Client(), private.URL, options.AuthorityReadToken, http.MethodGet, base+"/operations/"+plan.OperationID, nil, &operation, 200)
		if operation.Status != "ACTIVE" {
			t.Fatal("recovery fence silently unlocked Pg decision")
		}
		if err = inbox.Close(); err != nil {
			t.Fatal(err)
		}
		inbox, err = logger.OpenBillingInbox(inboxPath, false)
		if err != nil {
			t.Fatal(err)
		}
		if err = inbox.ConfigureLifecycle(configuration); err != nil {
			t.Fatal(err)
		}
		recovery, err := inbox.RecoveryState(ctx)
		if err != nil || !recovery.Fenced || recovery.ArchiveFloor != 0 || recovery.HighWater != 2 {
			t.Fatal("startup lost durable recovery fence or receipt horizon", recovery, err)
		}
		if _, err = inbox.ApplyRetirement(ctx, plan.OperationID); !errors.Is(err, logger.ErrBillingUnavailable) {
			t.Fatal("startup recovery fence allowed pending removal", err)
		}
		return
	}
	if outcome == "completed" {
		request(http.DefaultClient, local.URL, strings.Repeat("w", 32), http.MethodPost, logger.LifecyclePath+"retire/apply", map[string]string{"operation_id": plan.OperationID}, nil, 200)
	} else {
		request(http.DefaultClient, local.URL, strings.Repeat("w", 32), http.MethodPost, logger.LifecyclePath+"retire/abort", loggerPlan, nil, 200)
		request(http.DefaultClient, local.URL, strings.Repeat("w", 32), http.MethodPost, logger.LifecyclePath+"retire/apply", map[string]string{"operation_id": plan.OperationID}, nil, 409)
	}
	request(private.Client(), private.URL, options.ControllerToken, http.MethodPost, base+"/operations/"+plan.OperationID+"/resolve", map[string]string{}, &operation, 200)
	if operation.Status != strings.ToUpper(outcome) || operation.TerminalReceipt == nil || operation.TerminalReceipt.SetID != plan.SetID {
		t.Fatal("actual Logger terminal JSON not reconciled", operation)
	}
	request(private.Client(), private.URL, options.FinancialToken, http.MethodGet, base+"/holds/wire-late-hold", nil, &hold, 200)
	if hold.Status != "ACTIVE" || !hold.ProtectArchiveAndKeys {
		t.Fatal("queued hold not finalized", hold)
	}
	// Terminal replay precedes proof freshness and never resurrects an abort.
	plan.RangeProof.SignatureB64 = "expired-or-unavailable"
	request(private.Client(), private.URL, options.ControllerToken, http.MethodPost, base+"/operations", plan, &operation, 200)
	if operation.Status != strings.ToUpper(outcome) {
		t.Fatal("terminal operation replay regressed")
	}
	neverAccepted := plan
	neverAccepted.OperationID = "wire-never-accepted"
	neverAccepted.PlanSHA256 = rawretention.PlanDigest(neverAccepted)
	// Unmarshal into a fresh DTO: terminal_receipt is deliberately omitted for
	// a before-acceptance cancellation, not a replacement for an old pointer.
	operation = rawretention.Operation{}
	request(private.Client(), private.URL, options.ControllerToken, http.MethodPost, base+"/operations/"+neverAccepted.OperationID+"/abort", neverAccepted, &operation, 200)
	if operation.Status != "ABORTED" || operation.DecisionOrigin != "cancelled-before-acceptance" || operation.TerminalReceipt != nil || len(operation.ConsumerProofs) != 0 {
		t.Fatalf("unknown cancellation provenance: status=%s origin=%s terminal=%t proofs=%d", operation.Status, operation.DecisionOrigin, operation.TerminalReceipt != nil, len(operation.ConsumerProofs))
	}
	request(http.DefaultClient, local.URL, strings.Repeat("l", 32), http.MethodGet, logger.LifecyclePath+"retire/"+neverAccepted.OperationID, nil, nil, 404)
	request(private.Client(), private.URL, options.ControllerToken, http.MethodPost, base+"/operations", neverAccepted, &operation, 200)
	if operation.Status != "ABORTED" || operation.TerminalReceipt != nil {
		t.Fatal("late admission resurrected unknown cancellation")
	}
	public := httptest.NewServer(apiServer.Router())
	t.Cleanup(public.Close)
	request(http.DefaultClient, public.URL, options.ControllerToken, http.MethodGet, base+"/operations/"+plan.OperationID, nil, nil, 404)
	publicLogger := httptest.NewServer(logger.IngestHandler(logger.NewMemoryEventStore(), loggerHTTPConfig))
	t.Cleanup(publicLogger.Close)
	request(http.DefaultClient, publicLogger.URL, strings.Repeat("w", 32), http.MethodPost, logger.LifecyclePath+"retire/apply", map[string]string{"operation_id": plan.OperationID}, nil, 404)
}
