package billingstore

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

type otaGrantVerifierFunc func(context.Context, string, string, billing.OTAGrantEvidence) error

type otaTierVerifierFunc func(context.Context, string, time.Time, time.Time) error

func (f otaTierVerifierFunc) VerifyCommercialMonth(ctx context.Context, cloud string, start, end time.Time) error {
	return f(ctx, cloud, start, end)
}

func (f otaGrantVerifierFunc) VerifyOTAGrant(ctx context.Context, cloud, product string, grant billing.OTAGrantEvidence) error {
	return f(ctx, cloud, product, grant)
}

func TestPricedOTAFactsRequireHistoricalGrantAndHoldPositiveStorage(t *testing.T) {
	grant := &billing.OTAGrantEvidence{ProductServiceRevision: 2,
		ServiceGrantSHA256: strings.Repeat("a", 64), AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	fact := billing.UsageFact{OrganizationID: "cloud", ProductID: "product", ServiceCode: billing.ServiceOTA,
		MetricCode: billing.MetricOTADeviceTask, OTAGrant: grant}
	store := &Store{}
	if err := store.VerifyOTAFactGrants(context.Background(), []billing.UsageFact{fact}); !errors.Is(err, ErrIncomplete) {
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
	if err := store.VerifyOTAFactGrants(context.Background(), []billing.UsageFact{fact, fact}); err != nil || calls != 1 {
		t.Fatalf("duplicate historical lookup: calls=%d err=%v", calls, err)
	}
	fact.OTAGrant = nil
	if err := store.VerifyOTAFactGrants(context.Background(), []billing.UsageFact{fact}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("missing original witness accepted: %v", err)
	}
	fact.MetricCode = billing.MetricOTAArtifactStorageGiBMonth
	fact.Quantity = 1
	if err := store.VerifyOTAFactGrants(context.Background(), []billing.UsageFact{fact}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("unverified positive storage accepted: %v", err)
	}
	fact.Quantity = 0
	if err := store.VerifyOTAFactGrants(context.Background(), []billing.UsageFact{fact}); err != nil {
		t.Fatalf("zero storage blocked: %v", err)
	}
	store.SetOTAGrantVerifier(otaGrantVerifierFunc(func(context.Context, string, string, billing.OTAGrantEvidence) error {
		return errors.New("history disagrees")
	}))
	fact.MetricCode, fact.Quantity, fact.OTAGrant = billing.MetricOTADeviceTask, 1, grant
	if err := store.VerifyOTAFactGrants(context.Background(), []billing.UsageFact{fact}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("mismatched historical grant accepted: %v", err)
	}
}

func TestPricedOTAStorageChecksEveryObjectAndProductRounding(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	denominator := new(big.Int).Mul(big.NewInt(end.Sub(start).Microseconds()), big.NewInt(1<<30))
	half := new(big.Int).Rsh(denominator, 1).String()
	grant := &billing.OTAGrantEvidence{ProductServiceRevision: 2,
		ServiceGrantSHA256: strings.Repeat("a", 64), AuthorizedAt: start.Add(-time.Hour)}
	first := billing.UsageFact{OrganizationID: "cloud", ProductID: "product", ServiceCode: billing.ServiceOTA,
		MetricCode: billing.MetricOTAArtifactStorageGiBMonth, Quantity: 500_000_000,
		WindowStart: start, WindowEnd: end, OTAGrant: grant,
		OTAStorageObject: &billing.OTAStorageObjectEvidence{ObjectSHA256: strings.Repeat("b", 64), ByteMicroseconds: half}}
	second := first
	second.OTAStorageObject = &billing.OTAStorageObjectEvidence{ObjectSHA256: strings.Repeat("c", 64), ByteMicroseconds: half}
	store := &Store{}
	store.SetOTAGrantVerifier(otaGrantVerifierFunc(func(context.Context, string, string, billing.OTAGrantEvidence) error { return nil }))
	if err := store.VerifyOTAFactGrants(context.Background(), []billing.UsageFact{first, second}); err != nil {
		t.Fatalf("matching per-object byte-time was held: %v", err)
	}
	if err := store.VerifyOTAFactGrants(context.Background(), []billing.UsageFact{first, first}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("duplicate physical object accepted: %v", err)
	}
	second.Quantity++
	if err := store.VerifyOTAFactGrants(context.Background(), []billing.UsageFact{first, second}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("storage quantity above exact byte-time accepted: %v", err)
	}
	second.Quantity--
	second.OTAGrant = nil
	if err := store.VerifyOTAFactGrants(context.Background(), []billing.UsageFact{first, second}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("object without original Product grant accepted: %v", err)
	}
}
