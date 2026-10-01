package billingstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/currency"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/jackc/pgx/v5"
)

const LoggerIssuerProducer = "logger_producer"

// LoggerPeriodSeal is the authenticated attestation of the authoritative
// shared producer ledger's frozen month, including explicitly zero usage.
// A count or an empty delivery queue alone cannot create this attestation.
type LoggerPeriodSeal struct {
	OrganizationID  string           `json:"organization_id"`
	PeriodStart     time.Time        `json:"period_start"`
	PeriodEnd       time.Time        `json:"period_end"`
	IssuerKind      string           `json:"issuer_kind"`
	SealID          string           `json:"seal_id"`
	SourceHighWater json.RawMessage  `json:"source_high_water"`
	ProductIDs      []string         `json:"product_ids"`
	MetricCounts    map[string]int64 `json:"metric_counts"`
	FactSetSHA256   string           `json:"fact_set_sha256"`
	SourceSHA256    string           `json:"source_sha256"`
	SealedAt        time.Time        `json:"sealed_at"`
}

// LoggerSourceHighWater binds a seal to the environment ledger's durable
// coverage boundary. An empty newly created ledger cannot attest earlier
// months merely because its current outbox is empty.
type LoggerSourceHighWater struct {
	SchemaVersion      int       `json:"schema_version"`
	SourceID           string    `json:"source_id"`
	Ledger             string    `json:"ledger"`
	CoverageFrom       time.Time `json:"coverage_from"`
	Cutoff             time.Time `json:"cutoff"`
	MaxReceiptSequence int64     `json:"max_receipt_sequence"`
	ReceiptCount       int64     `json:"receipt_count"`
	PendingCount       int64     `json:"pending_count"`
}

func canonicalLoggerPeriodSeal(in LoggerPeriodSeal) (LoggerPeriodSeal, string, error) {
	var err error
	in.OrganizationID, err = otaUUID(in.OrganizationID)
	if err != nil {
		return LoggerPeriodSeal{}, "", ErrConflict
	}
	in.SealID, err = otaUUID(in.SealID)
	if err != nil {
		return LoggerPeriodSeal{}, "", ErrConflict
	}
	in.PeriodStart = in.PeriodStart.UTC().Truncate(time.Microsecond)
	in.PeriodEnd = in.PeriodEnd.UTC().Truncate(time.Microsecond)
	in.SealedAt = in.SealedAt.UTC().Truncate(time.Microsecond)
	if in.IssuerKind != LoggerIssuerProducer || !otaUTCMonth(in.PeriodStart, in.PeriodEnd) ||
		in.SealedAt.Before(in.PeriodEnd.Add(24*time.Hour)) || in.SealedAt.After(time.Now().UTC()) ||
		!otaDigest(in.FactSetSHA256) || !otaDigest(in.SourceSHA256) || in.ProductIDs == nil || !sort.StringsAreSorted(in.ProductIDs) {
		return LoggerPeriodSeal{}, "", ErrConflict
	}
	for i, productID := range in.ProductIDs {
		canonical, err := otaUUID(productID)
		if err != nil || canonical != productID || i > 0 && in.ProductIDs[i-1] == productID {
			return LoggerPeriodSeal{}, "", ErrConflict
		}
	}
	var highWater LoggerSourceHighWater
	var highWaterFields map[string]json.RawMessage
	if err := json.Unmarshal(in.SourceHighWater, &highWaterFields); err != nil || len(highWaterFields) != 8 {
		return LoggerPeriodSeal{}, "", ErrConflict
	}
	for _, value := range highWaterFields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return LoggerPeriodSeal{}, "", ErrConflict
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(in.SourceHighWater))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&highWater); err != nil || decoder.Decode(new(any)) != io.EOF {
		return LoggerPeriodSeal{}, "", ErrConflict
	}
	sourceID, err := otaUUID(highWater.SourceID)
	if err != nil || sourceID != highWater.SourceID || highWater.SchemaVersion != 1 || highWater.Ledger != "device_logger_receipts" ||
		highWater.CoverageFrom.IsZero() || highWater.CoverageFrom.After(in.PeriodStart) || !highWater.Cutoff.Equal(in.PeriodEnd) ||
		highWater.MaxReceiptSequence < 0 || highWater.ReceiptCount < 0 || highWater.PendingCount != 0 || highWater.ReceiptCount > highWater.MaxReceiptSequence {
		return LoggerPeriodSeal{}, "", ErrConflict
	}
	highWater.CoverageFrom = highWater.CoverageFrom.UTC()
	highWater.Cutoff = highWater.Cutoff.UTC()
	emptySource := sha256.Sum256([]byte("[]"))
	if highWater.ReceiptCount < int64(len(in.ProductIDs)) || len(in.ProductIDs) == 0 && highWater.ReceiptCount != 0 ||
		highWater.ReceiptCount == 0 && (highWater.MaxReceiptSequence != 0 || in.SourceSHA256 != hex.EncodeToString(emptySource[:]) || in.FactSetSHA256 != hex.EncodeToString(emptySource[:])) {
		return LoggerPeriodSeal{}, "", ErrConflict
	}
	in.SourceHighWater, err = json.Marshal(highWater)
	if err != nil {
		return LoggerPeriodSeal{}, "", err
	}
	if len(in.MetricCounts) != 2 || in.MetricCounts[billing.MetricLoggerIngest] != int64(len(in.ProductIDs)) || in.MetricCounts[billing.MetricLoggerRetained] != int64(len(in.ProductIDs)) {
		return LoggerPeriodSeal{}, "", ErrConflict
	}
	if _, ok := in.MetricCounts[billing.MetricLoggerIngest]; !ok {
		return LoggerPeriodSeal{}, "", ErrConflict
	}
	if _, ok := in.MetricCounts[billing.MetricLoggerRetained]; !ok {
		return LoggerPeriodSeal{}, "", ErrConflict
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return LoggerPeriodSeal{}, "", err
	}
	digest := sha256.Sum256(raw)
	return in, hex.EncodeToString(digest[:]), nil
}

