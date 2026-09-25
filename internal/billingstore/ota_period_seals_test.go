package billingstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

func testOTAPeriodSeal(issuer string) OTAPeriodSeal {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seal := OTAPeriodSeal{
		OrganizationID: "11111111-1111-4111-8111-111111111111",
		PeriodStart:    start, PeriodEnd: start.AddDate(0, 1, 0), IssuerKind: issuer,
		SealID: "22222222-2222-4222-8222-222222222222", SourceHighWater: json.RawMessage(`{"sequence":0}`),
		ProductIDs: []string{}, MetricCounts: map[string]int64{}, SourceSHA256: hex.EncodeToString(sha256.New().Sum(nil)),
		SealedAt: start.AddDate(0, 1, 0),
	}
	if issuer == OTAIssuerProducer {
		digest := sha256.Sum256([]byte("[]"))
		value := hex.EncodeToString(digest[:])
		seal.FactSetSHA256 = &value
		for _, metric := range otaMetricCodes {
			seal.MetricCounts[metric] = 0
		}
	}
	return seal
}

func TestOTAPeriodSealCanonicalReplayAndValidation(t *testing.T) {
	for _, issuer := range []string{OTAIssuerPlatformGrants, OTAIssuerProducer} {
		seal := testOTAPeriodSeal(issuer)
		_, digest, err := canonicalOTAPeriodSeal(seal)
		if err != nil || len(digest) != 64 {
			t.Fatalf("valid %s seal: digest=%s err=%v", issuer, digest, err)
		}
		seal.SourceHighWater = json.RawMessage(`{ "sequence" : 0 }`)
		_, replayDigest, err := canonicalOTAPeriodSeal(seal)
		if err != nil || digest != replayDigest {
			t.Fatalf("semantically identical marker changed digest: %s %s %v", digest, replayDigest, err)
		}
	}
	invalid := testOTAPeriodSeal(OTAIssuerProducer)
	invalid.MetricCounts[billing.MetricOTADeviceTask] = -1
	if _, _, err := canonicalOTAPeriodSeal(invalid); err != ErrConflict {
		t.Fatalf("negative metric count accepted: %v", err)
	}
	invalid = testOTAPeriodSeal(OTAIssuerProducer)
	invalid.ProductIDs = nil
	if _, _, err := canonicalOTAPeriodSeal(invalid); err != ErrConflict {
		t.Fatalf("missing zero-usage product set accepted: %v", err)
	}
	invalid = testOTAPeriodSeal(OTAIssuerProducer)
	invalid.PeriodEnd = invalid.PeriodEnd.Add(time.Hour)
	if _, _, err := canonicalOTAPeriodSeal(invalid); err != ErrConflict {
		t.Fatalf("non-UTC-month period accepted: %v", err)
	}
}

func TestOTAPricingRequiresExactFourMeters(t *testing.T) {
	rates := billing.ProposedOTARates()
	if enabled, complete := otaPricingComplete(rates); !enabled || !complete {
		t.Fatal("complete OTA pricing was rejected")
	}
	if enabled, complete := otaPricingComplete(rates[:3]); !enabled || complete {
		t.Fatal("partial OTA pricing was accepted")
	}
	rates[3].Unit = "writes"
	if enabled, complete := otaPricingComplete(rates); !enabled || complete {
		t.Fatal("wrong OTA rate unit was accepted")
	}
}
