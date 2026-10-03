package rawretention

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func signedFor(t *testing.T, value any, domain string, key ed25519.PrivateKey) SignedCompletion {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return SignedCompletion{"verifier-1", base64.StdEncoding.EncodeToString(raw), base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte(domain), raw...)))}
}

func evidenceFixture(t *testing.T) (Policy, Plan, VerificationPayload, RangeProof, ed25519.PrivateKey, map[string]ed25519.PublicKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	scope := Scope{"staging", strings.Repeat("a", 32)}
	policy := Policy{"raw-billing", 1, scope, 90, []string{"video-cloud-mqttusage/staging"}, "financial-approval"}
	plan := Plan{OperationID: "retire-1", PolicyID: policy.PolicyID, PolicyVersion: 1, ClearanceID: "clearance-1", Scope: scope, FromSequence: 1, ThroughSequence: 2, SetID: "archive-1", ArchiveManifestSHA256: strings.Repeat("b", 64), Records: []RecordBinding{{1, strings.Repeat("c", 64), "usage-1", strings.Repeat("d", 64)}, {2, strings.Repeat("e", 64), "usage-2", strings.Repeat("f", 64)}}}
	archive := VerificationPayload{Version: 1, Verdict: "verified", VerifierKeyID: "verifier-1", Environment: scope.Environment, Stack: "staging", StoreID: scope.StoreID, SetID: plan.SetID, ManifestSHA256: plan.ArchiveManifestSHA256, ObjectListSHA256: strings.Repeat("1", 64), HighWater: 2, FromSequence: 1, ThroughSequence: 2, RecordCount: 2, MaxReceivedAt: now.Add(-91 * 24 * time.Hour), RecordsBindingsSHA256: digest(plan.Records), EncryptionKeyID: "age-key-1", RecipientFingerprints: []string{strings.Repeat("2", 64)}, VerifiedAt: now.Add(-time.Hour), PolicyVersion: "billing-raw-v1", VerifierVersion: "test-verifier"}
	subset := RangeProof{Version: 1, Purpose: "billing-raw-reconciliation", VerifierKeyID: "verifier-1", Environment: scope.Environment, Stack: archive.Stack, StoreID: scope.StoreID, SetID: plan.SetID, ManifestSHA256: plan.ArchiveManifestSHA256, HighWater: 2, FromSequence: 1, ThroughSequence: 2, RecordCount: 2, MaxReceivedAt: archive.MaxReceivedAt, RecordsBindingsSHA256: digest(plan.Records), VerifiedAt: now, PolicyVersion: "1", VerifierVersion: "test-verifier"}
	plan.ArchiveCompletion = signedFor(t, archive, completionDomain, private)
	plan.RangeProof = signedFor(t, subset, rangeDomain, private)
	plan.PlanSHA256 = PlanDigest(plan)
	return policy, plan, archive, subset, private, map[string]ed25519.PublicKey{"verifier-1": public}
}

func TestArchiveVerificationBindsAgeSubsetAndTrustedKey(t *testing.T) {
	policy, plan, archive, subset, private, keys := evidenceFixture(t)
	now := time.Now().UTC()
	if !plan.valid() || verifyArchive(plan, policy, keys, now) != nil {
		t.Fatal("valid signed archive/subrange rejected")
	}
	for _, tc := range []struct {
		name   string
		change func(*VerificationPayload, *RangeProof, *Plan)
	}{
		{"young-receipt", func(a *VerificationPayload, r *RangeProof, p *Plan) {
			a.MaxReceivedAt = now.Add(-time.Hour)
			r.MaxReceivedAt = a.MaxReceivedAt
		}},
		{"wrong-record-bindings", func(_ *VerificationPayload, r *RangeProof, _ *Plan) {
			r.RecordsBindingsSHA256 = strings.Repeat("9", 64)
		}},
		{"wrong-store", func(_ *VerificationPayload, r *RangeProof, _ *Plan) { r.StoreID = strings.Repeat("9", 32) }},
		{"stale-subrange", func(_ *VerificationPayload, r *RangeProof, _ *Plan) { r.VerifiedAt = now.Add(-6 * time.Minute) }},
		{"foreign-policy", func(_ *VerificationPayload, r *RangeProof, _ *Plan) { r.PolicyVersion = "2" }},
		{"wrong-set", func(_ *VerificationPayload, r *RangeProof, _ *Plan) { r.SetID = "different-archive" }},
		{"wrong-count", func(_ *VerificationPayload, r *RangeProof, _ *Plan) { r.RecordCount = 1 }},
		{"not-verified", func(a *VerificationPayload, _ *RangeProof, _ *Plan) { a.Verdict = "uploaded" }},
		{"unknown-verification-format", func(a *VerificationPayload, _ *RangeProof, _ *Plan) { a.PolicyVersion = "1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, r, p := archive, subset, plan
			tc.change(&a, &r, &p)
			p.ArchiveCompletion = signedFor(t, a, completionDomain, private)
			p.RangeProof = signedFor(t, r, rangeDomain, private)
			if verifyArchive(p, policy, keys, now) == nil {
				t.Fatal("unsafe evidence accepted")
			}
		})
	}
	tampered := plan
	tampered.RangeProof.SignatureB64 = base64.StdEncoding.EncodeToString(make([]byte, 64))
	if verifyArchive(tampered, policy, keys, now) == nil {
		t.Fatal("forged signature accepted")
	}
	if verifyArchive(plan, policy, map[string]ed25519.PublicKey{}, now) == nil {
		t.Fatal("unregistered verifier accepted")
	}
	// Full archives may be larger than one RPC. Only the fresh exact subset's
	// receipt age is a retirement gate; younger unrelated full-set records do not
	// authorize a younger candidate and do not force re-signing immutable sets.
	archive.ThroughSequence = 5000
	archive.HighWater = 5000
	archive.RecordCount = 5000
	archive.MaxReceivedAt = now.Add(-time.Hour)
	archive.VerifiedAt = now
	subset.HighWater = 5000
	plan.ArchiveCompletion = signedFor(t, archive, completionDomain, private)
	plan.RangeProof = signedFor(t, subset, rangeDomain, private)
	if verifyArchive(plan, policy, keys, now) != nil {
		t.Fatal("valid bounded subset of larger immutable archive rejected")
	}
}

