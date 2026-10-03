package rawretention

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/jackc/pgx/v5"
)

type Store struct {
	db          database.Connection
	evidence    *EvidenceClient
	keys        map[string]ed25519.PublicKey
	environment string
}

func New(db database.Connection, evidence *EvidenceClient, keys map[string]ed25519.PublicKey, environment string) (*Store, error) {
	if db == nil || !(Scope{Environment: environment, StoreID: "00000000000000000000000000000000"}).valid() || evidence.Validate() != nil || len(keys) == 0 {
		return nil, ErrInvalid
	}
	approved := make(map[string]ed25519.PublicKey, len(keys))
	for id, key := range keys {
		if !identityPattern.MatchString(id) || len(key) != ed25519.PublicKeySize {
			return nil, ErrInvalid
		}
		approved[id] = append(ed25519.PublicKey(nil), key...)
	}
	return &Store{db: db, evidence: evidence, keys: approved, environment: environment}, nil
}

func (s *Store) scopeValid(scope Scope) bool {
	return scope.valid() && scope.Environment == s.environment
}
func mapMissing(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func lock(ctx context.Context, tx pgx.Tx, scope Scope) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "billing-raw-retention:"+scope.Environment+":"+scope.StoreID)
	return err
}
func audit(ctx context.Context, tx pgx.Tx, scope Scope, id, action, sha string) error {
	_, err := tx.Exec(ctx, `INSERT INTO billing_raw_retention_audit(environment,store_id,subject_id,action,binding_sha256) VALUES($1,$2,$3,$4,$5)`, scope.Environment, scope.StoreID, id, action, sha)
	return err
}
func marshal(value any) []byte { raw, _ := json.Marshal(value); return raw }

func readPolicy(ctx context.Context, db database.Connection, id string, version int64) (PolicyState, error) {
	var out PolicyState
	var raw []byte
	err := db.QueryRow(ctx, `SELECT specification,specification_sha256,COALESCE(recovery_approval_ref,''),active FROM billing_raw_retention_policies WHERE policy_id=$1 AND version=$2`, id, version).Scan(&raw, &out.PolicySHA256, &out.RecoveryApprovalRef, &out.Active)
	if err != nil {
		return out, mapMissing(err)
	}
	if json.Unmarshal(raw, &out.Policy) != nil || !out.Policy.valid() || digest(out.Policy) != out.PolicySHA256 {
		return out, ErrConflict
	}
	return out, nil
}

func (s *Store) Policy(ctx context.Context, id string, version int64) (PolicyState, error) {
	if !identityPattern.MatchString(id) || version < 1 {
		return PolicyState{}, ErrInvalid
	}
	out, err := readPolicy(ctx, s.db, id, version)
	if err == nil && !s.scopeValid(out.Scope) {
		return PolicyState{}, ErrNotFound
	}
	return out, err
}

