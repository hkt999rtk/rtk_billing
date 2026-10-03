package rawretention

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

type authorityFixture struct {
	store         *Store
	db            *pgxpool.Pool
	policy        Policy
	plan          Plan
	clearance     Clearance
	mu            sync.Mutex
	terminal      *TerminalReceipt
	failConsumer  bool
	consumerCalls int
}

func newAuthorityFixture(t *testing.T) *authorityFixture {
	t.Helper()
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
	policy, plan, _, _, _, keys := evidenceFixture(t)
	f := &authorityFixture{db: db, policy: policy, plan: plan}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/internal/billing-raw-lifecycle/reconcile" {
			if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("v", 32) {
				w.WriteHeader(401)
				return
			}
			f.mu.Lock()
			f.consumerCalls++
			failed := f.failConsumer
			f.mu.Unlock()
			if failed {
				w.WriteHeader(503)
				return
			}
			var request ReconcileRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				w.WriteHeader(400)
				return
			}
			p := ConsumerProof{ConsumerID: policy.RequiredConsumers[0], Environment: request.Environment, StoreID: request.StoreID, FromSequence: request.FromSequence, ThroughSequence: request.ThroughSequence, ArchiveManifestSHA256: request.ArchiveManifestSHA256, RequestSHA256: digest(request), RecordCount: len(request.Records), FactCount: 2 * len(request.Records), LastSequence: request.ThroughSequence, HighWater: request.ThroughSequence, VerifiedAt: time.Now().UTC()}
			p.ProofSHA256 = digest(p)
			_ = json.NewEncoder(w).Encode(p)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/internal/billing-lifecycle/retire/") {
			if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("l", 32) {
				w.WriteHeader(401)
				return
			}
			f.mu.Lock()
			receipt := f.terminal
			f.mu.Unlock()
			if receipt == nil {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(receipt)
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(server.Close)
	client := &EvidenceClient{Consumers: map[string]Consumer{policy.RequiredConsumers[0]: {ID: policy.RequiredConsumers[0], BaseURL: server.URL, Token: strings.Repeat("v", 32)}}, LoggerBaseURL: server.URL, LoggerToken: strings.Repeat("l", 32)}
	f.store, err = New(db, client, keys, policy.Environment)
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.store.PutPolicy(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.SetActive(ctx, policy.PolicyID, policy.Version, state.PolicySHA256, true); !errors.Is(err, ErrBlocked) {
		t.Fatalf("activation without recovery approval: %v", err)
	}
	if _, err = f.store.ApproveRecovery(ctx, policy.PolicyID, policy.Version, state.PolicySHA256, "recovery-approval"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.SetActive(ctx, policy.PolicyID, policy.Version, state.PolicySHA256, true); err != nil {
		t.Fatal(err)
	}
	periodStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	org := testutil.OrganizationID("raw-retention-financial-clearance")
	if _, err = db.Exec(ctx, `INSERT INTO billing_periods(organization_id,currency,period_start,period_end,state) VALUES($1,'TWD',$2,$3,'closed') ON CONFLICT(organization_id,currency,period_start,period_end) DO UPDATE SET state='closed'`, org, periodStart, periodEnd); err != nil {
		t.Fatal(err)
	}
	f.clearance = Clearance{ClearanceID: plan.ClearanceID, PolicyID: policy.PolicyID, PolicyVersion: 1, Scope: policy.Scope, FromSequence: 1, ThroughSequence: 2, Periods: []Period{{org, periodStart, periodEnd, strings.Repeat("3", 64), true, true}}, FinancialApprovalRef: "reconciled-period-approval", ReconciliationSHA256: strings.Repeat("4", 64)}
	return f
}

func (f *authorityFixture) clear(t *testing.T) {
	t.Helper()
	if _, err := f.store.PutClearance(context.Background(), f.clearance); err != nil {
		t.Fatal(err)
	}
}
func (f *authorityFixture) setTerminal(op Operation, status string) {
	r := TerminalReceipt{OperationID: op.OperationID, Scope: op.Scope, FromSequence: op.FromSequence, ThroughSequence: op.ThroughSequence, PlanSHA256: op.PlanSHA256, Status: status, CompletedAt: time.Now().UTC(), SetID: op.SetID}
	if status == "completed" {
		r.RetiredThrough = op.ThroughSequence
	}
	r.ReceiptSHA256 = digest(r)
	f.mu.Lock()
	f.terminal = &r
	f.mu.Unlock()
}

func TestAuthorityRequiresExplicitClearanceAndAuthenticatedConsumer(t *testing.T) {
	f := newAuthorityFixture(t)
	ctx := context.Background()
	if _, err := f.store.RequestOperation(ctx, f.plan); !errors.Is(err, ErrNotFound) {
		t.Fatalf("closed period without explicit source clearance accepted: %v", err)
	}
	c := f.clearance
	c.Periods = append([]Period(nil), c.Periods...)
	c.Periods[0].SourceComplete = false
	if _, err := f.store.PutClearance(ctx, c); !errors.Is(err, ErrInvalid) {
		t.Fatalf("source-incomplete clearance accepted: %v", err)
	}
	f.clear(t)
	f.mu.Lock()
	f.failConsumer = true
	f.mu.Unlock()
	if _, err := f.store.RequestOperation(ctx, f.plan); err == nil {
		t.Fatal("unavailable authenticated consumer accepted")
	}
	f.mu.Lock()
	f.failConsumer = false
	f.mu.Unlock()
	op, err := f.store.RequestOperation(ctx, f.plan)
	if err != nil || op.Status != "ACTIVE" || len(op.ConsumerProofs) != 1 {
		t.Fatalf("active decision %+v %v", op, err)
	}
	if _, err = f.db.Exec(ctx, `UPDATE billing_raw_retention_operations SET plan_sha256=$2 WHERE operation_id=$1`, op.OperationID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("immutable operation binding changed")
	}
	if err = f.store.RevokeClearance(ctx, f.clearance.ClearanceID); err != nil {
		t.Fatal(err)
	}
	if prior, err := f.store.RequestOperation(ctx, f.plan); err != nil || prior.Status != "ACTIVE" {
		t.Fatalf("revocation incorrectly canceled irrevocable decision: %v", err)
	}
}

func TestAuthorityHoldsAndExactResolutionFence(t *testing.T) {
	f := newAuthorityFixture(t)
	f.clear(t)
	ctx := context.Background()
	hold := Hold{HoldID: "hold-before", Scope: f.policy.Scope, FromSequence: 1, ThroughSequence: 2, Reason: "dispute", FinancialApprovalRef: "case-approved"}
	if h, err := f.store.PutHold(ctx, hold); err != nil || h.Status != "ACTIVE" {
		t.Fatalf("hold %+v %v", h, err)
	}
	if _, err := f.store.RequestOperation(ctx, f.plan); !errors.Is(err, ErrBlocked) {
		t.Fatalf("hold did not block: %v", err)
	}
	if _, err := f.store.ReleaseHold(ctx, hold.HoldID); err != nil {
		t.Fatal(err)
	}
	op, err := f.store.RequestOperation(ctx, f.plan)
	if err != nil {
		t.Fatal(err)
	}
	hold.HoldID = "hold-after"
	h, err := f.store.PutHold(ctx, hold)
	if err != nil || h.Status != "PENDING_FENCED" || !h.ProtectArchiveAndKeys {
		t.Fatalf("late hold %+v %v", h, err)
	}
	if _, err = f.store.Resolve(ctx, op.OperationID); err == nil {
		t.Fatal("unknown Logger outcome released fence")
	}
	if pending, err := f.store.RequestAbort(ctx, op.OperationID); err != nil || pending.Status != "ABORT_REQUESTED" {
		t.Fatalf("abort prematurely released fence: %v", err)
	}
	active, err := f.store.Operations(ctx, f.policy.Scope)
	if err != nil || len(active) != 1 {
		t.Fatal("ambiguous timeout lost durable fence")
	}
	f.setTerminal(op, "completed")
	resolved, err := f.store.Resolve(ctx, op.OperationID)
	if err != nil || resolved.Status != "COMPLETED" {
		t.Fatalf("exact resolution %+v %v", resolved, err)
	}
	h, err = f.store.Hold(ctx, hold.HoldID)
	if err != nil || h.Status != "ACTIVE" || !h.ProtectArchiveAndKeys {
		t.Fatalf("queued hold not finalized %+v %v", h, err)
	}
	if late, err := f.store.RequestAbort(ctx, op.OperationID); err != nil || late.Status != "COMPLETED" {
		t.Fatal("late abort changed terminal decision")
	}
	if _, err = f.db.Exec(ctx, `UPDATE billing_raw_retention_operations SET status='ACTIVE',terminal_receipt=NULL,resolved_at=NULL WHERE operation_id=$1`, op.OperationID); err == nil {
		t.Fatal("terminal operation resurrected")
	}
}

func TestAuthorityAbortPermanentAndReleasePendingHold(t *testing.T) {
	f := newAuthorityFixture(t)
	f.clear(t)
	ctx := context.Background()
	op, err := f.store.RequestOperation(ctx, f.plan)
	if err != nil {
		t.Fatal(err)
	}
	h := Hold{HoldID: "pending-release", Scope: f.policy.Scope, Reason: "case", FinancialApprovalRef: "case-approved"}
	if _, err = f.store.PutHold(ctx, h); err != nil {
		t.Fatal(err)
	}
	if out, err := f.store.ReleaseHold(ctx, h.HoldID); err != nil || out.Status != "RELEASE_PENDING" || !out.ProtectArchiveAndKeys {
		t.Fatal("hold released while fence ambiguous")
	}
	if _, err = f.store.RequestAbort(ctx, op.OperationID); err != nil {
		t.Fatal(err)
	}
	f.setTerminal(op, "aborted")
	if terminal, err := f.store.Resolve(ctx, op.OperationID); err != nil || terminal.Status != "ABORTED" {
		t.Fatalf("abort not exact %+v %v", terminal, err)
	}
	if replay, err := f.store.RequestOperation(ctx, f.plan); err != nil || replay.Status != "ABORTED" {
		t.Fatal("late apply resurrected aborted operation")
	}
	changed := f.plan
	changed.ClearanceID = "different-clearance"
	changed.PlanSHA256 = PlanDigest(changed)
	if _, err = f.store.RequestOperation(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused operation identity changed: %v", err)
	}
	out, err := f.store.Hold(ctx, h.HoldID)
	if err != nil || out.Status != "RELEASED" || out.ProtectArchiveAndKeys {
		t.Fatal("release did not finalize on proven abort")
	}
}

func TestAuthorityHoldAndActiveDecisionRaceLinearizes(t *testing.T) {
	for i := 0; i < 6; i++ {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			f := newAuthorityFixture(t)
			f.clear(t)
			ctx := context.Background()
			start := make(chan struct{})
			opErr := make(chan error, 1)
			holdErr := make(chan error, 1)
			h := Hold{HoldID: "racing-hold", Scope: f.policy.Scope, FromSequence: 1, ThroughSequence: 2, Reason: "dispute", FinancialApprovalRef: "approved-dispute"}
			go func() { <-start; _, err := f.store.RequestOperation(ctx, f.plan); opErr <- err }()
			go func() { <-start; _, err := f.store.PutHold(ctx, h); holdErr <- err }()
			close(start)
			e := <-opErr
			if err := <-holdErr; err != nil {
				t.Fatal(err)
			}
			hold, err := f.store.Hold(ctx, h.HoldID)
			if err != nil {
				t.Fatal(err)
			}
			if e == nil {
				if hold.Status != "PENDING_FENCED" {
					t.Fatal("ACTIVE decision raced an already active hold")
				}
			} else if !errors.Is(e, ErrBlocked) || hold.Status != "ACTIVE" {
				t.Fatalf("race outcome status=%s error=%v", hold.Status, e)
			}
		})
	}
}