func TestPlanDigestBindsStableIntentNotFreshVerification(t *testing.T) {
	_, p, _, _, _, _ := evidenceFixture(t)
	before := PlanDigest(p)
	p.RangeProof.SignatureB64 = "fresh-proof-signature"
	if PlanDigest(p) != before {
		t.Fatal("proof freshness changed intent")
	}
	p.ClearanceID = "another-clearance"
	if PlanDigest(p) == before {
		t.Fatal("clearance unbound")
	}
	p.PlanSHA256 = before
	if p.valid() {
		t.Fatal("caller-supplied hash is not validated")
	}
	if (Policy{PolicyID: "p", Version: 1, Scope: p.Scope, HotRetentionDays: 89, RequiredConsumers: []string{"consumer"}, FinancialApprovalRef: "approved"}).valid() {
		t.Fatal("under-90-day policy accepted")
	}
}

func TestConsumerProofExactBindingAndProgress(t *testing.T) {
	_, plan, _, _, _, _ := evidenceFixture(t)
	request := plan.reconcileRequest()
	now := time.Now().UTC()
	p := ConsumerProof{ConsumerID: "video-cloud-mqttusage/staging", Environment: plan.Environment, StoreID: plan.StoreID, FromSequence: 1, ThroughSequence: 2, ArchiveManifestSHA256: plan.ArchiveManifestSHA256, RequestSHA256: digest(request), RecordCount: 2, FactCount: 4, LastSequence: 2, HighWater: 3, VerifiedAt: now}
	p.ProofSHA256 = digest(p)
	if !validProof(p, p.ConsumerID, request, now) {
		t.Fatal("valid proof rejected")
	}
	p.LastSequence = 1
	p.ProofSHA256 = ""
	p.ProofSHA256 = digest(p)
	if validProof(p, p.ConsumerID, request, now) {
		t.Fatal("behind cursor accepted")
	}
}

func TestConsumerWireHashMatchesVideoCloudVector(t *testing.T) {
	r := ReconcileRequest{Environment: "staging", StoreID: strings.Repeat("a", 32), FromSequence: 9007199254740993, ThroughSequence: 9007199254740993,
		ArchiveManifestSHA256: strings.Repeat("b", 64), Records: []RecordBinding{{9007199254740993, strings.Repeat("c", 64), "usage-vector", strings.Repeat("d", 64)}}}
	if sha := digest(r); sha != "6f6204c7c38687b606e9f35e5b962b8c3780725f34787c70ff4add8d5dc6d5f5" {
		t.Fatal("consumer request wire hash drift", sha)
	}
	p := ConsumerProof{ConsumerID: "video-cloud-mqttusage/staging", Environment: r.Environment, StoreID: r.StoreID, FromSequence: r.FromSequence, ThroughSequence: r.ThroughSequence,
		ArchiveManifestSHA256: r.ArchiveManifestSHA256, RequestSHA256: digest(r), RecordCount: 1, FactCount: 2, LastSequence: r.ThroughSequence, HighWater: r.ThroughSequence, VerifiedAt: time.Date(2026, 10, 3, 1, 2, 3, 123456000, time.UTC)}
	if sha := digest(p); sha != "d3f8c6535b1000ae5b77c6b43a8b85c0e1f55d1a0ffe9de8d6e857d45e5d5824" {
		t.Fatal("consumer proof wire hash drift", sha)
	}
}

func TestRetirementPlanAndLoggerTerminalGoldenVectors(t *testing.T) {
	p := Plan{OperationID: "operation", PolicyID: "policy", PolicyVersion: 1, ClearanceID: "clearance", Scope: Scope{"dev", strings.Repeat("a", 32)}, FromSequence: 1, ThroughSequence: 1,
		SetID: "set", ArchiveManifestSHA256: strings.Repeat("a", 64), Records: []RecordBinding{{1, strings.Repeat("b", 64), "usage", strings.Repeat("c", 64)}}}
	sha := PlanDigest(p)
	if sha != "d97b684a97aad4e9b1ef6f7ba57e83436bb39e850a10cbac79385db0d1e76ebb" {
		t.Fatal("plan golden", sha)
	}
	receipt := TerminalReceipt{OperationID: p.OperationID, Scope: p.Scope, FromSequence: 1, ThroughSequence: 1, PlanSHA256: sha, Status: "completed",
		CompletedAt: time.Date(2026, 10, 3, 1, 2, 3, 123456000, time.UTC), RetiredThrough: 1, SetID: p.SetID}
	if terminal := digest(receipt); terminal != "131cca1d26463c3412d032b12e8041e1e2286aaff53dc7a84498af428e377fe6" {
		t.Fatal("terminal golden", terminal)
	}
}
