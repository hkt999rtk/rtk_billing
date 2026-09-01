package billingstore

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
)

type CreatePricingVersionInput struct {
	PlanKey       string
	Version       int64
	Currency      billing.Currency
	EffectiveFrom time.Time
	Rates         []billing.PricingRate
	CreatedBy     string
	Now           time.Time
}

func (s *Store) CreatePricingVersion(ctx context.Context, in CreatePricingVersionInput) (billing.PricingVersion, error) {
	if !required(in.PlanKey) || in.Version < 1 || in.Currency != billing.CurrencyTWD || in.EffectiveFrom.IsZero() || !required(in.CreatedBy) || len(in.Rates) == 0 {
		return billing.PricingVersion{}, ErrConflict
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.PricingVersion{}, err
	}
	defer tx.Rollback(ctx)
	var out billing.PricingVersion
	err = tx.QueryRow(ctx, `
		INSERT INTO pricing_plan_versions (plan_key, version, currency, status, effective_from, created_by, created_at)
		VALUES ($1, $2, $3, 'draft', $4, $5, $6)
		RETURNING id::text, plan_key, version, currency, status, effective_from, effective_until, activated_at, created_at
	`, strings.TrimSpace(in.PlanKey), in.Version, in.Currency, in.EffectiveFrom.UTC(), strings.TrimSpace(in.CreatedBy), in.Now.UTC()).Scan(
		&out.ID, &out.PlanKey, &out.Version, &out.Currency, &out.Status, &out.EffectiveFrom, &out.EffectiveUntil, &out.ActivatedAt, &out.CreatedAt)
	if err != nil {
		return billing.PricingVersion{}, err
	}
	out.Rates = make([]billing.PricingRate, 0, len(in.Rates))
	for _, rate := range in.Rates {
		if !required(rate.ServiceCode) || !required(rate.MetricCode) || !required(rate.Description) || !required(rate.Unit) ||
			rate.UnitPriceMinor < 0 || rate.UnitPriceScale < 0 || rate.UnitPriceScale > 9 || rate.TaxRateBasisPoints < 0 || rate.TaxRateBasisPoints > 10000 {
			return billing.PricingVersion{}, ErrConflict
		}
		if rate.RoundingMode == "" {
			rate.RoundingMode = billing.RoundingHalfUp
		}
		rate.PricingVersionID = out.ID
		err = tx.QueryRow(ctx, `
			INSERT INTO pricing_rates (pricing_version_id, service_code, metric_code, description, unit,
			    unit_price_minor, unit_price_scale, rounding_mode, tax_rate_basis_points, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING id::text
		`, out.ID, strings.TrimSpace(rate.ServiceCode), strings.TrimSpace(rate.MetricCode), strings.TrimSpace(rate.Description),
			strings.TrimSpace(rate.Unit), rate.UnitPriceMinor, rate.UnitPriceScale, rate.RoundingMode, rate.TaxRateBasisPoints, in.Now.UTC()).Scan(&rate.ID)
		if err != nil {
			return billing.PricingVersion{}, err
		}
		out.Rates = append(out.Rates, rate)
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.PricingVersion{}, err
	}
	return out, nil
}

