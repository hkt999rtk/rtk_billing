package paymentstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func authorizeFixtureHandoff(t *testing.T, env paymentIntegrationEnv, prepare PrepareOwnershipHandoffInput, scope HandoffScope) HandoffCommitAuthorization {
	t.Helper()
	ctx := context.Background()
	status, err := env.store.RecordHandoffSettlement(ctx, settlementReceipt(t, env, scope, scope.OperationID+"/settled"))
	if err != nil || status.Snapshot == nil {
		t.Fatalf("settlement=%+v err=%v", status, err)
	}
	for _, user := range []string{prepare.SourceUserID, prepare.TargetUserID} {
		if _, err := env.store.ConfirmHandoffSnapshot(ctx, snapshotConfirmation(scope, user, status.Snapshot)); err != nil {
			t.Fatal(err)
		}
	}
	grant, err := env.store.AuthorizeHandoffCommit(ctx, AuthorizeHandoffCommitInput{
		Scope: scope, AuthorizationID: testutil.OrganizationID(scope.OperationID + "/authorization"), SnapshotVersion: status.Snapshot.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func finalizeFixtureInput(scope HandoffScope, targetID string, grant HandoffCommitAuthorization) FinalizeHandoffInput {
	return FinalizeHandoffInput{
		Scope: scope, AuthorizationID: grant.AuthorizationID, CommittedOwnerUserID: targetID,
		CommittedOwnershipVersion: scope.OwnershipVersion + 1, CommittedAt: grant.CreatedAt.Add(time.Microsecond),
		AMCommitSHA256: strings.Repeat("d", 64),
	}
}

func TestHandoffCommitRequiresBothConfirmationsAndFencesLedgerAndUsage(t *testing.T) {
	env, account, prepare, scope := newSettlementFixture(t, 1)
	ctx := context.Background()
	status, err := env.store.RecordHandoffSettlement(ctx, settlementReceipt(t, env, scope, "authorization-snapshot"))
	if err != nil || status.Snapshot == nil {
		t.Fatalf("snapshot=%+v err=%v", status, err)
	}
	in := AuthorizeHandoffCommitInput{Scope: scope, AuthorizationID: testutil.OrganizationID("grant"), SnapshotVersion: status.Snapshot.Version}
	if _, err := env.store.AuthorizeHandoffCommit(ctx, in); !errors.Is(err, ErrHandoffNotConfirmable) {
		t.Fatalf("missing confirmations: %v", err)
	}
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, snapshotConfirmation(scope, prepare.SourceUserID, status.Snapshot)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AuthorizeHandoffCommit(ctx, in); !errors.Is(err, ErrHandoffNotConfirmable) {
		t.Fatalf("only source confirmation: %v", err)
	}
	if _, err := env.store.ConfirmHandoffSnapshot(ctx, snapshotConfirmation(scope, prepare.TargetUserID, status.Snapshot)); err != nil {
		t.Fatal(err)
	}
	grant, err := env.store.AuthorizeHandoffCommit(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := New(env.db).AuthorizeHandoffCommit(ctx, in); err != nil || replay != grant {
		t.Fatalf("authorization replay=%+v err=%v", replay, err)
	}
	changed := in
	changed.AuthorizationID = testutil.OrganizationID("changed-grant")
	if _, err := env.store.AuthorizeHandoffCommit(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed grant replay: %v", err)
	}
	if _, err := env.store.PostLedgerEntry(ctx, debitInput(account.ID, "after-grant", 1, testTime(14, 0))); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("postgrant debit: %v", err)
	}
	for _, query := range []string{
		`UPDATE commercial_accounts SET available_balance_minor=available_balance_minor-1 WHERE id=$1`,
		`INSERT INTO billing_usage_facts (usage_id,organization_id,service_code,metric_code,quantity,unit,window_start,window_end,source,source_sha256)
		 SELECT 'postgrant',organization_id,'mqtt','message',1,'message',now()-interval '1 hour',now(),'test',repeat('a',64) FROM commercial_accounts WHERE id=$1`,
		`INSERT INTO billing_periods (organization_id,currency,period_start,period_end)
		 SELECT organization_id,'TWD',now()-interval '1 hour',now() FROM commercial_accounts WHERE id=$1`,
	} {
		_, err := env.db.Exec(ctx, query, account.ID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != "billing_handoff_commit_barrier" {
			t.Fatalf("database commit barrier: %v", err)
		}
	}
	// All failed writes roll back; the same grant remains fresh and usable.
	if _, err := env.store.FinalizeOwnershipHandoff(ctx, finalizeFixtureInput(scope, prepare.TargetUserID, grant)); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffFinalizeRetiresProfileRevokesConsentAndPreservesBalance(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "handoff-finalize", 20000, 10000, 50000)
	ctx := context.Background()
	if _, err := env.db.Exec(ctx, `INSERT INTO billing_profiles
		(organization_id,legal_name,tax_identifier,billing_address,contact_email,delivery_preference,version)
		VALUES ($1,'Former owner','OLD-TAX','Old private address','old@example.test','portal_and_email',7)`, fixture.account.OrganizationID); err != nil {
		t.Fatal(err)
	}
	prepare := handoffInput(t, env, fixture.account)
	if _, err := env.store.PrepareOwnershipHandoff(ctx, prepare); err != nil {
		t.Fatal(err)
	}
	scope := HandoffScope{OrganizationID: prepare.OrganizationID, OperationID: prepare.OperationID, OwnershipVersion: prepare.OwnershipVersion}
	grant := authorizeFixtureHandoff(t, env, prepare, scope)
	in := finalizeFixtureInput(scope, prepare.TargetUserID, grant)
	wrong := in
	wrong.CommittedOwnerUserID = prepare.SourceUserID
	if _, err := env.store.FinalizeOwnershipHandoff(ctx, wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong committed owner: %v", err)
	}
	wrong = in
	wrong.CommittedOwnershipVersion++
	if _, err := env.store.FinalizeOwnershipHandoff(ctx, wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong committed version: %v", err)
	}
	ack, err := env.store.FinalizeOwnershipHandoff(ctx, in)
	if err != nil || ack.Phase != "finalized" {
		t.Fatalf("finalize=%+v err=%v", ack, err)
	}
	if replay, err := New(env.db).FinalizeOwnershipHandoff(ctx, in); err != nil || replay != ack {
		t.Fatalf("finalize replay=%+v err=%v", replay, err)
	}
	wrong = in
	wrong.AMCommitSHA256 = strings.Repeat("e", 64)
	if _, err := env.store.FinalizeOwnershipHandoff(ctx, wrong); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed finalization: %v", err)
	}
	account, err := env.store.GetCommercialAccount(ctx, fixture.account.ID)
	if err != nil || account.AvailableBalanceMinor != 20000 || account.Version != fixture.account.Version {
		t.Fatalf("transfer changed ledger balance: %+v err=%v", account, err)
	}
	var activeOwner string
	var version, opening int64
	var periodCount int
	if err := env.db.QueryRow(ctx, `SELECT owner_user_id::text,ownership_version,opening_balance_minor,
		(SELECT count(*) FROM billing_responsibility_periods WHERE account_id=$1)
		FROM billing_responsibility_periods WHERE account_id=$1 AND effective_until IS NULL`, account.ID).
		Scan(&activeOwner, &version, &opening, &periodCount); err != nil {
		t.Fatal(err)
	}
	if activeOwner != prepare.TargetUserID || version != in.CommittedOwnershipVersion || opening != 20000 || periodCount != 2 {
		t.Fatalf("owner=%s version=%d opening=%d periods=%d", activeOwner, version, opening, periodCount)
	}
	var sourceUntil time.Time
	if err := env.db.QueryRow(ctx, `SELECT effective_until FROM billing_responsibility_periods WHERE account_id=$1 AND owner_user_id=$2`, account.ID, prepare.SourceUserID).Scan(&sourceUntil); err != nil || !sourceUntil.Equal(in.CommittedAt) {
		t.Fatalf("source boundary=%v err=%v", sourceUntil, err)
	}
	method, err := env.store.GetPaymentMethod(ctx, account.ID, fixture.method.ID)
	if err != nil || method.Status != payment.PaymentMethodStatusRevoked {
		t.Fatalf("method=%+v err=%v", method, err)
	}
	policy, err := env.store.GetAutoTopUpPolicy(ctx, account.ID)
	if err != nil || policy.Enabled || policy.Armed {
		t.Fatalf("policy=%+v err=%v", policy, err)
	}
	var liveConsents int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM payment_consents WHERE account_id=$1 AND revoked_at IS NULL`, account.ID).Scan(&liveConsents); err != nil || liveConsents != 0 {
		t.Fatalf("live consents=%d err=%v", liveConsents, err)
	}
	profiles := billingstore.New(env.db)
	profile, err := profiles.GetBillingProfile(ctx, account.OrganizationID)
	if err != nil || !profile.RequiresConfiguration || profile.LegalName != "" || profile.TaxIdentifier != "" || profile.ContactEmail != "" || profile.BillingAddress != "" || profile.Version != 8 || profile.OwnershipVersion == nil || *profile.OwnershipVersion != in.CommittedOwnershipVersion {
		t.Fatalf("profile not reset: %+v err=%v", profile, err)
	}
	var historicalName string
	if err := env.db.QueryRow(ctx, `SELECT profile_snapshot->>'legal_name' FROM billing_retired_profiles WHERE operation_id=$1`, scope.OperationID).Scan(&historicalName); err != nil || historicalName != "Former owner" {
		t.Fatalf("archive=%q err=%v", historicalName, err)
	}
	draft, err := billing.BuildDraftInvoice(billing.Invoice{
		OrganizationID: account.OrganizationID, Recipient: profile, PricingVersionID: testutil.OrganizationID("draft-pricing"),
		Currency: billing.CurrencyTWD, PeriodStart: in.CommittedAt, PeriodEnd: in.CommittedAt.Add(time.Hour),
	}, nil, nil)
	if err != nil || !draft.Recipient.RequiresConfiguration || draft.Recipient.LegalName != "" {
		t.Fatalf("accounting must continue without copying old recipient PII: %+v err=%v", draft, err)
	}
	if _, err := billing.IssueInvoice(billing.Invoice{Recipient: profile}, "x", time.Now(), time.Now()); !errors.Is(err, billing.ErrProfileConfigurationRequired) {
		t.Fatalf("unset profile allowed invoice issuance: %v", err)
	}
	if _, err := profiles.PutBillingProfile(ctx, billingstore.PutProfileInput{OrganizationID: account.OrganizationID, LegalName: "Old stale form", Locale: "zh-TW", Timezone: "Asia/Taipei", DeliveryPreference: "portal", ExpectedVersion: 7}); !errors.Is(err, billingstore.ErrConflict) {
		t.Fatalf("stale profile ETag: %v", err)
	}
	configured, err := profiles.PutBillingProfile(ctx, billingstore.PutProfileInput{
		OrganizationID: account.OrganizationID, LegalName: "Incoming owner", ContactEmail: "new@example.test",
		Locale: "zh-TW", Timezone: "Asia/Taipei", DeliveryPreference: "portal", ExpectedVersion: profile.Version,
	})
	if err != nil || configured.RequiresConfiguration || configured.LegalName != "Incoming owner" || configured.OwnershipVersion == nil || *configured.OwnershipVersion != in.CommittedOwnershipVersion {
		t.Fatalf("incoming owner profile could not be configured: %+v err=%v", configured, err)
	}
	if _, err := env.store.BeginOwnershipHandoffAbort(ctx, BeginHandoffAbortInput{Scope: scope, CancellationID: testutil.OrganizationID("too-late-cancel"), AuthorizationID: grant.AuthorizationID, AMCancellationSHA256: strings.Repeat("f", 64)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("postcommit abort: %v", err)
	}
}

func TestHandoffFinalizationAuditFailureIsAtomicAndRetryable(t *testing.T) {
	env, account, prepare, scope := newSettlementFixture(t, 0)
	grant := authorizeFixtureHandoff(t, env, prepare, scope)
	in := finalizeFixtureInput(scope, prepare.TargetUserID, grant)
	ctx := context.Background()
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events ADD CONSTRAINT test_finalize_audit_failure CHECK(event_type<>'billing.ownership_handoff.finalize') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := env.db.Exec(context.Background(), `ALTER TABLE billing_audit_events DROP CONSTRAINT IF EXISTS test_finalize_audit_failure`)
		if err != nil {
			t.Error(err)
		}
	})
	if _, err := env.store.FinalizeOwnershipHandoff(ctx, in); err == nil {
		t.Fatal("finalize without audit succeeded")
	}
	var owner string
	var periods, finalizations int
	if err := env.db.QueryRow(ctx, `SELECT owner_user_id::text,(SELECT count(*) FROM billing_responsibility_periods WHERE account_id=$1),
		(SELECT count(*) FROM billing_handoff_finalizations WHERE operation_id=$2) FROM billing_responsibility_periods WHERE account_id=$1 AND effective_until IS NULL`, account.ID, scope.OperationID).Scan(&owner, &periods, &finalizations); err != nil {
		t.Fatal(err)
	}
	if owner != prepare.SourceUserID || periods != 1 || finalizations != 0 {
		t.Fatalf("partial finalization: owner=%s periods=%d finalizations=%d", owner, periods, finalizations)
	}
	op, err := env.store.GetOwnershipHandoff(ctx, scope.OrganizationID, scope.OperationID)
	if err != nil || op.Phase != "finalizing" {
		t.Fatalf("failed finalization released fence: %+v err=%v", op, err)
	}
	if _, err := env.store.BeginOwnershipHandoffAbort(ctx, BeginHandoffAbortInput{
		Scope: scope, CancellationID: testutil.OrganizationID("cancel-after-local-failure"),
		AMCancellationSHA256: strings.Repeat("a", 64), AuthorizationID: grant.AuthorizationID,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("known committed owner was allowed to roll back after local failure: %v", err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events DROP CONSTRAINT test_finalize_audit_failure`); err != nil {
		t.Fatal(err)
	}
	if ack, err := env.store.FinalizeOwnershipHandoff(ctx, in); err != nil || ack.Phase != "finalized" {
		t.Fatalf("retry=%+v err=%v", ack, err)
	}
}

func TestHandoffAbortRequiresCancellationAndHoldRelease(t *testing.T) {
	env, account, prepare, scope := newSettlementFixture(t, -1)
	ctx := context.Background()
	release := CompleteHandoffAbortInput{Scope: scope, CancellationID: testutil.OrganizationID("cancel"), HoldReleaseSHA256: strings.Repeat("b", 64)}
	if _, err := env.store.CompleteOwnershipHandoffAbort(ctx, release); !errors.Is(err, ErrNotFound) {
		t.Fatalf("release without cancellation: %v", err)
	}
	in := BeginHandoffAbortInput{Scope: scope, CancellationID: release.CancellationID, AMCancellationSHA256: strings.Repeat("a", 64)}
	if ack, err := env.store.BeginOwnershipHandoffAbort(ctx, in); err != nil || ack.Phase != "abort_pending" {
		t.Fatalf("begin abort=%+v err=%v", ack, err)
	}
	credit := PostLedgerEntryInput{AccountID: account.ID, Direction: payment.LedgerDirectionCredit, Reason: payment.LedgerReasonManualAdjustmentCredit, AmountMinor: 1, Currency: payment.CurrencyTWD, IdempotencyScope: "test", IdempotencyKey: "after-cancel"}
	if _, err := env.store.PostLedgerEntry(ctx, credit); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("cancellation alone released money fence: %v", err)
	}
	if ack, err := env.store.CompleteOwnershipHandoffAbort(ctx, release); err != nil || ack.Phase != "aborted" {
		t.Fatalf("complete abort=%+v err=%v", ack, err)
	}
	if _, err := New(env.db).CompleteOwnershipHandoffAbort(ctx, release); err != nil {
		t.Fatal(err)
	}
	if result, err := env.store.PostLedgerEntry(ctx, credit); err != nil || result.Account.AvailableBalanceMinor != 0 {
		t.Fatalf("original owner cannot settle after acknowledged release: %+v err=%v", result, err)
	}
	var owner string
	if err := env.db.QueryRow(ctx, `SELECT owner_user_id::text FROM billing_responsibility_periods WHERE account_id=$1 AND effective_until IS NULL`, account.ID).Scan(&owner); err != nil || owner != prepare.SourceUserID {
		t.Fatalf("cancellation changed owner: %s err=%v", owner, err)
	}
	changed := release
	changed.HoldReleaseSHA256 = strings.Repeat("c", 64)
	if _, err := env.store.CompleteOwnershipHandoffAbort(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed hold release: %v", err)
	}
}

func TestHandoffConcurrentFinalizationIsExactlyOnce(t *testing.T) {
	env, account, prepare, scope := newSettlementFixture(t, 0)
	grant := authorizeFixtureHandoff(t, env, prepare, scope)
	in := finalizeFixtureInput(scope, prepare.TargetUserID, grant)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := env.store.FinalizeOwnershipHandoff(ctx, in); results <- err }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var periods, acks int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM billing_responsibility_periods WHERE account_id=$1),
		(SELECT count(*) FROM billing_handoff_finalizations WHERE operation_id=$2)`, account.ID, scope.OperationID).Scan(&periods, &acks); err != nil || periods != 2 || acks != 1 {
		t.Fatalf("periods=%d finalizations=%d err=%v", periods, acks, err)
	}
}

func TestHandoffReturningOwnerGetsANewPeriodAndOldReplayCannotReopenHistory(t *testing.T) {
	env, account, firstPrepare, firstScope := newSettlementFixture(t, 0)
	ctx := context.Background()
	firstGrant := authorizeFixtureHandoff(t, env, firstPrepare, firstScope)
	firstFinalize := finalizeFixtureInput(firstScope, firstPrepare.TargetUserID, firstGrant)
	if _, err := env.store.FinalizeOwnershipHandoff(ctx, firstFinalize); err != nil {
		t.Fatal(err)
	}
	secondPrepare := PrepareOwnershipHandoffInput{
		OperationID: testutil.OrganizationID("returning-operation"), OrganizationID: account.OrganizationID,
		SourceUserID: firstPrepare.TargetUserID, TargetUserID: firstPrepare.SourceUserID,
		OwnershipVersion: firstScope.OwnershipVersion + 1, Cutoff: firstFinalize.CommittedAt.Add(time.Microsecond),
	}
	if _, err := env.store.PrepareOwnershipHandoff(ctx, secondPrepare); err != nil {
		t.Fatal(err)
	}
	secondScope := HandoffScope{OrganizationID: account.OrganizationID, OperationID: secondPrepare.OperationID, OwnershipVersion: secondPrepare.OwnershipVersion}
	secondGrant := authorizeFixtureHandoff(t, env, secondPrepare, secondScope)
	secondFinalize := finalizeFixtureInput(secondScope, secondPrepare.TargetUserID, secondGrant)
	if _, err := env.store.FinalizeOwnershipHandoff(ctx, secondFinalize); err != nil {
		t.Fatal(err)
	}
	if _, err := New(env.db).FinalizeOwnershipHandoff(ctx, firstFinalize); err != nil {
		t.Fatalf("old immutable finalization acknowledgment: %v", err)
	}
	var currentOwner string
	var version int64
	var returningPeriods, total int
	if err := env.db.QueryRow(ctx, `SELECT owner_user_id::text,ownership_version,
		(SELECT count(*) FROM billing_responsibility_periods WHERE account_id=$1 AND owner_user_id=$2),
		(SELECT count(*) FROM billing_responsibility_periods WHERE account_id=$1)
		FROM billing_responsibility_periods WHERE account_id=$1 AND effective_until IS NULL`, account.ID, firstPrepare.SourceUserID).
		Scan(&currentOwner, &version, &returningPeriods, &total); err != nil {
		t.Fatal(err)
	}
	if currentOwner != firstPrepare.SourceUserID || version != firstScope.OwnershipVersion+2 || returningPeriods != 2 || total != 3 {
		t.Fatalf("returning responsibility history: owner=%s version=%d own=%d total=%d", currentOwner, version, returningPeriods, total)
	}
}

func TestHandoffAbortOfCommitGrantKeepsBarrierUntilReleaseAndNeverRestoresRevocation(t *testing.T) {
	env := newPaymentIntegrationEnv(t)
	fixture := createPaymentFixture(t, env, "abort-grant", 20000, 10000, 50000)
	ctx := context.Background()
	prepare := handoffInput(t, env, fixture.account)
	if _, err := env.store.PrepareOwnershipHandoff(ctx, prepare); err != nil {
		t.Fatal(err)
	}
	scope := HandoffScope{OrganizationID: prepare.OrganizationID, OperationID: prepare.OperationID, OwnershipVersion: prepare.OwnershipVersion}
	grant := authorizeFixtureHandoff(t, env, prepare, scope)
	in := BeginHandoffAbortInput{Scope: scope, CancellationID: testutil.OrganizationID("abort-grant-cancel"), AMCancellationSHA256: strings.Repeat("a", 64)}
	if _, err := env.store.BeginOwnershipHandoffAbort(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("grant cancellation without bound authorization: %v", err)
	}
	in.AuthorizationID = grant.AuthorizationID
	if _, err := env.store.BeginOwnershipHandoffAbort(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PostLedgerEntry(ctx, debitInput(fixture.account.ID, "abort-pending-debit", 1, testTime(14, 0))); !errors.Is(err, ErrHandoffFenced) {
		t.Fatalf("abort pending released commit barrier: %v", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE payment_consents SET revoked_at=now(),revocation_reason='external_revocation' WHERE account_id=$1`, fixture.account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE payment_methods SET status='revoked' WHERE account_id=$1`, fixture.account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE auto_topup_policies SET enabled=false WHERE account_id=$1`, fixture.account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CompleteOwnershipHandoffAbort(ctx, CompleteHandoffAbortInput{Scope: scope, CancellationID: in.CancellationID, HoldReleaseSHA256: strings.Repeat("b", 64)}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AuthorizeHandoffCommit(ctx, AuthorizeHandoffCommitInput{Scope: scope, AuthorizationID: grant.AuthorizationID, SnapshotVersion: grant.SnapshotVersion}); !errors.Is(err, ErrConflict) {
		t.Fatalf("canceled grant revived: %v", err)
	}
	if _, err := env.store.PostLedgerEntry(ctx, debitInput(fixture.account.ID, "after-abort-debit", 1, testTime(15, 0))); err != nil {
		t.Fatal(err)
	}
	method, err := env.store.GetPaymentMethod(ctx, fixture.account.ID, fixture.method.ID)
	if err != nil || method.Status != payment.PaymentMethodStatusRevoked {
		t.Fatalf("revoked method restored: %+v err=%v", method, err)
	}
	policy, err := env.store.GetAutoTopUpPolicy(ctx, fixture.account.ID)
	if err != nil || policy.Enabled {
		t.Fatalf("disabled policy restored: %+v err=%v", policy, err)
	}
}
