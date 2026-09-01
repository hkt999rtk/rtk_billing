package paymentstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
	"github.com/jackc/pgx/v5/pgconn"
)

func closureInput(t *testing.T, env paymentIntegrationEnv, account payment.CommercialAccount) PrepareCloudClosureInput {
	t.Helper()
	authority := handoffInput(t, env, account)
	return PrepareCloudClosureInput{Scope: CloudClosureScope{CloudPreflightScope: CloudPreflightScope{OrganizationID: account.OrganizationID, OwnerUserID: authority.SourceUserID, OwnershipVersion: authority.OwnershipVersion}, OperationID: testutil.OrganizationID("close-" + account.ID)}, Cutoff: authority.Cutoff, AMRequestSHA256: strings.Repeat("d", 64)}
}

func TestCloudClosureSettlementRejectsStaleAndMisboundEvidence(t *testing.T) {
	for _, change := range []string{"balance", "provider_manifest", "expired", "wrong_owner", "wrong_version", "cutoff", "missing_checkpoint"} {
		t.Run(change, func(t *testing.T) {
			env := newPaymentIntegrationEnv(t)
			f := createPaymentFixture(t, env, "closure-stale", 0, 100, 200)
			ctx := context.Background()
			in := closureInput(t, env, f.account)
			if _, err := env.store.PrepareCloudClosure(ctx, in); err != nil {
				t.Fatal(err)
			}
			receipt := closureReceipt(t, env, in.Scope, "stale-closure-receipt")
			if err := env.store.RecordCloudClosureSettlement(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			changed := receipt
			changed.ReceiptID = testutil.OrganizationID("changed-closure-receipt")
			switch change {
			case "balance":
				// A cutoff debit is allowed during preparing, but invalidates readiness.
				if _, err := env.store.PostLedgerEntry(ctx, debitInput(f.account.ID, "cutoff-debit", 1, in.Cutoff)); err != nil {
					t.Fatal(err)
				}
			case "provider_manifest":
				ackClosureTasks(t, env, in.Scope)
			case "expired":
				changed.State.ObservedAt = time.Now().Add(-2 * time.Minute)
				changed.ExpiresAt = time.Now().Add(-time.Minute)
			case "wrong_owner":
				changed.State.Scope.OwnerUserID = testutil.OrganizationID("wrong-closure-owner")
			case "wrong_version":
				changed.State.Scope.OwnershipVersion++
			case "cutoff":
				changed.State.Cutoff = changed.State.Cutoff.Add(-time.Second)
			case "missing_checkpoint":
				changed.UsageCheckpointSHA256 = ""
			}
			if err := env.store.RecordCloudClosureSettlement(ctx, changed); err == nil {
				t.Fatal("accepted stale/misbound settlement")
			}
			if change == "balance" || change == "provider_manifest" {
				status, err := env.store.GetCloudClosureStatus(ctx, in.Scope)
				if err != nil || status.Ready || status.ReceiptID != "" || !slices.Contains(status.Blockers, "evidence_unavailable") {
					t.Fatalf("stale receipt remained usable: %+v %v", status, err)
				}
				if _, err := env.store.CloseCloud(ctx, CloseCloudInput{Scope: in.Scope, SettlementID: receipt.ReceiptID, AMReadinessSHA256: strings.Repeat("e", 64)}); !errors.Is(err, ErrCloudClosureNotReady) {
					t.Fatalf("closed with stale receipt: %v", err)
				}
			}
		})
	}
}

// Explicit synthetic provider/collector fixtures. Not evidence of live provider
// cancellation, producer drain or an actual Account Manager deletion decision.
func ackClosureTasks(t *testing.T, env paymentIntegrationEnv, scope CloudClosureScope) {
	t.Helper()
	tasks, err := env.store.PendingCloudClosureRevocations(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if err := env.store.RecordCloudClosureRevocation(context.Background(), scope, task, strings.Repeat("b", 64)); err != nil {
			t.Fatal(err)
		}
	}
}
func closureReceipt(t *testing.T, env paymentIntegrationEnv, scope CloudClosureScope, name string) RecordCloudClosureSettlementInput {
	t.Helper()
	state, err := env.store.CaptureCloudClosureState(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	state.Financial.UsageSettled = true
	state.Financial.InvoicesReconciled = true
	state.Financial.ProviderWorkReconciled = true
	return RecordCloudClosureSettlementInput{State: state, ReceiptID: testutil.OrganizationID(name), CoveredThrough: state.Cutoff, ExpiresAt: state.ObservedAt.Add(time.Minute), UsageCheckpointSHA256: strings.Repeat("a", 64), InvoiceCheckpointSHA256: strings.Repeat("b", 64), ProviderCheckpointSHA256: strings.Repeat("c", 64)}
}
func closureBarrier(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "billing_cloud_closure_barrier" {
		t.Fatalf("wanted closure DB barrier, got %v", err)
	}
}

func TestCloudClosurePreparationRevokesAuthorityAndRejectsLateSetup(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	f := createPaymentFixture(t, env, "closure-prepare", 0, 10000, 50000)
	setup, err := env.store.BeginPaymentMethodSetup(ctx, setupForHandoff(t, env, f.account.ID))
	if err != nil {
		t.Fatal(err)
	}
	in := closureInput(t, env, f.account)
	identity := billingidentity.New(env.db)
	claims, err := identity.AuthorizeOwner(ctx, in.Scope.OrganizationID, in.Scope.OwnerUserID, in.Scope.OwnershipVersion)
	if err != nil {
		t.Fatal(err)
	}
	op, err := env.store.PrepareCloudClosure(ctx, in)
	if err != nil || op.Phase != "preparing" {
		t.Fatalf("prepare %+v %v", op, err)
	}
	replay, err := New(env.db).PrepareCloudClosure(ctx, in)
	if err != nil || replay != op {
		t.Fatalf("durable replay %+v %v", replay, err)
	}
	changed := in
	changed.AMRequestSHA256 = strings.Repeat("e", 64)
	if _, err := env.store.PrepareCloudClosure(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("rebound intent: %v", err)
	}
	tasks, err := env.store.PendingCloudClosureRevocations(ctx, in.Scope)
	if err != nil || len(tasks) != 3 {
		t.Fatalf("method/setup manifest %+v %v", tasks, err)
	}
	if _, err := identity.AuthorizeOwner(ctx, in.Scope.OrganizationID, in.Scope.OwnerUserID, in.Scope.OwnershipVersion); !errors.Is(err, billingidentity.ErrTransition) {
		t.Fatalf("closure owner still enabled: %v", err)
	}
	if _, err := env.store.PostLedgerEntry(billingidentity.WithScope(ctx, claims), debitInput(f.account.ID, "stale-owner", 1, time.Now())); !errors.Is(err, billingidentity.ErrTransition) {
		t.Fatalf("in-flight owner request bypass: %v", err)
	}
	if _, err := env.store.BeginPaymentMethodSetup(ctx, setupForHandoff(t, env, f.account.ID)); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("new setup: %v", err)
	}
	late := CompletePaymentMethodSetupInput{AccountID: f.account.ID, SessionID: setup.Session.ID, State: payment.PaymentIntentStateSucceeded, ProviderCode: "OK", HostedURLSHA256: strings.Repeat("a", 64), ProviderCustomerRefCiphertext: []byte("old-secret"), ProviderMethodRefCiphertext: []byte("old-method"), ProviderMethodRefSHA256: strings.Repeat("f", 64)}
	for i := 0; i < 2; i++ {
		if _, err := env.store.CompletePaymentMethodSetup(ctx, late); !errors.Is(err, ErrSetupInvalidated) {
			t.Fatalf("late hosted callback: %v", err)
		}
	}
	var methods, consents, policies, observations int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_methods WHERE account_id=$1 AND status<>'revoked'),(SELECT count(*) FROM payment_consents WHERE account_id=$1 AND revoked_at IS NULL),(SELECT count(*) FROM auto_topup_policies WHERE account_id=$1 AND (enabled OR armed)),(SELECT count(*) FROM billing_handoff_setup_observations WHERE session_id=$2)`, f.account.ID, setup.Session.ID).Scan(&methods, &consents, &policies, &observations); err != nil || methods != 0 || consents != 0 || policies != 0 || observations != 1 {
		t.Fatalf("authority/late audit: %d %d %d %d %v", methods, consents, policies, observations, err)
	}
	for _, q := range []string{`UPDATE payment_methods SET status='active' WHERE account_id=$1`, `UPDATE payment_consents SET revoked_at=NULL WHERE account_id=$1`, `UPDATE auto_topup_policies SET enabled=true WHERE account_id=$1`, `UPDATE payment_method_setup_sessions SET state='succeeded' WHERE account_id=$1`} {
		_, err := env.db.Exec(ctx, q, f.account.ID)
		closureBarrier(t, err)
	}
	handoff := PrepareOwnershipHandoffInput{OperationID: testutil.OrganizationID("competing-transfer"), OrganizationID: in.Scope.OrganizationID, SourceUserID: in.Scope.OwnerUserID, TargetUserID: testutil.OrganizationID("new-owner"), OwnershipVersion: in.Scope.OwnershipVersion, Cutoff: in.Cutoff}
	if _, err := env.store.PrepareOwnershipHandoff(ctx, handoff); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("overlapping handoff: %v", err)
	}
}

func TestCloudClosureRequiresZeroAndCurrentSettlement(t *testing.T) {
	for _, balance := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprint(balance), func(t *testing.T) {
			env, account, owner := cloudPreflightFixture(t, balance)
			ctx := context.Background()
			in := PrepareCloudClosureInput{Scope: CloudClosureScope{CloudPreflightScope: owner, OperationID: testutil.OrganizationID("zero-rule")}, Cutoff: time.Now(), AMRequestSHA256: strings.Repeat("a", 64)}
			if _, err := env.store.PrepareCloudClosure(ctx, in); err != nil {
				t.Fatal(err)
			}
			status, err := env.store.GetCloudClosureStatus(ctx, in.Scope)
			if err != nil || status.Ready {
				t.Fatalf("no evidence ready %+v %v", status, err)
			}
			closeIn := CloseCloudInput{Scope: in.Scope, SettlementID: testutil.OrganizationID("closure-settlement"), AMReadinessSHA256: strings.Repeat("d", 64)}
			if _, err := env.store.CloseCloud(ctx, closeIn); !errors.Is(err, ErrCloudClosureNotReady) {
				t.Fatalf("preflight alone closed: %v", err)
			}
			receipt := closureReceipt(t, env, in.Scope, "closure-settlement")
			if err := env.store.RecordCloudClosureSettlement(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			status, err = env.store.GetCloudClosureStatus(ctx, in.Scope)
			if err != nil || status.Ready != (balance == 0) {
				t.Fatalf("zero rule %+v %v", status, err)
			}
			ack, err := env.store.CloseCloud(ctx, closeIn)
			if balance != 0 {
				if !errors.Is(err, ErrCloudClosureNotReady) {
					t.Fatalf("nonzero closed %+v %v", ack, err)
				}
				return
			}
			if err != nil || ack.Phase != "closed" || ack.ReceiptSHA256 == "" {
				t.Fatalf("close %+v %v", ack, err)
			}
			replay, err := New(env.db).CloseCloud(ctx, closeIn)
			if err != nil || replay != ack {
				t.Fatalf("lost reply recovery %+v %v", replay, err)
			}
			changed := closeIn
			changed.AMReadinessSHA256 = strings.Repeat("f", 64)
			if _, err := env.store.CloseCloud(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("changed completion: %v", err)
			}
			for _, q := range []string{`UPDATE commercial_accounts SET state='active' WHERE id=$1`, `UPDATE commercial_accounts SET available_balance_minor=1 WHERE id=$1`, `INSERT INTO balance_ledger_entries(account_id,direction,amount_minor,currency,reason,idempotency_scope,idempotency_key,balance_after_minor) VALUES($1,'credit',1,'TWD','manual_adjustment_credit','test','after-close',1)`} {
				_, err := env.db.Exec(ctx, q, account.ID)
				closureBarrier(t, err)
			}
			_, err = env.db.Exec(ctx, `UPDATE billing_access_states SET state='active' WHERE organization_id=$1`, owner.OrganizationID)
			closureBarrier(t, err)
			_, err = env.db.Exec(ctx, `UPDATE billing_responsibility_periods SET effective_until=NULL WHERE account_id=$1`, account.ID)
			closureBarrier(t, err)
			_, err = env.db.Exec(ctx, `INSERT INTO billing_responsibility_periods(account_id,owner_user_id,ownership_version,effective_from,source_evidence_sha256) SELECT account_id,owner_user_id,ownership_version+1,now(),source_evidence_sha256 FROM billing_responsibility_periods WHERE account_id=$1`, account.ID)
			closureBarrier(t, err)
			if _, err := env.store.CancelCloudClosure(ctx, in.Scope, testutil.OrganizationID("late-cancel"), strings.Repeat("a", 64)); !errors.Is(err, ErrConflict) {
				t.Fatalf("closed cancellation: %v", err)
			}
			var periodClosed bool
			var phase, state string
			if err := env.db.QueryRow(ctx, `SELECT c.phase,a.state,p.effective_until IS NOT NULL FROM billing_cloud_closures c JOIN commercial_accounts a ON a.id=c.account_id JOIN billing_responsibility_periods p ON p.id=c.source_period_id WHERE c.id=$1`, in.Scope.OperationID).Scan(&phase, &state, &periodClosed); err != nil || phase != "closed" || state != "closed" || !periodClosed {
				t.Fatalf("atomic close: %s %s %v %v", phase, state, periodClosed, err)
			}
		})
	}
}

func TestCloudClosureProviderManifestAndCancellation(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	f := createPaymentFixture(t, env, "close-cancel", 0, 10000, 50000)
	in := closureInput(t, env, f.account)
	if _, err := env.store.PrepareCloudClosure(ctx, in); err != nil {
		t.Fatal(err)
	}
	receipt := closureReceipt(t, env, in.Scope, "before-provider-ack")
	if err := env.store.RecordCloudClosureSettlement(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	status, err := env.store.GetCloudClosureStatus(ctx, in.Scope)
	if err != nil || status.Ready || !slices.Contains(status.Blockers, "provider_revocations_pending") {
		t.Fatalf("local revoke implies provider ack %+v %v", status, err)
	}
	cancelID := testutil.OrganizationID("closure-cancel")
	op, err := env.store.CancelCloudClosure(ctx, in.Scope, cancelID, strings.Repeat("c", 64))
	if err != nil || op.Phase != "canceling" {
		t.Fatalf("cancel %+v %v", op, err)
	}
	if _, err := env.store.CompleteCloudClosureCancellation(ctx, in.Scope, cancelID, strings.Repeat("d", 64)); err == nil {
		t.Fatal("released before provider cancellation")
	}
	var releases int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM billing_cloud_closure_release_acks`).Scan(&releases); err != nil || releases != 0 {
		t.Fatalf("partial release persisted %d %v", releases, err)
	}
	ackClosureTasks(t, env, in.Scope)
	op, err = env.store.CompleteCloudClosureCancellation(ctx, in.Scope, cancelID, strings.Repeat("d", 64))
	if err != nil || op.Phase != "canceled" {
		t.Fatalf("release %+v %v", op, err)
	}
	if _, err := New(env.db).CompleteCloudClosureCancellation(ctx, in.Scope, cancelID, strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	var methodStatus string
	var enabled, armed bool
	if err := env.db.QueryRow(ctx, `SELECT m.status,p.enabled,p.armed FROM payment_methods m JOIN auto_topup_policies p ON p.payment_method_id=m.id WHERE m.id=$1`, f.method.ID).Scan(&methodStatus, &enabled, &armed); err != nil || methodStatus != "revoked" || enabled || armed {
		t.Fatalf("cancel revived payer %s %v %v %v", methodStatus, enabled, armed, err)
	}
	if _, err := env.store.CreateConsent(ctx, CreateConsentInput{AccountID: f.account.ID, ConsentType: "payment_method", TextVersion: "new", TextSHA256: strings.Repeat("a", 64), AcceptedActorType: "user", AcceptedActorID: in.Scope.OwnerUserID, AcceptedAt: time.Now(), Locale: "zh-TW", Source: "test"}); err != nil {
		t.Fatalf("new consent after confirmed release: %v", err)
	}
	_, err = env.db.Exec(ctx, `UPDATE payment_methods SET status='active' WHERE id=$1`, f.method.ID)
	closureBarrier(t, err)
	_, err = env.db.Exec(ctx, `UPDATE payment_consents SET revoked_at=NULL WHERE account_id=$1 AND revocation_reason='cloud_closure'`, f.account.ID)
	closureBarrier(t, err)
	_, err = env.db.Exec(ctx, `UPDATE payment_consents SET revocation_reason='other' WHERE account_id=$1 AND revocation_reason='cloud_closure'`, f.account.ID)
	closureBarrier(t, err)
}