func TestUnknownIntentCancellationNeverReleasesAnAcceptedFence(t *testing.T) {
	f := newAuthorityFixture(t)
	f.clear(t)
	ctx := context.Background()
	active, err := f.store.RequestOperation(ctx, f.plan)
	if err != nil {
		t.Fatal(err)
	}
	unknown := f.plan
	unknown.OperationID = "cancel-never-accepted"
	unknown.PlanSHA256 = PlanDigest(unknown)
	unknown.ArchiveCompletion = SignedCompletion{}
	unknown.RangeProof = SignedCompletion{}
	if _, err = f.store.RequestAbort(ctx, unknown.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown id-only abort became a decision", err)
	}
	f.mu.Lock()
	callsBefore := f.consumerCalls
	f.mu.Unlock()
	cancelled, err := f.store.RequestAbortPlan(ctx, unknown)
	if err != nil || cancelled.Status != "ABORTED" || cancelled.DecisionOrigin != "cancelled-before-acceptance" || cancelled.TerminalReceipt != nil || cancelled.ResolvedAt == nil || len(cancelled.ConsumerProofs) != 0 {
		t.Fatal("never-accepted cancellation claimed a Logger terminal outcome", cancelled, err)
	}
	f.mu.Lock()
	callsAfter := f.consumerCalls
	f.mu.Unlock()
	if callsAfter != callsBefore {
		t.Fatal("denying deletion unnecessarily requested authorization evidence")
	}
	pending, err := f.store.Operations(ctx, f.policy.Scope)
	if err != nil || len(pending) != 1 || pending[0].OperationID != active.OperationID || pending[0].Status != "ACTIVE" {
		t.Fatal("unknown cancellation released another accepted operation", pending, err)
	}
	if replay, err := f.store.RequestOperation(ctx, unknown); err != nil || replay.Status != "ABORTED" || replay.TerminalReceipt != nil {
		t.Fatal("late command resurrected never-accepted intent", replay, err)
	}
	if resolved, err := f.store.Resolve(ctx, unknown.OperationID); err != nil || resolved.DecisionOrigin != "cancelled-before-acceptance" || resolved.TerminalReceipt != nil {
		t.Fatal("cancellation later fabricated participant evidence", resolved, err)
	}
	changed := unknown
	changed.ClearanceID = "different-clearance"
	changed.PlanSHA256 = PlanDigest(changed)
	if _, err = f.store.RequestAbortPlan(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("cancellation identity changed", err)
	}
	if _, err = f.db.Exec(ctx, `UPDATE billing_raw_retention_operations SET decision_origin='cancelled-before-acceptance',status='ABORTED',resolved_at=clock_timestamp() WHERE operation_id=$1`, active.OperationID); err == nil {
		t.Fatal("accepted operation was recast as a no-participant cancellation")
	}
	if _, err = f.db.Exec(ctx, `UPDATE billing_raw_retention_operations SET status='ACTIVE',decision_origin='accepted',resolved_at=NULL WHERE operation_id=$1`, unknown.OperationID); err == nil {
		t.Fatal("cancelled-before-acceptance tombstone resurrected")
	}
}