// PutLoggerPeriodSeal takes the same account row lock as fact acceptance and
// invoice issuance. Reconciliation and insertion share this transaction;
// afterwards the database barrier refuses any additional source-month facts.
func (s *Store) PutLoggerPeriodSeal(ctx context.Context, input LoggerPeriodSeal) (LoggerPeriodSeal, bool, error) {
	seal, digest, err := canonicalLoggerPeriodSeal(input)
	if err != nil {
		return LoggerPeriodSeal{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return LoggerPeriodSeal{}, false, err
	}
	defer tx.Rollback(ctx)
	var accountID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM commercial_accounts WHERE organization_id=$1 AND currency=$2 FOR UPDATE`, seal.OrganizationID, currency.Settlement).Scan(&accountID); err != nil {
		return LoggerPeriodSeal{}, false, mapNotFound(err)
	}
	view := *s
	view.db = database.TransactionConnection{Tx: tx}
	var oldID, oldDigest string
	err = tx.QueryRow(ctx, `SELECT seal_id::text,content_sha256 FROM logger_period_seals WHERE organization_id=$1 AND period_start=$2 AND period_end=$3`, seal.OrganizationID, seal.PeriodStart, seal.PeriodEnd).Scan(&oldID, &oldDigest)
	if err == nil {
		if oldID != seal.SealID || oldDigest != digest {
			return LoggerPeriodSeal{}, false, ErrConflict
		}
		if err := view.reconcileLoggerPeriodFacts(ctx, seal); err != nil {
			return LoggerPeriodSeal{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return LoggerPeriodSeal{}, false, err
		}
		return seal, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return LoggerPeriodSeal{}, false, err
	}
	if err := view.reconcileLoggerPeriodFacts(ctx, seal); err != nil {
		return LoggerPeriodSeal{}, false, err
	}
	products, _ := json.Marshal(seal.ProductIDs)
	counts, _ := json.Marshal(seal.MetricCounts)
	_, err = tx.Exec(ctx, `INSERT INTO logger_period_seals (organization_id,period_start,period_end,issuer_kind,seal_id,source_high_water,product_ids,metric_counts,fact_set_sha256,source_sha256,sealed_at,content_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, seal.OrganizationID, seal.PeriodStart, seal.PeriodEnd, seal.IssuerKind, seal.SealID, []byte(seal.SourceHighWater), products, counts, seal.FactSetSHA256, seal.SourceSHA256, seal.SealedAt, digest)
	if err != nil {
		var constraint interface{ SQLState() string }
		if errors.As(err, &constraint) && constraint.SQLState() == "23505" {
			return LoggerPeriodSeal{}, false, ErrConflict
		}
		return LoggerPeriodSeal{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LoggerPeriodSeal{}, false, err
	}
	return seal, true, nil
}

func (s *Store) verifyLoggerPeriodSeal(ctx context.Context, orgID string, start, end time.Time) error {
	if !otaUTCMonth(start.UTC(), end.UTC()) {
		return ErrIncomplete
	}
	var raw []byte
	var contentSHA string
	if err := s.db.QueryRow(ctx, `SELECT jsonb_build_object('organization_id',organization_id,'period_start',period_start,'period_end',period_end,'issuer_kind',issuer_kind,'seal_id',seal_id,'source_high_water',source_high_water,'product_ids',product_ids,'metric_counts',metric_counts,'fact_set_sha256',fact_set_sha256,'source_sha256',source_sha256,'sealed_at',sealed_at),content_sha256
		FROM logger_period_seals WHERE organization_id=$1 AND period_start=$2 AND period_end=$3`, orgID, start.UTC(), end.UTC()).Scan(&raw, &contentSHA); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrIncomplete
		}
		return err
	}
	var input LoggerPeriodSeal
	if err := json.Unmarshal(raw, &input); err != nil {
		return ErrIncomplete
	}
	seal, digest, err := canonicalLoggerPeriodSeal(input)
	if err != nil || digest != contentSHA {
		return ErrIncomplete
	}
	return s.reconcileLoggerPeriodFacts(ctx, seal)
}

