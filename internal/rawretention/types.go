// Package rawretention is the financial authority for retiring Logger's hot
// payloads. It never deletes usage facts, invoices, archives or recovery keys.
package rawretention

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid raw retention evidence")
	ErrConflict = errors.New("raw retention immutable identity conflict")
	ErrBlocked  = errors.New("raw retention safety gate is not satisfied")
	ErrNotFound = errors.New("raw retention authority record not found")
)

var identityPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var consumerPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]{0,127}$`)
var storePattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Scope struct {
	Environment string `json:"environment"`
	StoreID     string `json:"store_id"`
}

func (s Scope) valid() bool {
	return (s.Environment == "dev" || s.Environment == "staging" || s.Environment == "prod") && storePattern.MatchString(s.StoreID)
}

type Policy struct {
	PolicyID string `json:"policy_id"`
	Version  int64  `json:"version"`
	Scope
	HotRetentionDays     int      `json:"hot_retention_days"`
	RequiredConsumers    []string `json:"required_consumers"`
	FinancialApprovalRef string   `json:"financial_approval_ref"`
}

type PolicyState struct {
	Policy
	PolicySHA256        string `json:"policy_sha256"`
	RecoveryApprovalRef string `json:"recovery_approval_ref,omitempty"`
	Active              bool   `json:"active"`
}

func (p Policy) valid() bool {
	if !p.Scope.valid() || !identityPattern.MatchString(p.PolicyID) || p.Version < 1 || p.HotRetentionDays < 90 || p.HotRetentionDays > 3650 ||
		!identityPattern.MatchString(p.FinancialApprovalRef) || len(p.RequiredConsumers) < 1 || len(p.RequiredConsumers) > 64 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range p.RequiredConsumers {
		if !consumerPattern.MatchString(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

type Period struct {
	OrganizationID         string    `json:"organization_id"`
	PeriodStart            time.Time `json:"period_start"`
	PeriodEnd              time.Time `json:"period_end"`
	SourceCheckpointSHA256 string    `json:"source_checkpoint_sha256"`
	SourceComplete         bool      `json:"source_complete"`
	Reconciled             bool      `json:"reconciled"`
}

type Clearance struct {
	ClearanceID   string `json:"clearance_id"`
	PolicyID      string `json:"policy_id"`
	PolicyVersion int64  `json:"policy_version"`
	Scope
	FromSequence         uint64   `json:"from_sequence,string"`
	ThroughSequence      uint64   `json:"through_sequence,string"`
	Periods              []Period `json:"periods"`
	FinancialApprovalRef string   `json:"financial_approval_ref"`
	ReconciliationSHA256 string   `json:"reconciliation_sha256"`
}

func (c Clearance) valid() bool {
	if !c.Scope.valid() || !validRange(c.FromSequence, c.ThroughSequence) || !identityPattern.MatchString(c.ClearanceID) ||
		!identityPattern.MatchString(c.PolicyID) || c.PolicyVersion < 1 || !identityPattern.MatchString(c.FinancialApprovalRef) ||
		!digestPattern.MatchString(c.ReconciliationSHA256) || len(c.Periods) < 1 || len(c.Periods) > 1000 {
		return false
	}
	for _, p := range c.Periods {
		if p.OrganizationID == "" || len(p.OrganizationID) > 128 || p.PeriodStart.IsZero() || !p.PeriodEnd.After(p.PeriodStart) ||
			!p.SourceComplete || !p.Reconciled || !digestPattern.MatchString(p.SourceCheckpointSHA256) {
			return false
		}
	}
	return true
}

type Hold struct {
	HoldID string `json:"hold_id"`
	Scope
	FromSequence         uint64 `json:"from_sequence,string"`
	ThroughSequence      uint64 `json:"through_sequence,string"`
	Reason               string `json:"reason"`
	FinancialApprovalRef string `json:"financial_approval_ref"`
}

type HoldState struct {
	Hold
	Status string `json:"status"`
	// All unreleased holds protect ciphertext and decryption-key dependencies,
	// including a hold queued behind an irrevocably ACTIVE hot-retirement fence.
	ProtectArchiveAndKeys bool `json:"protect_archive_and_keys"`
}

func (h Hold) valid() bool {
	return h.Scope.valid() && identityPattern.MatchString(h.HoldID) &&
		(validRange(h.FromSequence, h.ThroughSequence) || h.FromSequence == 0 && h.ThroughSequence == 0) &&
		h.Reason != "" && len(h.Reason) <= 1000 && identityPattern.MatchString(h.FinancialApprovalRef)
}

// RecordBinding is the exact request contract shared with Video Cloud. The
// archive verifier and authenticated consumer must agree on every binding.
type RecordBinding struct {
	Sequence            uint64 `json:"sequence,string"`
	LoggerContentSHA256 string `json:"logger_content_sha256"`
	UsageID             string `json:"usage_id"`
	EventSHA256         string `json:"event_sha256"`
}

type ReconcileRequest struct {
	Environment           string          `json:"environment"`
	StoreID               string          `json:"store_id"`
	FromSequence          uint64          `json:"from_sequence,string"`
	ThroughSequence       uint64          `json:"through_sequence,string"`
	ArchiveManifestSHA256 string          `json:"archive_manifest_sha256"`
	Records               []RecordBinding `json:"records"`
}

type ConsumerProof struct {
	ConsumerID            string    `json:"consumer_id"`
	Environment           string    `json:"environment"`
	StoreID               string    `json:"store_id"`
	FromSequence          uint64    `json:"from_sequence,string"`
	ThroughSequence       uint64    `json:"through_sequence,string"`
	ArchiveManifestSHA256 string    `json:"archive_manifest_sha256"`
	RequestSHA256         string    `json:"request_sha256"`
	RecordCount           int       `json:"record_count"`
	FactCount             int       `json:"fact_count"`
	LastSequence          uint64    `json:"last_sequence,string"`
	HighWater             uint64    `json:"high_water,string"`
	VerifiedAt            time.Time `json:"verified_at"`
	ProofSHA256           string    `json:"proof_sha256,omitempty"`
}

type SignedCompletion struct {
	VerifierKeyID string `json:"verifier_key_id"`
	PayloadB64    string `json:"payload_b64"`
	SignatureB64  string `json:"signature_b64"`
}

type Plan struct {
	OperationID   string `json:"operation_id"`
	PolicyID      string `json:"policy_id"`
	PolicyVersion int64  `json:"policy_version"`
	ClearanceID   string `json:"clearance_id"`
	Scope
	FromSequence          uint64           `json:"from_sequence,string"`
	ThroughSequence       uint64           `json:"through_sequence,string"`
	PlanSHA256            string           `json:"plan_sha256"`
	SetID                 string           `json:"set_id"`
	ArchiveManifestSHA256 string           `json:"archive_manifest_sha256"`
	ArchiveCompletion     SignedCompletion `json:"archive_completion"`
	RangeProof            SignedCompletion `json:"range_proof"`
	Records               []RecordBinding  `json:"records"`
}

func (p Plan) valid() bool {
	if !p.Scope.valid() || !identityPattern.MatchString(p.OperationID) || !identityPattern.MatchString(p.PolicyID) || p.PolicyVersion < 1 ||
		!identityPattern.MatchString(p.ClearanceID) || !validRange(p.FromSequence, p.ThroughSequence) ||
		!digestPattern.MatchString(p.PlanSHA256) || p.PlanSHA256 != PlanDigest(p) || !identityPattern.MatchString(p.SetID) || !digestPattern.MatchString(p.ArchiveManifestSHA256) ||
		len(p.Records) < 1 || len(p.Records) > 1000 || uint64(len(p.Records)) != p.ThroughSequence-p.FromSequence+1 {
		return false
	}
	for i, r := range p.Records {
		if r.Sequence != p.FromSequence+uint64(i) || !digestPattern.MatchString(r.LoggerContentSHA256) ||
			!digestPattern.MatchString(r.EventSHA256) || r.UsageID == "" || len(r.UsageID) > 4096 {
			return false
		}
	}
	return true
}

type TerminalReceipt struct {
	OperationID string `json:"operation_id"`
	Scope
	FromSequence    uint64    `json:"from_sequence,string"`
	ThroughSequence uint64    `json:"through_sequence,string"`
	PlanSHA256      string    `json:"plan_sha256"`
	Status          string    `json:"status"`
	ReceiptSHA256   string    `json:"receipt_sha256"`
	CompletedAt     time.Time `json:"completed_at"`
	RetiredThrough  uint64    `json:"retired_through,string"`
	SetID           string    `json:"set_id"`
}

type Operation struct {
	Plan
	Status          string           `json:"status"`
	DecisionOrigin  string           `json:"decision_origin"`
	ConsumerProofs  []ConsumerProof  `json:"consumer_proofs"`
	CreatedAt       time.Time        `json:"created_at"`
	ResolvedAt      *time.Time       `json:"resolved_at,omitempty"`
	TerminalReceipt *TerminalReceipt `json:"terminal_receipt,omitempty"`
}

func validRange(first, last uint64) bool { return first > 0 && last >= first && last <= math.MaxInt64 }
func digest(value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p Plan) reconcileRequest() ReconcileRequest {
	return ReconcileRequest{p.Environment, p.StoreID, p.FromSequence, p.ThroughSequence, p.ArchiveManifestSHA256, p.Records}
}

// PlanDigest binds the stable retirement intent, not a verifier's fresh
// signature/timestamp. Field order and decimal-string sequences are part of v1.
func PlanDigest(p Plan) string {
	core := struct {
		OperationID           string `json:"operation_id"`
		PolicyID              string `json:"policy_id"`
		PolicyVersion         int64  `json:"policy_version"`
		ClearanceID           string `json:"clearance_id"`
		Environment           string `json:"environment"`
		StoreID               string `json:"store_id"`
		FromSequence          uint64 `json:"from_sequence,string"`
		ThroughSequence       uint64 `json:"through_sequence,string"`
		SetID                 string `json:"set_id"`
		ArchiveManifestSHA256 string `json:"archive_manifest_sha256"`
		RecordsBindingsSHA256 string `json:"records_bindings_sha256"`
	}{p.OperationID, p.PolicyID, p.PolicyVersion, p.ClearanceID, p.Environment, p.StoreID, p.FromSequence, p.ThroughSequence, p.SetID, p.ArchiveManifestSHA256, digest(p.Records)}
	raw, _ := json.Marshal(core)
	sum := sha256.Sum256(append([]byte("rtk-billing-raw-retirement-plan-v1\x00"), raw...))
	return hex.EncodeToString(sum[:])
}

func validProof(p ConsumerProof, expectedID string, request ReconcileRequest, now time.Time) bool {
	claimed := p.ProofSHA256
	p.ProofSHA256 = ""
	return p.ConsumerID == expectedID && p.Environment == request.Environment && p.StoreID == request.StoreID &&
		p.FromSequence == request.FromSequence && p.ThroughSequence == request.ThroughSequence &&
		p.ArchiveManifestSHA256 == request.ArchiveManifestSHA256 && p.RequestSHA256 == digest(request) &&
		p.RecordCount == len(request.Records) && p.FactCount >= p.RecordCount && p.LastSequence >= p.ThroughSequence && p.HighWater >= p.LastSequence &&
		!p.VerifiedAt.After(now.Add(30*time.Second)) && p.VerifiedAt.After(now.Add(-5*time.Minute)) && claimed == digest(p)
}
