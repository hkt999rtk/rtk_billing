package billingstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

type otaGrantVerifierFunc func(context.Context, string, string, billing.OTAGrantEvidence) error

func (f otaGrantVerifierFunc) VerifyOTAGrant(ctx context.Context, cloud, product string, grant billing.OTAGrantEvidence) error {
	return f(ctx, cloud, product, grant)
}

func TestPricedOTAFactsRequireHistoricalGrantAndHoldPositiveStorage(t *testing.T) {
	grant := &billing.OTAGrantEvidence{ProductServiceRevision: 2,
		ServiceGrantSHA256: strings.Repeat("a", 64), AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	fact := billing.UsageFact{OrganizationID: "cloud", ProductID: "product", ServiceCode: billing.ServiceOTA,
		MetricCode: billing.MetricOTADeviceTask, OTAGrant: grant}
	store := &Store{}
	if err := store.verifyOTAFactGrants(context.Background(), []billing.UsageFact{fact}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("missing verifier accepted: %v", err)
	}
	calls := 0
	store.SetOTAGrantVerifier(otaGrantVerifierFunc(func(_ context.Context, cloud, product string, observed billing.OTAGrantEvidence) error {
		calls++
		if cloud != fact.OrganizationID || product != fact.ProductID || observed != *grant {
			t.Fatalf("wrong historical lookup scope: %s %s %+v", cloud, product, observed)
		}
		return nil
	}))
	if err := store.verifyOTAFactGrants(context.Background(), []billing.UsageFact{fact, fact}); err != nil || calls != 1 {
		t.Fatalf("duplicate historical lookup: calls=%d err=%v", calls, err)
	}
	fact.OTAGrant = nil
	if err := store.verifyOTAFactGrants(context.Background(), []billing.UsageFact{fact}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("missing original witness accepted: %v", err)
	}
	fact.MetricCode = billing.MetricOTAArtifactStorageGiBMonth
	fact.Quantity = 1
	if err := store.verifyOTAFactGrants(context.Background(), []billing.UsageFact{fact}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("unverified positive storage accepted: %v", err)
	}
	fact.Quantity = 0
	if err := store.verifyOTAFactGrants(context.Background(), []billing.UsageFact{fact}); err != nil {
		t.Fatalf("zero storage blocked: %v", err)
	}
	store.SetOTAGrantVerifier(otaGrantVerifierFunc(func(context.Context, string, string, billing.OTAGrantEvidence) error {
		return errors.New("history disagrees")
	}))
	fact.MetricCode, fact.Quantity, fact.OTAGrant = billing.MetricOTADeviceTask, 1, grant
	if err := store.verifyOTAFactGrants(context.Background(), []billing.UsageFact{fact}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("mismatched historical grant accepted: %v", err)
	}
}