func (s *Store) reconcileLoggerPeriodFacts(ctx context.Context, seal LoggerPeriodSeal) error {
	rows, err := s.db.Query(ctx, `SELECT usage_id,COALESCE(product_id::text,''),metric_code,quantity,quantity_scale,unit,window_start,window_end,source_sha256
		FROM billing_usage_facts WHERE organization_id=$1 AND service_code='logger'
		AND tstzrange(window_start,window_end,'[)') && tstzrange($2::timestamptz,$3::timestamptz,'[)') ORDER BY usage_id`, seal.OrganizationID, seal.PeriodStart, seal.PeriodEnd)
	if err != nil {
		return err
	}
	defer rows.Close()
	expected := make(map[string]map[string]bool, len(seal.ProductIDs))
	for _, productID := range seal.ProductIDs {
		expected[productID] = map[string]bool{}
	}
	entries := make([][]any, 0, len(seal.ProductIDs)*2)
	for rows.Next() {
		var usageID, productID, metric, unit, sourceSHA string
		var quantity int64
		var scale int
		var start, end time.Time
		if err := rows.Scan(&usageID, &productID, &metric, &quantity, &scale, &unit, &start, &end, &sourceSHA); err != nil {
			return err
		}
		metrics, ok := expected[productID]
		if !ok || metrics[metric] || scale != 9 || quantity < 0 || !start.Equal(seal.PeriodStart) || !end.Equal(seal.PeriodEnd) || !otaDigest(sourceSHA) ||
			(metric != billing.MetricLoggerIngest && metric != billing.MetricLoggerRetained) ||
			metric == billing.MetricLoggerIngest && unit != "GiB" || metric == billing.MetricLoggerRetained && unit != "GiB-month" {
			return ErrIncomplete
		}
		metrics[metric] = true
		entries = append(entries, []any{usageID, productID, metric, quantity, scale, unit, start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano), sourceSHA})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, metrics := range expected {
		if len(metrics) != 2 {
			return ErrIncomplete
		}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != seal.FactSetSHA256 {
		return ErrIncomplete
	}
	return nil
}
