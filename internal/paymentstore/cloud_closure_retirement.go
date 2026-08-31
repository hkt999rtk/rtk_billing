package paymentstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrCloudClosureCommandRetired = errors.New("cloud closure command is permanently retired")

type CloudCloseResolution struct {
	OperationID       string           `json:"operation_id"`
	SettlementID      string           `json:"settlement_id"`
	AMReadinessSHA256 string           `json:"am_readiness_sha256"`
	Outcome           string           `json:"outcome"`
	ReceiptSHA256     string           `json:"receipt_sha256,omitempty"`
	RetiredAt         *time.Time       `json:"retired_at,omitempty"`
	Acknowledgment    *CloudClosureAck `json:"acknowledgment,omitempty"`
}

// RetireCloudClose is a durable command, not a status lookup. It races with
// CloseCloud on the account lock: exactly one of retirement or closure wins.
// It never releases the closure fence or certifies new settlement evidence.
func (s *Store) RetireCloudClose(ctx context.Context, in CloseCloudInput) (CloudCloseResolution, error) {
	if !in.Scope.valid() || !canonicalUUID(in.SettlementID) || !validLowerSHA256(in.AMReadinessSHA256) {
		return CloudCloseResolution{}, ErrConflict
	}
	digest, err := handoffDigest(in)
	if err != nil {
		return CloudCloseResolution{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CloudCloseResolution{}, err
	}
	defer tx.Rollback(ctx)
	_, op, err := loadCloudClosureTx(ctx, tx, in.Scope)
	if err != nil {
		return CloudCloseResolution{}, err
	}
	out := CloudCloseResolution{OperationID: op.ID, SettlementID: in.SettlementID, AMReadinessSHA256: in.AMReadinessSHA256}
	var retired time.Time
	err = tx.QueryRow(ctx, `SELECT receipt_sha256,retired_at FROM billing_cloud_closure_retired_commands WHERE operation_id=$1 AND request_sha256=$2`, op.ID, digest).Scan(&out.ReceiptSHA256, &retired)
	if err == nil {
		out.Outcome = "retired"
		out.RetiredAt = &retired
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	var prior string
	ack := CloudClosureAck{OperationID: op.ID, Phase: "closed"}
	err = tx.QueryRow(ctx, `SELECT request_sha256,closed_at,receipt_sha256 FROM billing_cloud_closure_completions WHERE operation_id=$1`, op.ID).Scan(&prior, &ack.ClosedAt, &ack.ReceiptSHA256)
	if err == nil {
		if prior != digest {
			return out, ErrIdempotencyConflict
		}
		out.Outcome = "closed"
		out.Acknowledgment = &ack
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if op.Phase != "preparing" {
		return out, ErrConflict
	}
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&retired); err != nil {
		return out, err
	}
	out.Outcome = "retired"
	out.RetiredAt = &retired
	out.ReceiptSHA256, err = handoffDigest(struct {
		Request   string
		RetiredAt time.Time
	}{digest, retired})
	if err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO billing_cloud_closure_retired_commands(operation_id,request_sha256,settlement_id,am_readiness_sha256,receipt_sha256,retired_at) VALUES($1,$2,$3,$4,$5,$6)`, op.ID, digest, in.SettlementID, in.AMReadinessSHA256, out.ReceiptSHA256, retired); err != nil {
		return out, err
	}
	if err = cloudClosureAuditTx(ctx, tx, in.Scope, "command_retired", out.ReceiptSHA256); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