func TestCloudClosureConcurrentReplayAndAuditRollback(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	ctx := context.Background()
	f := createPaymentFixture(t, env, "close-rollback", 0, 10000, 50000)
	in := closureInput(t, env, f.account)
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events ADD CONSTRAINT test_closure_audit CHECK(event_type<>'billing.cloud_closure.prepare') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(context.Background(), `ALTER TABLE billing_audit_events DROP CONSTRAINT IF EXISTS test_closure_audit`)
	})
	if _, err := env.store.PrepareCloudClosure(ctx, in); err == nil {
		t.Fatal("audit failed but closure persisted")
	}
	var closures int
	var status string
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM billing_cloud_closures),status FROM payment_methods WHERE id=$1`, f.method.ID).Scan(&closures, &status); err != nil || closures != 0 || status != "active" {
		t.Fatalf("partial preparation %d %s %v", closures, status, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events DROP CONSTRAINT test_closure_audit`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := env.store.PrepareCloudClosure(ctx, in); failures <- err }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	ackClosureTasks(t, env, in.Scope)
	receipt := closureReceipt(t, env, in.Scope, "close-final")
	if err := env.store.RecordCloudClosureSettlement(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	final := CloseCloudInput{Scope: in.Scope, SettlementID: receipt.ReceiptID, AMReadinessSHA256: strings.Repeat("e", 64)}
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events ADD CONSTRAINT test_closure_audit CHECK(event_type<>'billing.cloud_closure.closed') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CloseCloud(ctx, final); err == nil {
		t.Fatal("closed without audit")
	}
	var phase string
	if err := env.db.QueryRow(ctx, `SELECT phase FROM billing_cloud_closures WHERE id=$1`, in.Scope.OperationID).Scan(&phase); err != nil || phase != "preparing" {
		t.Fatalf("partial close %s %v", phase, err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events DROP CONSTRAINT test_closure_audit`); err != nil {
		t.Fatal(err)
	}
	results := make(chan CloudClosureAck, 8)
	failures = make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ack, err := New(env.db).CloseCloud(ctx, final)
			results <- ack
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first CloudClosureAck
	for ack := range results {
		if first.OperationID == "" {
			first = ack
		}
		if first != ack {
			t.Fatalf("unstable closure receipt %+v %+v", first, ack)
		}
	}
}

func TestCloudClosureSerializesWithNewPayerAuthority(t *testing.T) {
	env, _, owner := cloudPreflightFixture(t, 0)
	ctx := context.Background()
	in := PrepareCloudClosureInput{Scope: CloudClosureScope{CloudPreflightScope: owner, OperationID: testutil.OrganizationID("closure-payment-race")}, Cutoff: time.Now(), AMRequestSHA256: strings.Repeat("a", 64)}
	var accountID string
	if err := env.db.QueryRow(ctx, `SELECT id::text FROM commercial_accounts WHERE organization_id=$1`, owner.OrganizationID).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsCh := make(chan error, 9)
	for i := 0; i < 8; i++ {
		go func() {
			<-start
			_, err := env.store.CreateConsent(ctx, CreateConsentInput{AccountID: accountID, ConsentType: "payment_method", TextVersion: "race", TextSHA256: strings.Repeat("b", 64), AcceptedActorType: "user", AcceptedActorID: owner.OwnerUserID, AcceptedAt: time.Now(), Locale: "zh-TW", Source: "test"})
			if errors.Is(err, ErrHandoffFenced) {
				err = nil
			}
			errorsCh <- err
		}()
	}
	go func() {
		<-start
		_, err := env.store.PrepareCloudClosure(ctx, in)
		errorsCh <- err
	}()
	close(start)
	for i := 0; i < 9; i++ {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	var active int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM payment_consents WHERE account_id=$1 AND revoked_at IS NULL`, accountID).Scan(&active); err != nil || active != 0 {
		t.Fatalf("concurrent authority escaped fence: %d %v", active, err)
	}
}
