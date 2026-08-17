package billingstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

type PutProfileInput struct {
	OrganizationID     string
	LegalName          string
	TaxIdentifier      string
	BillingAddress     string
	ContactEmail       string
	Locale             string
	Timezone           string
	DeliveryPreference string
	ExpectedVersion    int64
	Now                time.Time
}

func (s *Store) EnsureBillingProfile(ctx context.Context, organizationID string, now time.Time) (billing.BillingProfile, bool, error) {
	if !required(organizationID) {
		return billing.BillingProfile{}, false, ErrConflict
	}
	profile, err := s.GetBillingProfile(ctx, organizationID)
	if err == nil {
		return profile, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return billing.BillingProfile{}, false, err
	}
	var created bool
	err = s.db.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO billing_profiles (organization_id, legal_name, locale, timezone, delivery_preference, created_at, updated_at)
			VALUES ($1::uuid, $1::uuid::text, 'zh-TW', 'Asia/Taipei', 'portal', $2, $2)
			ON CONFLICT (organization_id) DO NOTHING
			RETURNING true
		)
		SELECT COALESCE((SELECT true FROM inserted), false)
	`, organizationID, now.UTC()).Scan(&created)
	if err != nil {
		return billing.BillingProfile{}, false, err
	}
	profile, err = s.GetBillingProfile(ctx, organizationID)
	return profile, created, err
}

func (s *Store) GetBillingProfile(ctx context.Context, organizationID string) (billing.BillingProfile, error) {
	return scanProfile(s.db.QueryRow(ctx, `
		SELECT organization_id::text, legal_name, COALESCE(tax_identifier, ''), COALESCE(billing_address, ''),
		       COALESCE(contact_email, ''), locale, timezone, delivery_preference, version, created_at, updated_at
		FROM billing_profiles WHERE organization_id = $1
	`, organizationID))
}

func (s *Store) PutBillingProfile(ctx context.Context, in PutProfileInput) (billing.BillingProfile, error) {
	if !required(in.OrganizationID) || !required(in.LegalName) || !required(in.Locale) || !required(in.Timezone) ||
		(in.DeliveryPreference != "portal" && in.DeliveryPreference != "portal_and_email") || in.ExpectedVersion < 1 {
		return billing.BillingProfile{}, ErrConflict
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	profile, err := scanProfile(s.db.QueryRow(ctx, `
		UPDATE billing_profiles
		SET legal_name = $3, tax_identifier = NULLIF($4, ''), billing_address = NULLIF($5, ''),
		    contact_email = NULLIF($6, ''), locale = $7, timezone = $8, delivery_preference = $9,
		    version = version + 1, updated_at = $10
		WHERE organization_id = $1 AND version = $2
		RETURNING organization_id::text, legal_name, COALESCE(tax_identifier, ''), COALESCE(billing_address, ''),
		          COALESCE(contact_email, ''), locale, timezone, delivery_preference, version, created_at, updated_at
	`, in.OrganizationID, in.ExpectedVersion, strings.TrimSpace(in.LegalName), strings.TrimSpace(in.TaxIdentifier),
		strings.TrimSpace(in.BillingAddress), strings.TrimSpace(in.ContactEmail), strings.TrimSpace(in.Locale),
		strings.TrimSpace(in.Timezone), in.DeliveryPreference, in.Now.UTC()))
	if errors.Is(err, ErrNotFound) {
		var exists bool
		if scanErr := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_profiles WHERE organization_id = $1)`, in.OrganizationID).Scan(&exists); scanErr != nil {
			return billing.BillingProfile{}, scanErr
		}
		if exists {
			return billing.BillingProfile{}, ErrConflict
		}
	}
	return profile, err
}

func scanProfile(row rowScanner) (billing.BillingProfile, error) {
	var out billing.BillingProfile
	err := row.Scan(&out.OrganizationID, &out.LegalName, &out.TaxIdentifier, &out.BillingAddress, &out.ContactEmail,
		&out.Locale, &out.Timezone, &out.DeliveryPreference, &out.Version, &out.CreatedAt, &out.UpdatedAt)
	return out, mapNotFound(err)
}

var _ = pgx.ErrNoRows