func (s *Store) PutPolicy(ctx context.Context, p Policy) (PolicyState, error) {
	if !p.valid() || !s.scopeValid(p.Scope) {
		return PolicyState{}, ErrInvalid
	}
	for _, id := range p.RequiredConsumers {
		if _, ok := s.evidence.Consumers[id]; !ok {
			return PolicyState{}, ErrBlocked
		}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return PolicyState{}, err
	}
	defer tx.Rollback(ctx)
	if err = lock(ctx, tx, p.Scope); err != nil {
		return PolicyState{}, err
	}
	sha := digest(p)
	prior, err := readPolicy(ctx, database.TransactionConnection{Tx: tx}, p.PolicyID, p.Version)
	if err == nil {
		if prior.PolicySHA256 != sha {
			return PolicyState{}, ErrConflict
		}
		return prior, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return PolicyState{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_raw_retention_policies(policy_id,version,environment,store_id,specification,specification_sha256) VALUES($1,$2,$3,$4,$5,$6)`, p.PolicyID, p.Version, p.Environment, p.StoreID, marshal(p), sha)
	if err != nil {
		return PolicyState{}, err
	}
	if err = audit(ctx, tx, p.Scope, p.PolicyID, "policy_financial_approved", sha); err != nil {
		return PolicyState{}, err
	}
	return PolicyState{Policy: p, PolicySHA256: sha}, tx.Commit(ctx)
}

func (s *Store) ApproveRecovery(ctx context.Context, id string, version int64, sha, ref string) (PolicyState, error) {
	if !digestPattern.MatchString(sha) || !identityPattern.MatchString(ref) {
		return PolicyState{}, ErrInvalid
	}
	p, err := s.Policy(ctx, id, version)
	if err != nil {
		return p, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(ctx)
	if err = lock(ctx, tx, p.Scope); err != nil {
		return p, err
	}
	p, err = readPolicy(ctx, database.TransactionConnection{Tx: tx}, id, version)
	if err != nil {
		return p, err
	}
	if p.PolicySHA256 != sha || p.RecoveryApprovalRef != "" && p.RecoveryApprovalRef != ref {
		return p, ErrConflict
	}
	if p.RecoveryApprovalRef != "" {
		return p, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE billing_raw_retention_policies SET recovery_approval_ref=$3 WHERE policy_id=$1 AND version=$2`, id, version, ref); err != nil {
		return p, err
	}
	if err = audit(ctx, tx, p.Scope, id, "policy_recovery_approved", digest([]string{sha, ref})); err != nil {
		return p, err
	}
	p.RecoveryApprovalRef = ref
	return p, tx.Commit(ctx)
}

func (s *Store) SetActive(ctx context.Context, id string, version int64, sha string, active bool) (PolicyState, error) {
	if !digestPattern.MatchString(sha) {
		return PolicyState{}, ErrInvalid
	}
	p, err := s.Policy(ctx, id, version)
	if err != nil {
		return p, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(ctx)
	if err = lock(ctx, tx, p.Scope); err != nil {
		return p, err
	}
	p, err = readPolicy(ctx, database.TransactionConnection{Tx: tx}, id, version)
	if err != nil {
		return p, err
	}
	if p.PolicySHA256 != sha {
		return p, ErrConflict
	}
	if active && p.RecoveryApprovalRef == "" {
		return p, ErrBlocked
	}
	if p.Active == active {
		return p, nil
	}
	if active {
		if _, err = tx.Exec(ctx, `UPDATE billing_raw_retention_policies SET active=false WHERE environment=$1 AND store_id=$2 AND active`, p.Environment, p.StoreID); err != nil {
			return p, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE billing_raw_retention_policies SET active=$3 WHERE policy_id=$1 AND version=$2`, id, version, active); err != nil {
		return p, err
	}
	action := "policy_deactivated"
	if active {
		action = "policy_activated"
	}
	if err = audit(ctx, tx, p.Scope, id, action, sha); err != nil {
		return p, err
	}
	p.Active = active
	return p, tx.Commit(ctx)
}

func (s *Store) PutClearance(ctx context.Context, c Clearance) (Clearance, error) {
	if !c.valid() || !s.scopeValid(c.Scope) {
		return Clearance{}, ErrInvalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Clearance{}, err
	}
	defer tx.Rollback(ctx)
	if err = lock(ctx, tx, c.Scope); err != nil {
		return Clearance{}, err
	}
	p, err := readPolicy(ctx, database.TransactionConnection{Tx: tx}, c.PolicyID, c.PolicyVersion)
	if err != nil {
		return Clearance{}, err
	}
	if p.Scope != c.Scope {
		return Clearance{}, ErrConflict
	}
	var prior string
	err = tx.QueryRow(ctx, `SELECT attestation_sha256 FROM billing_raw_retention_clearances WHERE clearance_id=$1`, c.ClearanceID).Scan(&prior)
	sha := digest(c)
	if err == nil {
		if prior != sha {
			return Clearance{}, ErrConflict
		}
		return c, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Clearance{}, err
	}
	for _, period := range c.Periods {
		var closed bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_periods WHERE organization_id::text=$1 AND period_start=$2 AND period_end=$3 AND state='closed')`, period.OrganizationID, period.PeriodStart, period.PeriodEnd).Scan(&closed)
		if err != nil {
			return Clearance{}, err
		}
		if !closed {
			return Clearance{}, ErrBlocked
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_raw_retention_clearances(clearance_id,policy_id,policy_version,environment,store_id,from_sequence,through_sequence,attestation,attestation_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, c.ClearanceID, c.PolicyID, c.PolicyVersion, c.Environment, c.StoreID, int64(c.FromSequence), int64(c.ThroughSequence), marshal(c), sha)
	if err != nil {
		return Clearance{}, err
	}
	if err = audit(ctx, tx, c.Scope, c.ClearanceID, "periods_source_complete_reconciled", sha); err != nil {
		return Clearance{}, err
	}
	return c, tx.Commit(ctx)
}

func (s *Store) RevokeClearance(ctx context.Context, id string) error {
	if !identityPattern.MatchString(id) {
		return ErrInvalid
	}
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT attestation FROM billing_raw_retention_clearances WHERE clearance_id=$1`, id).Scan(&raw)
	if err != nil {
		return mapMissing(err)
	}
	var c Clearance
	if json.Unmarshal(raw, &c) != nil || !s.scopeValid(c.Scope) {
		return ErrNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lock(ctx, tx, c.Scope); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE billing_raw_retention_clearances SET revoked=true WHERE clearance_id=$1`, id); err != nil {
		return err
	}
	if err = audit(ctx, tx, c.Scope, id, "clearance_revoked_for_future_operations", digest(c)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func overlappingFence(ctx context.Context, tx pgx.Tx, h Hold) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_raw_retention_operations WHERE environment=$1 AND store_id=$2 AND status IN ('ACTIVE','ABORT_REQUESTED') AND ($3::bigint=0 OR (from_sequence<=$4 AND through_sequence>=$3)))`, h.Environment, h.StoreID, int64(h.FromSequence), int64(h.ThroughSequence)).Scan(&exists)
	return exists, err
}

func (s *Store) Hold(ctx context.Context, id string) (HoldState, error) {
	if !identityPattern.MatchString(id) {
		return HoldState{}, ErrInvalid
	}
	var h HoldState
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT request,status FROM billing_raw_retention_holds WHERE hold_id=$1`, id).Scan(&raw, &h.Status)
	if err != nil {
		return h, mapMissing(err)
	}
	if json.Unmarshal(raw, &h.Hold) != nil || !s.scopeValid(h.Scope) {
		return h, ErrNotFound
	}
	h.ProtectArchiveAndKeys = h.Status != "RELEASED"
	return h, nil
}

func (s *Store) PutHold(ctx context.Context, h Hold) (HoldState, error) {
	if !h.valid() || !s.scopeValid(h.Scope) {
		return HoldState{}, ErrInvalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return HoldState{}, err
	}
	defer tx.Rollback(ctx)
	if err = lock(ctx, tx, h.Scope); err != nil {
		return HoldState{}, err
	}
	var prior, status string
	err = tx.QueryRow(ctx, `SELECT request_sha256,status FROM billing_raw_retention_holds WHERE hold_id=$1`, h.HoldID).Scan(&prior, &status)
	sha := digest(h)
	if err == nil {
		if prior != sha {
			return HoldState{}, ErrConflict
		}
		return HoldState{h, status, status != "RELEASED"}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return HoldState{}, err
	}
	fenced, err := overlappingFence(ctx, tx, h)
	if err != nil {
		return HoldState{}, err
	}
	status = "ACTIVE"
	if fenced {
		status = "PENDING_FENCED"
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_raw_retention_holds(hold_id,environment,store_id,from_sequence,through_sequence,request,request_sha256,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, h.HoldID, h.Environment, h.StoreID, int64(h.FromSequence), int64(h.ThroughSequence), marshal(h), sha, status)
	if err != nil {
		return HoldState{}, err
	}
	if err = audit(ctx, tx, h.Scope, h.HoldID, "hold_"+status, sha); err != nil {
		return HoldState{}, err
	}
	return HoldState{h, status, true}, tx.Commit(ctx)
}

func (s *Store) ReleaseHold(ctx context.Context, id string) (HoldState, error) {
	h, err := s.Hold(ctx, id)
	if err != nil {
		return h, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return h, err
	}
	defer tx.Rollback(ctx)
	if err = lock(ctx, tx, h.Scope); err != nil {
		return h, err
	}
	fenced, err := overlappingFence(ctx, tx, h.Hold)
	if err != nil {
		return h, err
	}
	status := "RELEASED"
	if fenced {
		status = "RELEASE_PENDING"
	}
	var current string
	if err = tx.QueryRow(ctx, `SELECT status FROM billing_raw_retention_holds WHERE hold_id=$1`, id).Scan(&current); err != nil {
		return h, err
	}
	if current == "RELEASED" {
		h.Status = current
		h.ProtectArchiveAndKeys = false
		return h, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE billing_raw_retention_holds SET status=$2 WHERE hold_id=$1`, id, status); err != nil {
		return h, err
	}
	if err = audit(ctx, tx, h.Scope, id, "hold_"+status, digest(h.Hold)); err != nil {
		return h, err
	}
	h.Status = status
	h.ProtectArchiveAndKeys = status != "RELEASED"
	return h, tx.Commit(ctx)
}

func readOperation(ctx context.Context, db database.Connection, id string) (Operation, error) {
	var op Operation
	var raw, proofs, terminal []byte
	err := db.QueryRow(ctx, `SELECT plan,status,decision_origin,consumer_proofs,created_at,resolved_at,terminal_receipt FROM billing_raw_retention_operations WHERE operation_id=$1`, id).Scan(&raw, &op.Status, &op.DecisionOrigin, &proofs, &op.CreatedAt, &op.ResolvedAt, &terminal)
	if err != nil {
		return op, mapMissing(err)
	}
	if json.Unmarshal(raw, &op.Plan) != nil || json.Unmarshal(proofs, &op.ConsumerProofs) != nil {
		return op, ErrConflict
	}
	if terminal != nil {
		op.TerminalReceipt = &TerminalReceipt{}
		if json.Unmarshal(terminal, op.TerminalReceipt) != nil {
			return op, ErrConflict
		}
	}
	return op, nil
}

func (s *Store) Operation(ctx context.Context, id string) (Operation, error) {
	if !identityPattern.MatchString(id) {
		return Operation{}, ErrInvalid
	}
	op, err := readOperation(ctx, s.db, id)
	if err == nil && !s.scopeValid(op.Scope) {
		return Operation{}, ErrNotFound
	}
	return op, err
}

func (s *Store) Operations(ctx context.Context, scope Scope) ([]Operation, error) {
	if !s.scopeValid(scope) {
		return nil, ErrInvalid
	}
	rows, err := s.db.Query(ctx, `SELECT operation_id FROM billing_raw_retention_operations WHERE environment=$1 AND store_id=$2 AND status IN ('ACTIVE','ABORT_REQUESTED') ORDER BY created_at`, scope.Environment, scope.StoreID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]Operation, 0, len(ids))
	for _, id := range ids {
		op, err := s.Operation(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}

func (s *Store) RequestOperation(ctx context.Context, p Plan) (Operation, error) {
	if !p.valid() || !s.scopeValid(p.Scope) {
		return Operation{}, ErrInvalid
	}
	prior, err := s.Operation(ctx, p.OperationID)
	if err == nil {
		if prior.PlanSHA256 != p.PlanSHA256 {
			return Operation{}, ErrConflict
		}
		return prior, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Operation{}, err
	}
	policy, err := s.Policy(ctx, p.PolicyID, p.PolicyVersion)
	if err != nil {
		return Operation{}, err
	}
	if !policy.Active || policy.Scope != p.Scope {
		return Operation{}, ErrBlocked
	}
	if err = verifyArchive(p, policy.Policy, s.keys, time.Now().UTC()); err != nil {
		return Operation{}, err
	}
	// Caller-supplied proofs are deliberately absent from Plan. Query every
	// registered consumer ourselves through configured authenticated origins.
	proofs, err := s.evidence.Proofs(ctx, policy.RequiredConsumers, p.reconcileRequest())
	if err != nil {
		return Operation{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback(ctx)
	if err = lock(ctx, tx, p.Scope); err != nil {
		return Operation{}, err
	}
	prior, err = readOperation(ctx, database.TransactionConnection{Tx: tx}, p.OperationID)
	if err == nil {
		if prior.PlanSHA256 != p.PlanSHA256 {
			return Operation{}, ErrConflict
		}
		return prior, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Operation{}, err
	}
	policy, err = readPolicy(ctx, database.TransactionConnection{Tx: tx}, p.PolicyID, p.PolicyVersion)
	if err != nil {
		return Operation{}, err
	}
	if !policy.Active || policy.RecoveryApprovalRef == "" || policy.Scope != p.Scope {
		return Operation{}, ErrBlocked
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return Operation{}, err
	}
	if err = verifyArchive(p, policy.Policy, s.keys, now); err != nil {
		return Operation{}, err
	}
	for i, proof := range proofs {
		if !validProof(proof, policy.RequiredConsumers[i], p.reconcileRequest(), now) {
			return Operation{}, ErrBlocked
		}
	}
	var clearanceRaw []byte
	var revoked bool
	err = tx.QueryRow(ctx, `SELECT attestation,revoked FROM billing_raw_retention_clearances WHERE clearance_id=$1`, p.ClearanceID).Scan(&clearanceRaw, &revoked)
	if err != nil {
		return Operation{}, mapMissing(err)
	}
	var clearance Clearance
	if json.Unmarshal(clearanceRaw, &clearance) != nil || !clearance.valid() || revoked || clearance.Scope != p.Scope ||
		clearance.PolicyID != p.PolicyID || clearance.PolicyVersion != p.PolicyVersion || clearance.FromSequence > p.FromSequence || clearance.ThroughSequence < p.ThroughSequence {
		return Operation{}, ErrBlocked
	}
	var blocked bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_raw_retention_holds WHERE environment=$1 AND store_id=$2 AND status<>'RELEASED' AND (from_sequence=0 OR (from_sequence<=$4 AND through_sequence>=$3))) OR EXISTS(SELECT 1 FROM billing_raw_retention_operations WHERE environment=$1 AND store_id=$2 AND status IN ('ACTIVE','ABORT_REQUESTED'))`, p.Environment, p.StoreID, int64(p.FromSequence), int64(p.ThroughSequence)).Scan(&blocked)
	if err != nil {
		return Operation{}, err
	}
	if blocked {
		return Operation{}, ErrBlocked
	}
	var frontier int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(max(through_sequence),0) FROM billing_raw_retention_operations WHERE environment=$1 AND store_id=$2 AND status='COMPLETED'`, p.Environment, p.StoreID).Scan(&frontier)
	if err != nil {
		return Operation{}, err
	}
	if p.FromSequence != uint64(frontier)+1 {
		return Operation{}, ErrBlocked
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_raw_retention_operations(operation_id,environment,store_id,from_sequence,through_sequence,plan_sha256,request_sha256,plan,consumer_proofs,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'ACTIVE')`, p.OperationID, p.Environment, p.StoreID, int64(p.FromSequence), int64(p.ThroughSequence), p.PlanSHA256, digest(p), marshal(p), marshal(proofs))
	if err != nil {
		return Operation{}, err
	}
	if err = audit(ctx, tx, p.Scope, p.OperationID, "retirement_ACTIVE", digest(p)); err != nil {
		return Operation{}, err
	}
	op := Operation{Plan: p, Status: "ACTIVE", DecisionOrigin: "accepted", ConsumerProofs: proofs, CreatedAt: now}
	return op, tx.Commit(ctx)
}

func (s *Store) RequestAbort(ctx context.Context, id string) (Operation, error) {
	op, err := s.Operation(ctx, id)
	if err != nil {
		return op, err
	}
	return s.RequestAbortPlan(ctx, op.Plan)
}

// RequestAbortPlan permits a nondestructive, permanently bound cancellation of
// an intent that Billing has never accepted. An existing ACTIVE decision still
// retains its fence until authenticated Logger evidence proves the outcome.
// Neither policy approval nor archive freshness is needed to deny deletion.
func (s *Store) RequestAbortPlan(ctx context.Context, p Plan) (Operation, error) {
	if !p.valid() || !s.scopeValid(p.Scope) {
		return Operation{}, ErrInvalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback(ctx)
	if err = lock(ctx, tx, p.Scope); err != nil {
		return Operation{}, err
	}
	op, err := readOperation(ctx, database.TransactionConnection{Tx: tx}, p.OperationID)
	if errors.Is(err, ErrNotFound) {
		var now time.Time
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return op, err
		}
		proofs := []ConsumerProof{}
		_, err = tx.Exec(ctx, `INSERT INTO billing_raw_retention_operations(operation_id,environment,store_id,from_sequence,through_sequence,plan_sha256,request_sha256,plan,consumer_proofs,status,decision_origin,resolved_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'ABORTED','cancelled-before-acceptance',$10)`, p.OperationID, p.Environment, p.StoreID, int64(p.FromSequence), int64(p.ThroughSequence), p.PlanSHA256, digest(p), marshal(p), marshal(proofs), now)
		if err != nil {
			return op, err
		}
		if err = audit(ctx, tx, p.Scope, p.OperationID, "retirement_cancelled_before_acceptance", p.PlanSHA256); err != nil {
			return op, err
		}
		op = Operation{Plan: p, Status: "ABORTED", DecisionOrigin: "cancelled-before-acceptance", ConsumerProofs: proofs, CreatedAt: now, ResolvedAt: &now}
		return op, tx.Commit(ctx)
	}
	if err != nil {
		return op, err
	}
	if op.PlanSHA256 != p.PlanSHA256 {
		return op, ErrConflict
	}
	if op.Status != "ACTIVE" {
		return op, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE billing_raw_retention_operations SET status='ABORT_REQUESTED' WHERE operation_id=$1`, p.OperationID); err != nil {
		return op, err
	}
	if err = audit(ctx, tx, op.Scope, p.OperationID, "abort_requested_fence_retained", digest(op.Plan)); err != nil {
		return op, err
	}
	op.Status = "ABORT_REQUESTED"
	return op, tx.Commit(ctx)
}

func (s *Store) Resolve(ctx context.Context, id string) (Operation, error) {
	op, err := s.Operation(ctx, id)
	if err != nil {
		return op, err
	}
	if op.Status == "COMPLETED" || op.Status == "ABORTED" {
		return op, nil
	}
	receipt, err := s.evidence.Terminal(ctx, op)
	if err != nil {
		return op, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return op, err
	}
	defer tx.Rollback(ctx)
	if err = lock(ctx, tx, op.Scope); err != nil {
		return op, err
	}
	op, err = readOperation(ctx, database.TransactionConnection{Tx: tx}, id)
	if err != nil {
		return op, err
	}
	if op.TerminalReceipt != nil {
		if digest(*op.TerminalReceipt) != digest(receipt) {
			return op, ErrConflict
		}
		return op, nil
	}
	status := "COMPLETED"
	if receipt.Status == "aborted" {
		status = "ABORTED"
	}
	var resolved time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&resolved); err != nil {
		return op, err
	}
	if _, err = tx.Exec(ctx, `UPDATE billing_raw_retention_operations SET status=$2,terminal_receipt=$3,resolved_at=$4 WHERE operation_id=$1`, id, status, marshal(receipt), resolved); err != nil {
		return op, err
	}
	// The fence ends only with a durable exact participant outcome. Holds that
	// arrived later immediately protected archives and now finalize locally.
	if _, err = tx.Exec(ctx, `UPDATE billing_raw_retention_holds SET status=CASE status WHEN 'PENDING_FENCED' THEN 'ACTIVE' WHEN 'RELEASE_PENDING' THEN 'RELEASED' ELSE status END WHERE environment=$1 AND store_id=$2 AND status IN ('PENDING_FENCED','RELEASE_PENDING')`, op.Environment, op.StoreID); err != nil {
		return op, err
	}
	if err = audit(ctx, tx, op.Scope, id, "retirement_"+status, receipt.ReceiptSHA256); err != nil {
		return op, err
	}
	op.Status = status
	op.ResolvedAt = &resolved
	op.TerminalReceipt = &receipt
	return op, tx.Commit(ctx)
}
