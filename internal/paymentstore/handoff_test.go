package paymentstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func handoffInput(t *testing.T, env paymentIntegrationEnv, account payment.CommercialAccount) PrepareOwnershipHandoffInput {
	t.Helper()
	in := PrepareOwnershipHandoffInput{
		OperationID: testutil.OrganizationID("handoff-" + account.ID), OrganizationID: account.OrganizationID,
		SourceUserID: testutil.OrganizationID("source"), TargetUserID: testutil.OrganizationID("target"),
		OwnershipVersion: 3, Cutoff: testTime(12, 0),
	}
	if err := env.store.InitializeResponsibility(context.Background(), InitialResponsibilityInput{
		AccountID: account.ID, OwnerUserID: in.SourceUserID, OwnershipVersion: in.OwnershipVersion,
		EffectiveFrom: testTime(8, 0), SourceEvidenceSHA256: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestHandoffPreparationEvidenceAndReplay(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "handoff-evidence", 20000, 10000, 50000)
	ctx := context.Background()
	in := PrepareOwnershipHandoffInput{
		OperationID: testutil.OrganizationID("operation"), OrganizationID: fixture.account.OrganizationID,
		SourceUserID: testutil.OrganizationID("source"), TargetUserID: testutil.OrganizationID("target"),
		OwnershipVersion: 3, Cutoff: testTime(12, 0),
	}
	if _, err := env.store.PrepareOwnershipHandoff(ctx, in); !errors.Is(err, ErrOwnershipEvidenceMissing) {
		t.Fatalf("missing responsibility must fail closed: %v", err)
	}
	in = handoffInput(t, env, fixture.account)
	for _, changed := range []PrepareOwnershipHandoffInput{
		func() PrepareOwnershipHandoffInput {
			v := in
			v.SourceUserID = testutil.OrganizationID("other-owner")
			return v
		}(),
		func() PrepareOwnershipHandoffInput { v := in; v.OwnershipVersion--; return v }(),
		func() PrepareOwnershipHandoffInput { v := in; v.Cutoff = testTime(7, 0); return v }(),
	} {
		if _, err := env.store.PrepareOwnershipHandoff(ctx, changed); !errors.Is(err, ErrOwnershipVersionConflict) {
			t.Fatalf("stale authority/cutoff must fail: %v", err)
		}
	}
	first, err := env.store.PrepareOwnershipHandoff(ctx, in)
	if err != nil || first.Phase != "preparing" || first.Version != 1 {
		t.Fatalf("prepare=%+v err=%v", first, err)
	}
	// A fresh store instance observes the same durable operation, even when a
	// transport serializes the cutoff in a different timezone/precision.
	in.Cutoff = in.Cutoff.In(time.FixedZone("client", 8*3600)).Add(123 * time.Nanosecond)
	replay, err := New(env.db).PrepareOwnershipHandoff(ctx, in)
	if err != nil || replay != first {
		t.Fatalf("replay=%+v first=%+v err=%v", replay, first, err)
	}
	changed := in
	changed.TargetUserID = testutil.OrganizationID("another-target")
	if _, err := env.store.PrepareOwnershipHandoff(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed same-operation payload: %v", err)
	}
	changed = in
	changed.OperationID = testutil.OrganizationID("second-operation")
	if _, err := env.store.PrepareOwnershipHandoff(ctx, changed); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("only one active handoff: %v", err)
	}
	if _, err := env.store.GetOwnershipHandoff(ctx, testutil.OrganizationID("unrelated-cloud"), in.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-cloud operation lookup: %v", err)
	}
	account, err := env.store.GetCommercialAccount(ctx, fixture.account.ID)
	if err != nil || account.AvailableBalanceMinor != fixture.account.AvailableBalanceMinor {
		t.Fatalf("prepare must not change money: account=%+v err=%v", account, err)
	}
	var periods, operations int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM billing_responsibility_periods),
		(SELECT count(*) FROM billing_ownership_handoffs)`).Scan(&periods, &operations); err != nil || periods != 1 || operations != 1 {
		t.Fatalf("periods=%d operations=%d err=%v", periods, operations, err)
	}
	var audits int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM billing_audit_events
		WHERE subject_id=$1 AND event_type='billing.ownership_handoff.prepare'`, first.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("one audit per preparation, not per replay: count=%d err=%v", audits, err)
	}
}

