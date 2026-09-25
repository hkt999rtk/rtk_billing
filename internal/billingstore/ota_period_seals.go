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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

const (
	OTAIssuerPlatformGrants = "platform_grants"
	OTAIssuerProducer       = "ota_producer"
)

var otaMetricCodes = []string{
	billing.MetricOTADeviceTask,
	billing.MetricOTASuccessfulDownloadGiB,
	billing.MetricOTAArtifactStorageGiBMonth,
	billing.MetricOTAArtifactWrite,
}

// OTAPeriodSeal is an immutable source attestation for one UTC month.
// source_high_water is source-owned JSON; Billing verifies its presence and
// content binding but does not infer source completeness from a number alone.
type OTAPeriodSeal struct {
	OrganizationID  string           `json:"organization_id"`
	PeriodStart     time.Time        `json:"period_start"`
	PeriodEnd       time.Time        `json:"period_end"`
	IssuerKind      string           `json:"issuer_kind"`
	SealID          string           `json:"seal_id"`
	SourceHighWater json.RawMessage  `json:"source_high_water"`
	ProductIDs      []string         `json:"product_ids"`
	MetricCounts    map[string]int64 `json:"metric_counts"`
	FactSetSHA256   *string          `json:"fact_set_sha256,omitempty"`
	SourceSHA256    string           `json:"source_sha256"`
	SealedAt        time.Time        `json:"sealed_at"`
}

func canonicalOTAPeriodSeal(in OTAPeriodSeal) (OTAPeriodSeal, string, error) {
	var err error
	in.OrganizationID, err = otaUUID(in.OrganizationID)
	if err != nil {
		return OTAPeriodSeal{}, "", ErrConflict
	}
	in.SealID, err = otaUUID(in.SealID)
	if err != nil {
		return OTAPeriodSeal{}, "", ErrConflict
	}
	in.PeriodStart = in.PeriodStart.UTC().Truncate(time.Microsecond)
	in.PeriodEnd = in.PeriodEnd.UTC().Truncate(time.Microsecond)
	in.SealedAt = in.SealedAt.UTC().Truncate(time.Microsecond)
	if !otaUTCMonth(in.PeriodStart, in.PeriodEnd) || in.SealedAt.Before(in.PeriodEnd) ||
		(in.IssuerKind != OTAIssuerPlatformGrants && in.IssuerKind != OTAIssuerProducer) ||
		!otaDigest(in.SourceSHA256) {
		return OTAPeriodSeal{}, "", ErrConflict
	}
	if in.ProductIDs == nil || !sort.StringsAreSorted(in.ProductIDs) {
		return OTAPeriodSeal{}, "", ErrConflict
	}
	for i, productID := range in.ProductIDs {
		canonical, err := otaUUID(productID)
		if err != nil || canonical != productID || i > 0 && in.ProductIDs[i-1] == productID {
			return OTAPeriodSeal{}, "", ErrConflict
		}
	}
	var highWater any
	decoder := json.NewDecoder(bytes.NewReader(in.SourceHighWater))
	decoder.UseNumber()
	if err := decoder.Decode(&highWater); err != nil || decoder.Decode(new(any)) != io.EOF || highWater == nil {
		return OTAPeriodSeal{}, "", ErrConflict
	}
	switch value := highWater.(type) {
	case string:
		if value == "" {
			return OTAPeriodSeal{}, "", ErrConflict
		}
	case map[string]any:
		if len(value) == 0 {
			return OTAPeriodSeal{}, "", ErrConflict
		}
	case []any:
		if len(value) == 0 {
			return OTAPeriodSeal{}, "", ErrConflict
		}
	}
	in.SourceHighWater, err = json.Marshal(highWater)
	if err != nil {
		return OTAPeriodSeal{}, "", ErrConflict
	}
	if in.MetricCounts == nil {
		return OTAPeriodSeal{}, "", ErrConflict
	}
	if in.IssuerKind == OTAIssuerPlatformGrants {
		if len(in.MetricCounts) != 0 || in.FactSetSHA256 != nil {
			return OTAPeriodSeal{}, "", ErrConflict
		}
	} else {
		if len(in.MetricCounts) != len(otaMetricCodes) || in.FactSetSHA256 == nil || !otaDigest(*in.FactSetSHA256) {
			return OTAPeriodSeal{}, "", ErrConflict
		}
		for _, metric := range otaMetricCodes {
			if count, ok := in.MetricCounts[metric]; !ok || count < 0 {
				return OTAPeriodSeal{}, "", ErrConflict
			}
		}
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return OTAPeriodSeal{}, "", err
	}
	digest := sha256.Sum256(raw)
	return in, hex.EncodeToString(digest[:]), nil
}