func (s *Store) ActivatePricingVersion(ctx context.Context, id string, now time.Time) (billing.PricingVersion, error) {
	if !required(id) {
		return billing.PricingVersion{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.PricingVersion{}, err
	}
	defer tx.Rollback(ctx)
	var planKey string
	if err := tx.QueryRow(ctx, `SELECT plan_key FROM pricing_plan_versions WHERE id = $1 FOR UPDATE`, id).Scan(&planKey); err != nil {
		return billing.PricingVersion{}, mapNotFound(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE pricing_plan_versions SET status = 'retired', effective_until = $2 WHERE plan_key = $1 AND status = 'active' AND id <> $3`, planKey, now.UTC(), id); err != nil {
		return billing.PricingVersion{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE pricing_plan_versions SET status = 'active', activated_at = $2 WHERE id = $1 AND status = 'draft'`, id, now.UTC()); err != nil {
		return billing.PricingVersion{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.PricingVersion{}, err
	}
	return s.GetPricingVersion(ctx, id)
}

func (s *Store) GetPricingVersion(ctx context.Context, id string) (billing.PricingVersion, error) {
	var out billing.PricingVersion
	err := s.db.QueryRow(ctx, `
		SELECT id::text, plan_key, version, currency, status, effective_from, effective_until, activated_at, created_at
		FROM pricing_plan_versions WHERE id = $1
	`, id).Scan(&out.ID, &out.PlanKey, &out.Version, &out.Currency, &out.Status, &out.EffectiveFrom, &out.EffectiveUntil, &out.ActivatedAt, &out.CreatedAt)
	if err != nil {
		return billing.PricingVersion{}, mapNotFound(err)
	}
	rates, err := s.listPricingRates(ctx, out.ID)
	if err != nil {
		return billing.PricingVersion{}, err
	}
	out.Rates = rates
	return out, nil
}

func (s *Store) ActivePricingVersion(ctx context.Context, at time.Time, currency billing.Currency) (billing.PricingVersion, error) {
	var id string
	err := s.db.QueryRow(ctx, `
		SELECT id::text FROM pricing_plan_versions
		WHERE status = 'active' AND currency = $1 AND effective_from <= $2
		  AND (effective_until IS NULL OR effective_until > $2)
		ORDER BY effective_from DESC, version DESC LIMIT 1
	`, currency, at.UTC()).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return billing.PricingVersion{}, ErrPricingUnavailable
		}
		return billing.PricingVersion{}, err
	}
	return s.GetPricingVersion(ctx, id)
}

func (s *Store) listPricingRates(ctx context.Context, versionID string) ([]billing.PricingRate, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id::text, pricing_version_id::text, service_code, metric_code, description, unit,
		       unit_price_minor, unit_price_scale, rounding_mode, tax_rate_basis_points
		FROM pricing_rates WHERE pricing_version_id = $1 ORDER BY service_code, metric_code, unit
	`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]billing.PricingRate, 0)
	for rows.Next() {
		var rate billing.PricingRate
		if err := rows.Scan(&rate.ID, &rate.PricingVersionID, &rate.ServiceCode, &rate.MetricCode, &rate.Description,
			&rate.Unit, &rate.UnitPriceMinor, &rate.UnitPriceScale, &rate.RoundingMode, &rate.TaxRateBasisPoints); err != nil {
			return nil, err
		}
		out = append(out, rate)
	}
	return out, rows.Err()
}

func (s *Store) PutUsageFact(ctx context.Context, fact billing.UsageFact) (billing.UsageFact, bool, error) {
	if !required(fact.UsageID) || !required(fact.OrganizationID) || !required(fact.ServiceCode) || !required(fact.MetricCode) ||
		!required(fact.Unit) || !required(fact.Source) || len(fact.SourceSHA256) != 64 || fact.Quantity < 0 ||
		fact.QuantityScale < 0 || fact.QuantityScale > 9 || !fact.WindowEnd.After(fact.WindowStart) {
		return billing.UsageFact{}, false, ErrConflict
	}
	if _, err := hex.DecodeString(fact.SourceSHA256); err != nil {
		return billing.UsageFact{}, false, ErrConflict
	}
	var organization pgtype.UUID
	if err := organization.Scan(fact.OrganizationID); err != nil || !organization.Valid || organization.Bytes == [16]byte{} {
		return billing.UsageFact{}, false, ErrConflict
	}
	fact.OrganizationID = organization.String()
	fact.SourceSHA256 = strings.ToLower(fact.SourceSHA256)
	fact.WindowStart = fact.WindowStart.UTC().Truncate(time.Microsecond)
	fact.WindowEnd = fact.WindowEnd.UTC().Truncate(time.Microsecond)
	if fact.WindowStart.IsZero() || !fact.WindowEnd.After(fact.WindowStart) {
		return billing.UsageFact{}, false, ErrConflict
	}
	if stored, err := s.GetUsageFact(ctx, fact.UsageID); err == nil {
		if !sameUsageFact(stored, fact) {
			return billing.UsageFact{}, false, ErrConflict
		}
		return stored, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return billing.UsageFact{}, false, err
	}
	var locked bool
	if err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM billing_periods
			WHERE organization_id = $1 AND state IN ('closing', 'closed')
			  AND tstzrange(period_start, period_end, '[)') && tstzrange($2::timestamptz, $3::timestamptz, '[)')
		)
	`, fact.OrganizationID, fact.WindowStart.UTC(), fact.WindowEnd.UTC()).Scan(&locked); err != nil {
		return billing.UsageFact{}, false, err
	}
	if locked {
		return billing.UsageFact{}, false, ErrInvoiceImmutable
	}
	var id string
	err := s.db.QueryRow(ctx, `
		INSERT INTO billing_usage_facts (usage_id, organization_id, service_code, metric_code, quantity,
		    quantity_scale, unit, window_start, window_end, source, source_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (usage_id) DO NOTHING
		RETURNING id::text
	`, fact.UsageID, fact.OrganizationID, fact.ServiceCode, fact.MetricCode, fact.Quantity, fact.QuantityScale,
		fact.Unit, fact.WindowStart.UTC(), fact.WindowEnd.UTC(), fact.Source, strings.ToLower(fact.SourceSHA256)).Scan(&id)
	created := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.ConstraintName == "billing_usage_period_barrier" {
			return billing.UsageFact{}, false, ErrInvoiceImmutable
		}
		return billing.UsageFact{}, false, err
	}
	stored, err := s.GetUsageFact(ctx, fact.UsageID)
	if err != nil {
		return billing.UsageFact{}, false, err
	}
	if !sameUsageFact(stored, fact) {
		return billing.UsageFact{}, false, ErrConflict
	}
	return stored, created, nil
}