func TestHandoffConcurrentPreparationSerializes(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "handoff-concurrent", 0, 10000, 50000)
	in := handoffInput(t, env, fixture.account)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := env.store.PrepareOwnershipHandoff(ctx, in)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("identical concurrent replay: %v", err)
		}
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM billing_ownership_handoffs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestHandoffCompetingOperationsCannotBothPrepare(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "handoff-competing", 0, 10000, 50000)
	in := handoffInput(t, env, fixture.account)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, id := range []string{"competitor-one", "competitor-two"} {
		go func(id string) {
			candidate := in
			candidate.OperationID = testutil.OrganizationID(id)
			<-start
			_, err := env.store.PrepareOwnershipHandoff(ctx, candidate)
			results <- err
		}(id)
	}
	close(start)
	succeeded, fenced := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrHandoffFenced) {
			fenced++
		} else {
			t.Fatalf("unexpected competing result: %v", err)
		}
	}
	if succeeded != 1 || fenced != 1 {
		t.Fatalf("succeeded=%d fenced=%d", succeeded, fenced)
	}
}

func TestHandoffAuditFailureRollsBackFenceAndSetupRevocation(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "handoff-rollback", 0, 10000, 50000)
	in := handoffInput(t, env, fixture.account)
	ctx := context.Background()
	setup, err := env.store.BeginPaymentMethodSetup(ctx, setupForHandoff(t, env, fixture.account.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events ADD CONSTRAINT test_handoff_audit_failure
		CHECK (event_type <> 'billing.ownership_handoff.prepare') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := env.db.Exec(context.Background(), `ALTER TABLE billing_audit_events DROP CONSTRAINT IF EXISTS test_handoff_audit_failure`); err != nil {
			t.Error(err)
		}
	})
	if _, err := env.store.PrepareOwnershipHandoff(ctx, in); err == nil {
		t.Fatal("prepare without durable audit should fail")
	}
	if _, err := env.store.GetOwnershipHandoff(ctx, in.OrganizationID, in.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed transaction left a fence: %v", err)
	}
	var state, methodState string
	var noInvalidation, validConsent bool
	if err := env.db.QueryRow(ctx, `SELECT s.state,m.status,s.invalidated_by_handoff IS NULL,c.revoked_at IS NULL
		FROM payment_method_setup_sessions s JOIN payment_methods m ON m.id=s.payment_method_id
		JOIN payment_consents c ON c.id=m.consent_id WHERE s.id=$1`, setup.Session.ID).
		Scan(&state, &methodState, &noInvalidation, &validConsent); err != nil {
		t.Fatal(err)
	}
	if state != "created" || methodState != "pending" || !noInvalidation || !validConsent {
		t.Fatalf("partial preparation leaked: state=%s method=%s valid=%v consent=%v", state, methodState, noInvalidation, validConsent)
	}
}