func otaUUID(value string) (string, error) {
	var id pgtype.UUID
	if err := id.Scan(value); err != nil || !id.Valid || id.Bytes == [16]byte{} {
		return "", ErrConflict
	}
	return id.String(), nil
}

func otaDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func otaUTCMonth(start, end time.Time) bool {
	if start.IsZero() || end.IsZero() {
		return false
	}
	monthStart := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start.Equal(monthStart) && end.Equal(monthStart.AddDate(0, 1, 0))
}

func otaPricingComplete(rates []billing.PricingRate) (enabled, complete bool) {
	units := map[string]string{
		billing.MetricOTADeviceTask:              billing.UnitOTADeviceTask,
		billing.MetricOTASuccessfulDownloadGiB:   billing.UnitOTAGiB,
		billing.MetricOTAArtifactStorageGiBMonth: billing.UnitOTAGiBMonth,
		billing.MetricOTAArtifactWrite:           billing.UnitOTAArtifactWrite,
	}
	seen := make(map[string]bool, len(units))
	for _, rate := range rates {
		if rate.ServiceCode != billing.ServiceOTA {
			continue
		}
		enabled = true
		unit, ok := units[rate.MetricCode]
		if !ok || unit != rate.Unit || seen[rate.MetricCode] {
			return true, false
		}
		seen[rate.MetricCode] = true
	}
	return enabled, !enabled || len(seen) == len(units)
}

// PutOTAPeriodSeal accepts exact replays and rejects any changed evidence.
func (s *Store) PutOTAPeriodSeal(ctx context.Context, input OTAPeriodSeal) (OTAPeriodSeal, bool, error) {
	seal, digest, err := canonicalOTAPeriodSeal(input)
	if err != nil {
		return OTAPeriodSeal{}, false, err
	}
	products, _ := json.Marshal(seal.ProductIDs)
	counts, _ := json.Marshal(seal.MetricCounts)
	var factDigest any
	if seal.FactSetSHA256 != nil {
		factDigest = *seal.FactSetSHA256
	}
	var inserted string
	err = s.db.QueryRow(ctx, `INSERT INTO ota_period_seals
		(organization_id,period_start,period_end,issuer_kind,seal_id,source_high_water,product_ids,metric_counts,fact_set_sha256,source_sha256,sealed_at,content_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT DO NOTHING RETURNING seal_id::text`, seal.OrganizationID, seal.PeriodStart, seal.PeriodEnd,
		seal.IssuerKind, seal.SealID, []byte(seal.SourceHighWater), products, counts,
		factDigest, seal.SourceSHA256, seal.SealedAt, digest).Scan(&inserted)
	if err == nil {
		return seal, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return OTAPeriodSeal{}, false, err
	}
	var oldID, oldDigest string
	err = s.db.QueryRow(ctx, `SELECT seal_id::text,content_sha256 FROM ota_period_seals
		WHERE organization_id=$1 AND period_start=$2 AND period_end=$3 AND issuer_kind=$4`,
		seal.OrganizationID, seal.PeriodStart, seal.PeriodEnd, seal.IssuerKind).Scan(&oldID, &oldDigest)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OTAPeriodSeal{}, false, ErrConflict
		}
		return OTAPeriodSeal{}, false, err
	}
	if oldID != seal.SealID || oldDigest != digest {
		return OTAPeriodSeal{}, false, ErrConflict
	}
	return seal, false, nil
}