func (s *Store) GetUsageFact(ctx context.Context, usageID string) (billing.UsageFact, error) {
	args := []any{usageID}
	visibility := "true"
	if scope, ok := billingidentity.FromContext(ctx); ok {
		if !s.tenantRead {
			return tenantRead(ctx, s, scope.OrganizationID, func(view *Store) (billing.UsageFact, error) { return view.GetUsageFact(ctx, usageID) })
		}
		args = append(args, scope.OrganizationID)
		visibility = "organization_id=$2 AND " + usageVisibility(ctx, &args)
	}
	var out billing.UsageFact
	err := s.db.QueryRow(ctx, `
		SELECT id::text, usage_id, organization_id::text, service_code, metric_code, quantity, quantity_scale,
		       unit, window_start, window_end, source, source_sha256
		FROM billing_usage_facts WHERE usage_id = $1 AND `+visibility,
		args...).Scan(&out.ID, &out.UsageID, &out.OrganizationID, &out.ServiceCode, &out.MetricCode, &out.Quantity,
		&out.QuantityScale, &out.Unit, &out.WindowStart, &out.WindowEnd, &out.Source, &out.SourceSHA256)
	return out, mapNotFound(err)
}

func (s *Store) ListUsageFacts(ctx context.Context, organizationID string, start, end time.Time) ([]billing.UsageFact, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, organizationID, func(view *Store) ([]billing.UsageFact, error) {
			return view.ListUsageFacts(ctx, organizationID, start, end)
		})
	}
	args := []any{organizationID, start.UTC(), end.UTC()}
	visibility := usageVisibility(ctx, &args)
	rows, err := s.db.Query(ctx, `
		SELECT id::text, usage_id, organization_id::text, service_code, metric_code, quantity, quantity_scale,
		       unit, window_start, window_end, source, source_sha256
		FROM billing_usage_facts
		WHERE organization_id = $1 AND window_start >= $2 AND window_end <= $3 AND `+visibility+`
		ORDER BY service_code, metric_code, unit, window_start, usage_id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]billing.UsageFact, 0)
	for rows.Next() {
		var fact billing.UsageFact
		if err := rows.Scan(&fact.ID, &fact.UsageID, &fact.OrganizationID, &fact.ServiceCode, &fact.MetricCode, &fact.Quantity,
			&fact.QuantityScale, &fact.Unit, &fact.WindowStart, &fact.WindowEnd, &fact.Source, &fact.SourceSHA256); err != nil {
			return nil, err
		}
		out = append(out, fact)
	}
	return out, rows.Err()
}

func sameUsageFact(a, b billing.UsageFact) bool {
	// The upstream digest is provenance, not authority to change any persisted
	// field. ID is assigned by this service and is not part of producer input.
	return a.UsageID == b.UsageID && a.OrganizationID == b.OrganizationID &&
		a.ServiceCode == b.ServiceCode && a.MetricCode == b.MetricCode &&
		a.Quantity == b.Quantity && a.QuantityScale == b.QuantityScale && a.Unit == b.Unit &&
		a.WindowStart.Equal(b.WindowStart) && a.WindowEnd.Equal(b.WindowEnd) &&
		a.Source == b.Source && a.SourceSHA256 == b.SourceSHA256
}