func TestInitialResponsibilityCannotRewriteEvidenceOrOwner(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "responsibility-bootstrap", 0, 10000, 50000)
	ctx := context.Background()
	in := InitialResponsibilityInput{
		AccountID: fixture.account.ID, OwnerUserID: testutil.OrganizationID("source"), OwnershipVersion: 3,
		EffectiveFrom: testTime(8, 0), SourceEvidenceSHA256: strings.Repeat("a", 64),
	}
	for i := 0; i < 2; i++ {
		if err := env.store.InitializeResponsibility(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	changed := in
	changed.OwnerUserID = testutil.OrganizationID("different-owner")
	if err := env.store.InitializeResponsibility(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("bootstrap cannot be used as owner transfer: %v", err)
	}
	changed = in
	changed.SourceEvidenceSHA256 = strings.Repeat("b", 64)
	if err := env.store.InitializeResponsibility(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("evidence mutation: %v", err)
	}
	if _, err := env.db.Exec(ctx, `INSERT INTO billing_responsibility_periods
		(account_id,owner_user_id,ownership_version,effective_from,source_evidence_sha256)
		VALUES ($1,$2,4,now(),$3)`, in.AccountID, changed.OwnerUserID, changed.SourceEvidenceSHA256); err == nil {
		t.Fatal("database must reject two open responsibility periods")
	}
}

func setupForHandoff(t *testing.T, env paymentIntegrationEnv, accountID string) BeginPaymentMethodSetupInput {
	t.Helper()
	return BeginPaymentMethodSetupInput{
		AccountID: accountID, Provider: "fake", IdempotencyKey: "setup-handoff", RequestSHA256: strings.Repeat("a", 64),
		CorrelationID: "handoff-setup-test", Capabilities: payment.ProviderCapabilities{VaultedMethod: true},
		Consent: CreateConsentInput{
			AccountID: accountID, ConsentType: "payment_method",
			TextVersion: "v1", TextSHA256: strings.Repeat("a", 64), AcceptedActorType: "user",
			AcceptedActorID: testutil.OrganizationID("source"), Locale: "zh-TW", Source: "test",
		}, Now: testTime(10, 0),
	}
}

func TestHandoffInvalidatesHostedSetupAndRetainsLateEvidence(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "handoff-setup", 0, 10000, 50000)
	ctx := context.Background()
	setupIn := setupForHandoff(t, env, fixture.account.ID)
	setup, err := env.store.BeginPaymentMethodSetup(ctx, setupIn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CompletePaymentMethodSetup(ctx, CompletePaymentMethodSetupInput{
		AccountID: fixture.account.ID, SessionID: setup.Session.ID, State: payment.PaymentIntentStateRequiresAction,
		HostedURLSHA256: strings.Repeat("b", 64), ProviderCode: "OK",
	}); err != nil {
		t.Fatal(err)
	}
	in := handoffInput(t, env, fixture.account)
	if _, err := env.store.PrepareOwnershipHandoff(ctx, in); err != nil {
		t.Fatal(err)
	}
	late := CompletePaymentMethodSetupInput{
		AccountID: fixture.account.ID, SessionID: setup.Session.ID, State: payment.PaymentIntentStateSucceeded,
		HostedURLSHA256: strings.Repeat("b", 64), ProviderCode: "OK",
		ProviderCustomerRefCiphertext: []byte("old-customer-secret"), ProviderMethodRefCiphertext: []byte("old-card-secret"),
		ProviderMethodRefSHA256: strings.Repeat("c", 64), CardBrand: "visa", LastFour: "4242",
	}
	for i := 0; i < 2; i++ {
		if _, err := env.store.CompletePaymentMethodSetup(ctx, late); !errors.Is(err, ErrSetupInvalidated) {
			t.Fatalf("late setup completion: %v", err)
		}
	}
	if _, err := env.store.BeginPaymentMethodSetup(ctx, setupIn); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("even an old setup key cannot resume a hosted flow: %v", err)
	}
	var state, methodState string
	var consentRevoked, noCredentials bool
	var observations int
	if err := env.db.QueryRow(ctx, `SELECT s.state,m.status,c.revoked_at IS NOT NULL,
		m.provider_customer_ref_ciphertext IS NULL AND m.provider_method_ref_ciphertext IS NULL,
		(SELECT count(*) FROM billing_handoff_setup_observations WHERE session_id=s.id)
		FROM payment_method_setup_sessions s JOIN payment_methods m ON m.id=s.payment_method_id
		JOIN payment_consents c ON c.id=m.consent_id WHERE s.id=$1`, setup.Session.ID).
		Scan(&state, &methodState, &consentRevoked, &noCredentials, &observations); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || methodState != "revoked" || !consentRevoked || !noCredentials || observations != 1 {
		t.Fatalf("state=%s method=%s revoked=%v noCredentials=%v observations=%d", state, methodState, consentRevoked, noCredentials, observations)
	}
	// Simulate the eventual coordinator's terminal state, not a completed transfer
	// test. Persistent setup invalidation must outlive the monetary fence itself.
	if _, err := env.db.Exec(ctx, `UPDATE billing_ownership_handoffs SET phase='aborted' WHERE id=$1`, in.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CompletePaymentMethodSetup(ctx, late); !errors.Is(err, ErrSetupInvalidated) {
		t.Fatalf("cancellation must not revive an old setup: %v", err)
	}
}

func TestHandoffFencesNewPaymentsButPermitsReconciliation(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "handoff-payments", 20000, 10000, 50000)
	ctx := context.Background()
	manual := CreateManualTopUpInput{
		AccountID: fixture.account.ID, PaymentMethodID: fixture.method.ID, AmountMinor: 50000,
		Currency: payment.CurrencyTWD, IdempotencyKey: "before-handoff", CorrelationID: "manual-test", Now: testTime(10, 0),
	}
	oldIntent, err := env.store.CreateManualTopUp(ctx, manual)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := env.store.ClaimPaymentJobs(ctx, testTime(11, 0), testTime(9, 0), "test-worker", 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	in := handoffInput(t, env, fixture.account)
	if _, err := env.store.PrepareOwnershipHandoff(ctx, in); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"before-handoff", "after-handoff"} {
		manual.IdempotencyKey = key
		if _, err := env.store.CreateManualTopUp(ctx, manual); !errors.Is(err, ErrHandoffFenced) {
			t.Fatalf("new or replayed top-up key=%s: %v", key, err)
		}
	}
	if _, err := env.store.CreateHostedTopUp(ctx, CreateHostedTopUpInput{
		AccountID: fixture.account.ID, Provider: "fake", AmountMinor: 50000, Currency: payment.CurrencyTWD,
		IdempotencyKey: "new-hosted", CorrelationID: "hosted-test",
	}); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("hosted checkout must be fenced: %v", err)
	}
	if _, err := env.store.BeginProviderAttempt(ctx, BeginProviderAttemptInput{
		JobID: jobs[0].ID, LeaseOwner: "test-worker", Operation: payment.ProviderOperationCharge,
	}); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("leased but not dispatched charge must be fenced: %v", err)
	}
	if _, err := env.store.CreateConsent(ctx, CreateConsentInput{
		AccountID: fixture.account.ID, ConsentType: "payment_method", TextVersion: "v1", TextSHA256: strings.Repeat("a", 64),
		AcceptedActorType: "user", AcceptedActorID: in.SourceUserID, AcceptedAt: testTime(13, 0), Locale: "zh-TW", Source: "test",
	}); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("new consent: %v", err)
	}
	if _, err := env.store.CreatePaymentMethod(ctx, CreatePaymentMethodInput{
		AccountID: fixture.account.ID, Provider: "fake", ConsentID: fixture.method.ConsentID, Status: payment.PaymentMethodStatusPending,
	}); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("direct method creation: %v", err)
	}
	if _, err := env.store.PutAutoTopUpPolicy(ctx, PutAutoTopUpPolicyInput{
		AccountID: fixture.account.ID, PaymentMethodID: fixture.method.ID, ConsentID: fixture.policy.ConsentID, ActorID: in.SourceUserID,
	}); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("policy mutation: %v", err)
	}
	debit, err := env.store.PostLedgerEntry(ctx, debitInput(fixture.account.ID, "cutoff-debit", 30000, testTime(13, 0)))
	if err != nil || debit.Intent != nil || debit.Account.AvailableBalanceMinor != -10000 {
		t.Fatalf("cutoff debit must reconcile without automatic charge: %+v err=%v", debit, err)
	}
	credit := debitInput(fixture.account.ID, "manual-credit-fence", 10000, testTime(13, 0))
	credit.Direction, credit.Reason = payment.LedgerDirectionCredit, payment.LedgerReasonManualAdjustmentCredit
	if _, err := env.store.PostLedgerEntry(ctx, credit); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("manual top-up exception forbidden: %v", err)
	}
	// External outcomes remain ingestible even when new charge dispatch is denied.
	if _, err := env.store.TransitionIntent(ctx, TransitionIntentInput{IntentID: oldIntent.Intent.ID, ToState: payment.PaymentIntentStateProcessing}); err != nil {
		t.Fatal(err)
	}
	reconciled, err := env.store.TransitionIntent(ctx, TransitionIntentInput{IntentID: oldIntent.Intent.ID, ToState: payment.PaymentIntentStateSucceeded})
	if err != nil || reconciled.Account.AvailableBalanceMinor != 40000 {
		t.Fatalf("reconcile preexisting payment: %+v err=%v", reconciled, err)
	}
	op, err := env.store.GetOwnershipHandoff(ctx, in.OrganizationID, in.OperationID)
	if err != nil || op.Phase != "preparing" {
		t.Fatalf("positive credit is not settlement proof: op=%+v err=%v", op, err)
	}
}

func TestHandoffInputValidationWithoutDatabase(t *testing.T) {
	s := &Store{}
	if err := s.InitializeResponsibility(context.Background(), InitialResponsibilityInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("empty initialization: %v", err)
	}
	if _, err := s.PrepareOwnershipHandoff(context.Background(), PrepareOwnershipHandoffInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("empty preparation: %v", err)
	}
	for _, value := range []string{"", "not-a-uuid", "00000000-0000-0000-0000-000000000000", strings.ToUpper(testutil.OrganizationID("uppercase"))} {
		if canonicalUUID(value) {
			t.Fatalf("unsafe UUID accepted: %q", value)
		}
	}
}