func TestUnknownCancellationAndAdmissionRaceLinearizes(t *testing.T) {
	for i := 0; i < 6; i++ {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			f := newAuthorityFixture(t)
			f.clear(t)
			ctx := context.Background()
			start := make(chan struct{})
			results := make(chan error, 2)
			go func() { <-start; _, err := f.store.RequestOperation(ctx, f.plan); results <- err }()
			go func() { <-start; _, err := f.store.RequestAbortPlan(ctx, f.plan); results <- err }()
			close(start)
			for n := 0; n < 2; n++ {
				if err := <-results; err != nil {
					t.Fatal("concurrent cancel/admission failed", err)
				}
			}
			op, err := f.store.Operation(ctx, f.plan.OperationID)
			if err != nil || op.TerminalReceipt != nil {
				t.Fatal("race fabricated a Logger terminal receipt", op, err)
			}
			pending, err := f.store.Operations(ctx, f.policy.Scope)
			if err != nil {
				t.Fatal(err)
			}
			switch op.DecisionOrigin {
			case "accepted":
				if op.Status != "ABORT_REQUESTED" || len(pending) != 1 || op.ResolvedAt != nil {
					t.Fatal("accepted decision lost fence", op, pending)
				}
			case "cancelled-before-acceptance":
				if op.Status != "ABORTED" || len(pending) != 0 || op.ResolvedAt == nil {
					t.Fatal("never-accepted intent acquired a deletion fence", op, pending)
				}
			default:
				t.Fatal("unclassified decision origin", op.DecisionOrigin)
			}
			if replay, err := f.store.RequestOperation(ctx, f.plan); err != nil || replay.Status == "ACTIVE" {
				t.Fatal("late admission reversed cancellation", replay, err)
			}
		})
	}
}

