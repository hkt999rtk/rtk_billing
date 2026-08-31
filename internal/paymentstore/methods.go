package paymentstore

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

type CreateConsentInput struct {
	AccountID         string
	ConsentType       string
	TextVersion       string
	TextSHA256        string
	AcceptedActorType string
	AcceptedActorID   string
	AcceptedAt        time.Time
	Locale            string
	Source            string
}

func (s *Store) CreateConsent(ctx context.Context, in CreateConsentInput) (payment.PaymentConsent, error) {
	if !required(in.AccountID) || (in.ConsentType != "payment_method" && in.ConsentType != "auto_topup") ||
		!required(in.TextVersion) || len(in.TextSHA256) != 64 || !required(in.AcceptedActorType) ||
		!required(in.AcceptedActorID) || in.AcceptedAt.IsZero() || !required(in.Locale) || !required(in.Source) {
		return payment.PaymentConsent{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return payment.PaymentConsent{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := getAccountForUpdate(ctx, tx, in.AccountID); err != nil {
		return payment.PaymentConsent{}, err
	}
	if err := requireNoHandoffTx(ctx, tx, in.AccountID); err != nil {
		return payment.PaymentConsent{}, err
	}
	consent, err := scanConsent(tx.QueryRow(ctx, `
		INSERT INTO payment_consents (
			account_id, consent_type, text_version, text_sha256,
			accepted_actor_type, accepted_actor_id, accepted_at, locale, source
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+consentColumns,
		in.AccountID, in.ConsentType, strings.TrimSpace(in.TextVersion), strings.ToLower(in.TextSHA256),
		in.AcceptedActorType, in.AcceptedActorID, in.AcceptedAt.UTC(), in.Locale, in.Source,
	))
	if err != nil {
		return payment.PaymentConsent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return payment.PaymentConsent{}, err
	}
	return consent, nil
}

type CreatePaymentMethodInput struct {
	AccountID                     string
	Provider                      string
	ProviderCustomerRefCiphertext []byte
	ProviderMethodRefCiphertext   []byte
	ProviderMethodRefSHA256       string
	CardBrand                     string
	LastFour                      string
	ExpiryMonth                   *int
	ExpiryYear                    *int
	Capabilities                  payment.ProviderCapabilities
	Status                        payment.PaymentMethodStatus
	ConsentID                     string
}

func (s *Store) CreatePaymentMethod(ctx context.Context, in CreatePaymentMethodInput) (payment.PaymentMethod, error) {
	provider := payment.NormalizeProvider(in.Provider)
	if !required(in.AccountID) || provider == "" || !required(in.ConsentID) {
		return payment.PaymentMethod{}, ErrConflict
	}
	if in.Status == payment.PaymentMethodStatusActive &&
		(len(in.ProviderCustomerRefCiphertext) == 0 || len(in.ProviderMethodRefCiphertext) == 0 || len(in.ProviderMethodRefSHA256) != 64) {
		return payment.PaymentMethod{}, ErrConflict
	}
	if in.LastFour != "" && (len(in.LastFour) != 4 || strings.Trim(in.LastFour, "0123456789") != "") {
		return payment.PaymentMethod{}, ErrConflict
	}
	capabilities, err := json.Marshal(in.Capabilities)
	if err != nil {
		return payment.PaymentMethod{}, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return payment.PaymentMethod{}, err
	}
	defer tx.Rollback(ctx)

	if _, err := getAccountForUpdate(ctx, tx, in.AccountID); err != nil {
		return payment.PaymentMethod{}, err
	}
	if err := requireNoHandoffTx(ctx, tx, in.AccountID); err != nil {
		return payment.PaymentMethod{}, err
	}
	consent, err := getConsentForUpdate(ctx, tx, in.ConsentID)
	if err != nil {
		return payment.PaymentMethod{}, err
	}
	if consent.AccountID != in.AccountID || consent.ConsentType != "payment_method" || consent.RevokedAt != nil {
		return payment.PaymentMethod{}, ErrConflict
	}

	method, err := scanPaymentMethod(tx.QueryRow(ctx, `
		INSERT INTO payment_methods (
			account_id, provider, provider_customer_ref_ciphertext,
			provider_method_ref_ciphertext, provider_method_ref_sha256,
			card_brand, last_four, expiry_month, expiry_year, capabilities,
			status, consent_id
		)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), $8, $9, $10::jsonb, $11, $12)
		RETURNING `+paymentMethodColumns,
		in.AccountID, provider, in.ProviderCustomerRefCiphertext, in.ProviderMethodRefCiphertext,
		strings.ToLower(in.ProviderMethodRefSHA256), in.CardBrand, in.LastFour,
		in.ExpiryMonth, in.ExpiryYear, capabilities, in.Status, in.ConsentID,
	))
	if err != nil {
		return payment.PaymentMethod{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return payment.PaymentMethod{}, err
	}
	return method, nil
}

func getConsentForUpdate(ctx context.Context, tx pgx.Tx, consentID string) (payment.PaymentConsent, error) {
	return scanConsent(tx.QueryRow(ctx, `
		SELECT `+consentColumns+`
		FROM payment_consents
		WHERE id = $1
		FOR UPDATE
	`, consentID))
}

func getPaymentMethodForUpdate(ctx context.Context, tx pgx.Tx, methodID string) (payment.PaymentMethod, error) {
	return scanPaymentMethod(tx.QueryRow(ctx, `
		SELECT `+paymentMethodColumns+`
		FROM payment_methods
		WHERE id = $1
		FOR UPDATE
	`, methodID))
}
