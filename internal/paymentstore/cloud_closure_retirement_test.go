package paymentstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestCloudClosureRetirementIsPermanentAndAllowsFreshEvidence(t *testing.T) {
	env, _, owner := cloudPreflightFixture(t, 0)
	ctx := context.Background()
	in := PrepareCloudClosureInput{Scope: CloudClosureScope{CloudPreflightScope: owner, OperationID: testutil.OrganizationID("retirement")}, Cutoff: time.Now(), AMRequestSHA256: strings.Repeat("a", 64)}
	if _, err := env.store.PrepareCloudClosure(ctx, in); err != nil {
		t.Fatal(err)
	}
	old := CloseCloudInput{Scope: in.Scope, SettlementID: testutil.OrganizationID("old-settlement"), AMReadinessSHA256: strings.Repeat("b", 64)}
	if _, err := env.store.CloseCloud(ctx, old); !errors.Is(err, ErrCloudClosureNotReady) {
		t.Fatalf("unexpected close: %v", err)
	}
	result, err := env.store.RetireCloudClose(ctx, old)
	if err != nil || result.Outcome != "retired" || result.RetiredAt == nil || result.ReceiptSHA256 == "" {
		t.Fatalf("retire %+v %v", result, err)
	}
	replay, err := New(env.db).RetireCloudClose(ctx, old)
	if err != nil || replay.ReceiptSHA256 != result.ReceiptSHA256 || !replay.RetiredAt.Equal(*result.RetiredAt) {
		t.Fatalf("replay %+v %v", replay, err)
	}
	// Even if that previously unavailable evidence later arrives, the retired
	// command can never succeed through a delayed retry.
	evidence := closureReceipt(t, env, in.Scope, "old-settlement")
	if err := env.store.RecordCloudClosureSettlement(ctx, evidence); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.CloseCloud(ctx, old); !errors.Is(err, ErrCloudClosureCommandRetired) {
		t.Fatalf("retired command revived: %v", err)
	}
	fresh := closureReceipt(t, env, in.Scope, "new-settlement")
	if err := env.store.RecordCloudClosureSettlement(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	next := CloseCloudInput{Scope: in.Scope, SettlementID: fresh.ReceiptID, AMReadinessSHA256: strings.Repeat("c", 64)}
	ack, err := env.store.CloseCloud(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := env.store.RetireCloudClose(ctx, next)
	if err != nil || resolved.Outcome != "closed" || resolved.Acknowledgment == nil || *resolved.Acknowledgment != ack {
		t.Fatalf("closed changed to retired %+v %v", resolved, err)
	}
	if _, err := env.store.CloseCloud(ctx, old); !errors.Is(err, ErrCloudClosureCommandRetired) {
		t.Fatalf("old replay after new close: %v", err)
	}
}

func TestCloudClosureRetirementRacesCloseAtomically(t *testing.T) {
	for run := 0; run < 8; run++ {
		t.Run(string(rune('a'+run)), func(t *testing.T) {
			env, _, owner := cloudPreflightFixture(t, 0)
			ctx := context.Background()
			in := PrepareCloudClosureInput{Scope: CloudClosureScope{CloudPreflightScope: owner, OperationID: testutil.OrganizationID("race-retirement")}, Cutoff: time.Now(), AMRequestSHA256: strings.Repeat("a", 64)}
			if _, err := env.store.PrepareCloudClosure(ctx, in); err != nil {
				t.Fatal(err)
			}
			evidence := closureReceipt(t, env, in.Scope, "race-settlement")
			if err := env.store.RecordCloudClosureSettlement(ctx, evidence); err != nil {
				t.Fatal(err)
			}
			command := CloseCloudInput{Scope: in.Scope, SettlementID: evidence.ReceiptID, AMReadinessSHA256: strings.Repeat("d", 64)}
			start := make(chan struct{})
			closed := make(chan error, 1)
			retired := make(chan CloudCloseResolution, 1)
			retireErr := make(chan error, 1)
			go func() { <-start; _, err := env.store.CloseCloud(ctx, command); closed <- err }()
			go func() {
				<-start
				out, err := env.store.RetireCloudClose(ctx, command)
				retired <- out
				retireErr <- err
			}()
			close(start)
			closeErr := <-closed
			out := <-retired
			if err := <-retireErr; err != nil {
				t.Fatal(err)
			}
			if out.Outcome == "closed" {
				if closeErr != nil {
					t.Fatalf("closed race inconsistent: %v", closeErr)
				}
			} else if out.Outcome != "retired" || !errors.Is(closeErr, ErrCloudClosureCommandRetired) {
				t.Fatalf("race %+v %v", out, closeErr)
			}
			var both bool
			if err := env.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_cloud_closure_retired_commands r JOIN billing_cloud_closure_completions c USING(operation_id,request_sha256))`).Scan(&both); err != nil || both {
				t.Fatalf("contradictory outcomes %v %v", both, err)
			}
		})
	}
}

func TestCloudClosureRetirementAuditFailureRollsBack(t *testing.T) {
	env, _, owner := cloudPreflightFixture(t, 0)
	ctx := context.Background()
	in := PrepareCloudClosureInput{Scope: CloudClosureScope{CloudPreflightScope: owner, OperationID: testutil.OrganizationID("retirement-audit")}, Cutoff: time.Now(), AMRequestSHA256: strings.Repeat("a", 64)}
	if _, err := env.store.PrepareCloudClosure(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `ALTER TABLE billing_audit_events ADD CONSTRAINT test_retirement_audit CHECK(event_type<>'billing.cloud_closure.command_retired') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(context.Background(), `ALTER TABLE billing_audit_events DROP CONSTRAINT IF EXISTS test_retirement_audit`)
	})
	_, err := env.store.RetireCloudClose(ctx, CloseCloudInput{Scope: in.Scope, SettlementID: testutil.OrganizationID("audit-receipt"), AMReadinessSHA256: strings.Repeat("c", 64)})
	if err == nil {
		t.Fatal("audit failure accepted")
	}
	var count int
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM billing_cloud_closure_retired_commands`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial retirement %d %v", count, err)
	}
}
