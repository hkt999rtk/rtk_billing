package rawretention

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"strconv"
	"time"
)

const completionDomain = "rtk-billing-backup-verification-v1\x00"
const rangeDomain = "rtk-billing-backup-range-verification-v1\x00"

// These v1 DTOs match Logger billingarchive. Billing verifies the exact signed
// bytes, not a caller's reconstructed JSON or a self-asserted complete flag.
type VerificationPayload struct {
	Version               int       `json:"version"`
	Verdict               string    `json:"verdict"`
	VerifierKeyID         string    `json:"verifier_key_id"`
	Environment           string    `json:"environment"`
	Stack                 string    `json:"stack"`
	StoreID               string    `json:"store_id"`
	SetID                 string    `json:"set_id"`
	ManifestSHA256        string    `json:"manifest_sha256"`
	ObjectListSHA256      string    `json:"object_list_sha256"`
	HighWater             uint64    `json:"high_water,string"`
	FromSequence          uint64    `json:"from_sequence,string"`
	ThroughSequence       uint64    `json:"through_sequence,string"`
	RecordCount           uint64    `json:"record_count,string"`
	MaxReceivedAt         time.Time `json:"max_received_at"`
	RecordsBindingsSHA256 string    `json:"records_bindings_sha256"`
	EncryptionKeyID       string    `json:"encryption_key_id"`
	RecipientFingerprints []string  `json:"recipient_fingerprints"`
	VerifiedAt            time.Time `json:"verified_at"`
	PolicyVersion         string    `json:"policy_version"`
	VerifierVersion       string    `json:"verifier_version"`
}

type RangeProof struct {
	Version               int       `json:"version"`
	Purpose               string    `json:"purpose"`
	VerifierKeyID         string    `json:"verifier_key_id"`
	Environment           string    `json:"environment"`
	Stack                 string    `json:"stack"`
	StoreID               string    `json:"store_id"`
	SetID                 string    `json:"set_id"`
	ManifestSHA256        string    `json:"manifest_sha256"`
	HighWater             uint64    `json:"high_water,string"`
	FromSequence          uint64    `json:"from_sequence,string"`
	ThroughSequence       uint64    `json:"through_sequence,string"`
	RecordCount           uint64    `json:"record_count,string"`
	MaxReceivedAt         time.Time `json:"max_received_at"`
	RecordsBindingsSHA256 string    `json:"records_bindings_sha256"`
	VerifiedAt            time.Time `json:"verified_at"`
	PolicyVersion         string    `json:"policy_version"`
	VerifierVersion       string    `json:"verifier_version"`
}

func verifySigned(c SignedCompletion, keys map[string]ed25519.PublicKey, domain string, out any) error {
	key := keys[c.VerifierKeyID]
	if !identityPattern.MatchString(c.VerifierKeyID) || len(key) != ed25519.PublicKeySize || len(c.PayloadB64) > 24<<10 {
		return ErrBlocked
	}
	raw, err := base64.StdEncoding.DecodeString(c.PayloadB64)
	if err != nil || len(raw) > 16<<10 {
		return ErrBlocked
	}
	signature, err := base64.StdEncoding.DecodeString(c.SignatureB64)
	if err != nil || !ed25519.Verify(key, append([]byte(domain), raw...), signature) {
		return ErrBlocked
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrBlocked
	}
	return nil
}

func verifyArchive(plan Plan, policy Policy, keys map[string]ed25519.PublicKey, now time.Time) error {
	var archive VerificationPayload
	var subset RangeProof
	if err := verifySigned(plan.ArchiveCompletion, keys, completionDomain, &archive); err != nil {
		return err
	}
	if err := verifySigned(plan.RangeProof, keys, rangeDomain, &subset); err != nil {
		return err
	}
	version := strconv.FormatInt(policy.Version, 10)
	if archive.Version != 1 || archive.Verdict != "verified" || archive.VerifierKeyID != plan.ArchiveCompletion.VerifierKeyID ||
		archive.Environment != plan.Environment || archive.StoreID != plan.StoreID || archive.ManifestSHA256 != plan.ArchiveManifestSHA256 ||
		!identityPattern.MatchString(archive.Stack) || archive.SetID != plan.SetID || !validRange(archive.FromSequence, archive.ThroughSequence) ||
		archive.FromSequence > plan.FromSequence || archive.ThroughSequence < plan.ThroughSequence || archive.HighWater < archive.ThroughSequence ||
		archive.RecordCount != archive.ThroughSequence-archive.FromSequence+1 || !digestPattern.MatchString(archive.ObjectListSHA256) ||
		!digestPattern.MatchString(archive.RecordsBindingsSHA256) || !identityPattern.MatchString(archive.EncryptionKeyID) ||
		len(archive.RecipientFingerprints) == 0 || archive.PolicyVersion != "billing-raw-v1" || archive.VerifierVersion == "" ||
		archive.VerifiedAt.IsZero() || archive.VerifiedAt.After(now.Add(30*time.Second)) || archive.MaxReceivedAt.IsZero() || archive.MaxReceivedAt.After(archive.VerifiedAt) {
		return ErrBlocked
	}
	for _, fingerprint := range archive.RecipientFingerprints {
		if !digestPattern.MatchString(fingerprint) {
			return ErrBlocked
		}
	}
	if subset.Version != 1 || subset.Purpose != "billing-raw-reconciliation" || subset.VerifierKeyID != plan.RangeProof.VerifierKeyID ||
		subset.Environment != archive.Environment || subset.StoreID != archive.StoreID || subset.Stack != archive.Stack || subset.SetID != archive.SetID ||
		subset.ManifestSHA256 != archive.ManifestSHA256 || subset.HighWater != archive.HighWater ||
		subset.FromSequence != plan.FromSequence || subset.ThroughSequence != plan.ThroughSequence || subset.RecordCount != uint64(len(plan.Records)) ||
		subset.RecordsBindingsSHA256 != digest(plan.Records) || subset.PolicyVersion != version || subset.VerifierVersion == "" ||
		subset.VerifiedAt.After(now.Add(30*time.Second)) || !subset.VerifiedAt.After(now.Add(-5*time.Minute)) ||
		subset.MaxReceivedAt.IsZero() || subset.MaxReceivedAt.After(subset.VerifiedAt) || subset.MaxReceivedAt.After(archive.MaxReceivedAt) ||
		subset.MaxReceivedAt.After(now.Add(-time.Duration(policy.HotRetentionDays)*24*time.Hour)) {
		return ErrBlocked
	}
	return nil
}