// verifyOTAPeriodSeals runs under the commercial account close lock and the
// same transaction snapshot as invoice preparation.
func (s *Store) verifyOTAPeriodSeals(ctx context.Context, orgID string, start, end time.Time) error {
	if !otaUTCMonth(start.UTC(), end.UTC()) {
		return ErrIncomplete
	}
	type sealed struct {
		products []string
		counts   map[string]int64
		factSHA  *string
	}
	byIssuer := make(map[string]sealed, 2)
	rows, err := s.db.Query(ctx, `SELECT issuer_kind,product_ids,metric_counts,fact_set_sha256 FROM ota_period_seals
		WHERE organization_id=$1 AND period_start=$2 AND period_end=$3`, orgID, start.UTC(), end.UTC())
	if err != nil {
		return err
	}
	for rows.Next() {
		var issuer string
		var productsRaw, countsRaw []byte
		var factSHA *string
		if err := rows.Scan(&issuer, &productsRaw, &countsRaw, &factSHA); err != nil {
			rows.Close()
			return err
		}
		var item sealed
		if err := json.Unmarshal(productsRaw, &item.products); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(countsRaw, &item.counts); err != nil {
			rows.Close()
			return err
		}
		item.factSHA = factSHA
		byIssuer[issuer] = item
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	platform, platformOK := byIssuer[OTAIssuerPlatformGrants]
	producer, producerOK := byIssuer[OTAIssuerProducer]
	if !platformOK || !producerOK || producer.factSHA == nil {
		return ErrIncomplete
	}
	expectedProducts := make(map[string]bool, len(platform.products)+len(producer.products))
	for _, product := range platform.products {
		expectedProducts[product] = true
	}
	for _, product := range producer.products {
		expectedProducts[product] = true
	}
	var overlapping bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_usage_facts
		WHERE organization_id=$1 AND service_code='ota'
		AND tstzrange(window_start,window_end,'[)') && tstzrange($2::timestamptz,$3::timestamptz,'[)')
		AND (window_start<$2 OR window_end>$3))`, orgID, start.UTC(), end.UTC()).Scan(&overlapping); err != nil {
		return err
	}
	if overlapping {
		return ErrIncomplete
	}
	usageRows, err := s.db.Query(ctx, `SELECT usage_id,COALESCE(product_id::text,''),metric_code,quantity,quantity_scale,unit,window_start,window_end,source_sha256
		FROM billing_usage_facts WHERE organization_id=$1 AND service_code='ota' AND window_start>=$2 AND window_end<=$3
		ORDER BY usage_id`, orgID, start.UTC(), end.UTC())
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("["))
	actualCounts := make(map[string]int64, len(otaMetricCodes))
	for _, metric := range otaMetricCodes {
		actualCounts[metric] = 0
	}
	first := true
	for usageRows.Next() {
		var usageID, productID, metric, unit, sourceSHA string
		var quantity int64
		var scale int
		var windowStart, windowEnd time.Time
		if err := usageRows.Scan(&usageID, &productID, &metric, &quantity, &scale, &unit, &windowStart, &windowEnd, &sourceSHA); err != nil {
			usageRows.Close()
			return err
		}
		if !expectedProducts[productID] || !billing.ValidOTAUsageFact(billing.UsageFact{ProductID: productID, ServiceCode: billing.ServiceOTA, MetricCode: metric, Quantity: quantity, QuantityScale: scale, Unit: unit, WindowStart: windowStart, WindowEnd: windowEnd}) {
			usageRows.Close()
			return ErrIncomplete
		}
		actualCounts[metric]++
		entry, err := json.Marshal([]any{usageID, productID, metric, quantity, scale, unit,
			windowStart.UTC().Format(time.RFC3339Nano), windowEnd.UTC().Format(time.RFC3339Nano), sourceSHA})
		if err != nil {
			usageRows.Close()
			return err
		}
		if !first {
			_, _ = hash.Write([]byte(","))
		}
		first = false
		_, _ = hash.Write(entry)
	}
	err = usageRows.Err()
	usageRows.Close()
	if err != nil {
		return err
	}
	_, _ = hash.Write([]byte("]"))
	if hex.EncodeToString(hash.Sum(nil)) != *producer.factSHA || len(producer.counts) != len(actualCounts) {
		return ErrIncomplete
	}
	for _, metric := range otaMetricCodes {
		if producer.counts[metric] != actualCounts[metric] {
			return ErrIncomplete
		}
	}
	return nil
}