func TestInactivePolicyBlocksNewAdmissionButKeepsExactFenceRecovery(t *testing.T) {
	f := newAuthorityFixture(t)
	f.clear(t)
	ctx := context.Background()
	op, err := f.store.RequestOperation(ctx, f.plan)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := f.store.Policy(ctx, f.policy.PolicyID, f.policy.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.SetActive(ctx, f.policy.PolicyID, f.policy.Version, policy.PolicySHA256, false); err != nil {
		t.Fatal(err)
	}
	newIntent := f.plan
	newIntent.OperationID = "blocked-after-maintenance-stop"
	newIntent.PlanSHA256 = PlanDigest(newIntent)
	if _, err = f.store.RequestOperation(ctx, newIntent); !errors.Is(err, ErrBlocked) {
		t.Fatal("inactive policy admitted a new intent", err)
	}
	// Only the exact retained identity bypasses current policy activation and
	// proof freshness. It returns the prior decision rather than authorizing new
	// deletion or replacing immutable evidence.
	replay := f.plan
	replay.RangeProof.SignatureB64 = "expired"
	if prior, err := f.store.RequestOperation(ctx, replay); err != nil || prior.Status != "ACTIVE" || prior.RangeProof != op.RangeProof {
		t.Fatal("inactive policy prevented exact accepted-intent recovery", err)
	}
	changed := replay
	changed.ClearanceID = "changed-intent"
	changed.PlanSHA256 = PlanDigest(changed)
	if _, err = f.store.RequestOperation(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("inactive policy replay admitted changed immutable intent", err)
	}
	if pending, err := f.store.RequestAbort(ctx, op.OperationID); err != nil || pending.Status != "ABORT_REQUESTED" {
		t.Fatal("maintenance stop disabled abort or prematurely unlocked", err)
	}
	if _, err = f.store.Resolve(ctx, op.OperationID); err == nil {
		t.Fatal("inactive policy allowed an unproven terminal result")
	}
	f.setTerminal(op, "aborted")
	if resolved, err := f.store.Resolve(ctx, op.OperationID); err != nil || resolved.Status != "ABORTED" || resolved.TerminalReceipt == nil {
		t.Fatal("maintenance stop disabled exact Logger outcome reconciliation", err)
	}
	if pending, err := f.store.Operations(ctx, f.policy.Scope); err != nil || len(pending) != 0 {
		t.Fatal("proven terminal result failed to finish maintenance drain", err)
	}
	if prior, err := f.store.RequestOperation(ctx, replay); err != nil || prior.Status != "ABORTED" {
		t.Fatal("maintenance replay resurrected an aborted operation", err)
	}
}

func TestAuthorityImmutableApprovalsClearancesAndHoldReplay(t *testing.T) {
	f := newAuthorityFixture(t)
	ctx := context.Background()
	policy, err := f.store.Policy(ctx, f.policy.PolicyID, f.policy.Version)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := f.store.PutPolicy(ctx, f.policy); err != nil || replay.PolicySHA256 != policy.PolicySHA256 {
		t.Fatal("identical approved policy not replayable", err)
	}
	changed := f.policy
	changed.HotRetentionDays++
	if _, err := f.store.PutPolicy(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("immutable policy overwritten", err)
	}
	changed = f.policy
	changed.PolicyID = "unknown-consumer-policy"
	changed.RequiredConsumers = []string{"unregistered/consumer"}
	if _, err := f.store.PutPolicy(ctx, changed); !errors.Is(err, ErrBlocked) {
		t.Fatal("unregistered required consumer accepted", err)
	}
	if _, err := f.store.ApproveRecovery(ctx, policy.PolicyID, policy.Version, policy.PolicySHA256, policy.RecoveryApprovalRef); err != nil {
		t.Fatal("exact recovery approval not replayable", err)
	}
	if _, err := f.store.ApproveRecovery(ctx, policy.PolicyID, policy.Version, policy.PolicySHA256, "different-approval"); !errors.Is(err, ErrConflict) {
		t.Fatal("recovery approval rebound", err)
	}
	if _, err := f.store.SetActive(ctx, policy.PolicyID, policy.Version, strings.Repeat("0", 64), true); !errors.Is(err, ErrConflict) {
		t.Fatal("financial activation trusted a foreign policy hash", err)
	}
	if _, err := f.store.SetActive(ctx, policy.PolicyID, policy.Version, policy.PolicySHA256, true); err != nil {
		t.Fatal("exact financial activation not replayable", err)
	}
	uncleared := f.clearance
	uncleared.ClearanceID = "open-period-is-not-clearance"
	uncleared.Periods = append([]Period(nil), uncleared.Periods...)
	uncleared.Periods[0].PeriodStart = uncleared.Periods[0].PeriodStart.AddDate(0, 1, 0)
	uncleared.Periods[0].PeriodEnd = uncleared.Periods[0].PeriodEnd.AddDate(0, 1, 0)
	if _, err := f.store.PutClearance(ctx, uncleared); !errors.Is(err, ErrBlocked) {
		t.Fatal("attestation bypassed actual period closure", err)
	}
	f.clear(t)
	if _, err := f.store.PutClearance(ctx, f.clearance); err != nil {
		t.Fatal("exact clearance not replayable", err)
	}
	uncleared = f.clearance
	uncleared.ReconciliationSHA256 = strings.Repeat("5", 64)
	if _, err := f.store.PutClearance(ctx, uncleared); !errors.Is(err, ErrConflict) {
		t.Fatal("clearance evidence overwritten", err)
	}
	hold := Hold{HoldID: "immutable-hold", Scope: policy.Scope, Reason: "legal review", FinancialApprovalRef: "case-approved"}
	if _, err := f.store.PutHold(ctx, hold); err != nil {
		t.Fatal(err)
	}
	if replay, err := f.store.PutHold(ctx, hold); err != nil || replay.Status != "ACTIVE" || !replay.ProtectArchiveAndKeys {
		t.Fatal("active hold not replayable", err)
	}
	changedHold := hold
	changedHold.Reason = "different case"
	if _, err := f.store.PutHold(ctx, changedHold); !errors.Is(err, ErrConflict) {
		t.Fatal("hold identity rebound", err)
	}
	if _, err := f.store.ReleaseHold(ctx, hold.HoldID); err != nil {
		t.Fatal(err)
	}
	if released, err := f.store.ReleaseHold(ctx, hold.HoldID); err != nil || released.ProtectArchiveAndKeys {
		t.Fatal("released hold not replayable", err)
	}
	if replay, err := f.store.PutHold(ctx, hold); err != nil || replay.Status != "RELEASED" || replay.ProtectArchiveAndKeys {
		t.Fatal("late hold creation resurrected released identity", err)
	}
	if err := f.store.RevokeClearance(ctx, f.clearance.ClearanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RequestOperation(ctx, f.plan); !errors.Is(err, ErrBlocked) {
		t.Fatal("revoked clearance admitted a fresh operation", err)
	}
	if _, err := f.store.Resolve(ctx, "missing-operation"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown authority outcome invented", err)
	}
}

func TestAuthorityInvalidAndForeignScopeNeverCrossesBoundary(t *testing.T) {
	f := newAuthorityFixture(t)
	ctx := context.Background()
	if _, err := f.store.Policy(ctx, "../policy", 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.store.PutPolicy(ctx, Policy{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.store.ApproveRecovery(ctx, f.policy.PolicyID, 1, "bad-hash", "approval"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.store.SetActive(ctx, f.policy.PolicyID, 1, "bad-hash", true); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.store.PutClearance(ctx, Clearance{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := f.store.RevokeClearance(ctx, "../clearance"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := f.store.RevokeClearance(ctx, "unknown-clearance"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := f.store.Hold(ctx, "../hold"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.store.PutHold(ctx, Hold{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.store.ReleaseHold(ctx, "unknown-hold"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := f.store.Operation(ctx, "../operation"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.store.Operations(ctx, Scope{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.store.RequestOperation(ctx, Plan{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.store.RequestAbortPlan(ctx, Plan{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	f.clear(t)
	if _, err := f.store.RequestOperation(ctx, f.plan); err != nil {
		t.Fatal(err)
	}
	hold := Hold{HoldID: "scoped-hold", Scope: f.policy.Scope, Reason: "review", FinancialApprovalRef: "approved"}
	if _, err := f.store.PutHold(ctx, hold); err != nil {
		t.Fatal(err)
	}
	foreign, err := New(f.db, f.store.evidence, f.store.keys, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Policy(ctx, f.policy.PolicyID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign policy disclosed", err)
	}
	if _, err := foreign.Operation(ctx, f.plan.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign operation disclosed", err)
	}
	if _, err := foreign.Hold(ctx, hold.HoldID); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign hold disclosed", err)
	}
	if err := foreign.RevokeClearance(ctx, f.clearance.ClearanceID); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign clearance revoked", err)
	}
}

func TestAuthorityDatabaseUnavailableDoesNotProduceDecision(t *testing.T) {
	f := newAuthorityFixture(t)
	ctx := context.Background()
	// Use a separate closed pool: the fixture's live pool still owns the shared
	// test lock, so this does not interfere with another package's database.
	db, err := database.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	unavailable, err := New(db, f.store.evidence, f.store.keys, f.policy.Environment)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for name, call := range map[string]func() error{
		"policy":     func() error { _, err := unavailable.Policy(ctx, f.policy.PolicyID, 1); return err },
		"put-policy": func() error { _, err := unavailable.PutPolicy(ctx, f.policy); return err },
		"recovery-approval": func() error {
			_, err := unavailable.ApproveRecovery(ctx, f.policy.PolicyID, 1, digest(f.policy), "approval")
			return err
		},
		"activation": func() error {
			_, err := unavailable.SetActive(ctx, f.policy.PolicyID, 1, digest(f.policy), true)
			return err
		},
		"clearance":        func() error { _, err := unavailable.PutClearance(ctx, f.clearance); return err },
		"revoke-clearance": func() error { return unavailable.RevokeClearance(ctx, f.clearance.ClearanceID) },
		"hold": func() error {
			_, err := unavailable.PutHold(ctx, Hold{HoldID: "hold", Scope: f.policy.Scope, Reason: "review", FinancialApprovalRef: "approval"})
			return err
		},
		"release-hold": func() error { _, err := unavailable.ReleaseHold(ctx, "hold"); return err },
		"operation":    func() error { _, err := unavailable.Operation(ctx, f.plan.OperationID); return err },
		"list":         func() error { _, err := unavailable.Operations(ctx, f.policy.Scope); return err },
		"admit":        func() error { _, err := unavailable.RequestOperation(ctx, f.plan); return err },
		"cancel":       func() error { _, err := unavailable.RequestAbortPlan(ctx, f.plan); return err },
		"resolve":      func() error { _, err := unavailable.Resolve(ctx, f.plan.OperationID); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("database failure fabricated an authority result")
			}
		})
	}
}
