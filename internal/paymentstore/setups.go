package paymentstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

type PaymentMethodSetupSession struct {
	ID              string
	AccountID       string
	Provider        string
	IdempotencyKey  string
	RequestSHA256   string
	CorrelationID   string
	PaymentMethodID string
	State           payment.PaymentIntentState
	ProviderCode    string
	HostedURLSHA256 string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type BeginPaymentMethodSetupInput struct {
	AccountID      string
	Provider       string
	IdempotencyKey string
	RequestSHA256  string
	CorrelationID  string
	Capabilities   payment.ProviderCapabilities
	Consent        CreateConsentInput
	Now            time.Time
}

type BeginPaymentMethodSetupResult struct {
	Session   PaymentMethodSetupSession
	Consent   payment.PaymentConsent
	Method    payment.PaymentMethod
	Duplicate bool
}

func (s *Store) BeginPaymentMethodSetup(ctx context.Context, in BeginPaymentMethodSetupInput) (BeginPaymentMethodSetupResult, error) {
	in.Provider = payment.NormalizeProvider(in.Provider)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	in.RequestSHA256 = strings.ToLower(strings.TrimSpace(in.RequestSHA256))
	in.CorrelationID = strings.TrimSpace(in.CorrelationID)
	if !required(in.AccountID) || in.Provider == "" || !required(in.IdempotencyKey) || len(in.IdempotencyKey) > 128 ||
		!validLowerSHA256(in.RequestSHA256) || !required(in.CorrelationID) || in.Consent.AccountID != in.AccountID ||
		in.Consent.ConsentType != "payment_method" || !required(in.Consent.TextVersion) ||
		!validLowerSHA256(strings.ToLower(in.Consent.TextSHA256)) || !required(in.Consent.AcceptedActorType) ||
		!required(in.Consent.AcceptedActorID) || !required(in.Consent.Locale) || !required(in.Consent.Source) {
		return BeginPaymentMethodSetupResult{}, ErrConflict
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	} else {
		in.Now = in.Now.UTC()
	}
	in.Consent.AcceptedAt = in.Now

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return BeginPaymentMethodSetupResult{}, err
	}
	defer tx.Rollback(ctx)
	account, err := getAccountForUpdate(ctx, tx, in.AccountID)
	if err != nil {
		return BeginPaymentMethodSetupResult{}, err
	}
	if account.State == payment.AccountStateClosed {
		return BeginPaymentMethodSetupResult{}, ErrAccountClosed
	}
	if err := requireNoHandoffTx(ctx, tx, account.ID); err != nil {
		return BeginPaymentMethodSetupResult{}, err
	}
	existing, err := getPaymentMethodSetupByKey(ctx, tx, in.AccountID, in.Provider, in.IdempotencyKey)
	if err == nil {
		if existing.RequestSHA256 != in.RequestSHA256 {
			return BeginPaymentMethodSetupResult{}, ErrIdempotencyConflict
		}
		method, methodErr := getPaymentMethodForUpdate(ctx, tx, existing.PaymentMethodID)
		if methodErr != nil {
			return BeginPaymentMethodSetupResult{}, methodErr
		}
		consent, consentErr := getConsentForUpdate(ctx, tx, method.ConsentID)
		if consentErr != nil {
			return BeginPaymentMethodSetupResult{}, consentErr
		}
		return BeginPaymentMethodSetupResult{Session: existing, Consent: consent, Method: method, Duplicate: true}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return BeginPaymentMethodSetupResult{}, err
	}

	consent, err := scanConsent(tx.QueryRow(ctx, `
		INSERT INTO payment_consents (
			account_id, consent_type, text_version, text_sha256,
			accepted_actor_type, accepted_actor_id, accepted_at, locale, source
		)
		VALUES ($1, 'payment_method', $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+consentColumns,
		in.AccountID, strings.TrimSpace(in.Consent.TextVersion), strings.ToLower(in.Consent.TextSHA256),
		strings.TrimSpace(in.Consent.AcceptedActorType), strings.TrimSpace(in.Consent.AcceptedActorID),
		in.Now, strings.TrimSpace(in.Consent.Locale), strings.TrimSpace(in.Consent.Source),
	))
	if err != nil {
		return BeginPaymentMethodSetupResult{}, err
	}
	capabilities, err := json.Marshal(in.Capabilities)
	if err != nil {
		return BeginPaymentMethodSetupResult{}, err
	}
	method, err := scanPaymentMethod(tx.QueryRow(ctx, `
		INSERT INTO payment_methods (account_id, provider, capabilities, status, consent_id)
		VALUES ($1, $2, $3::jsonb, 'pending', $4)
		RETURNING `+paymentMethodColumns,
		in.AccountID, in.Provider, capabilities, consent.ID,
	))
	if err != nil {
		return BeginPaymentMethodSetupResult{}, err
	}
	session, err := scanPaymentMethodSetup(tx.QueryRow(ctx, `
		INSERT INTO payment_method_setup_sessions (
			account_id, provider, idempotency_key, request_sha256,
			correlation_id, payment_method_id, state, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'created', $7, $7)
		RETURNING `+paymentMethodSetupColumns,
		in.AccountID, in.Provider, in.IdempotencyKey, in.RequestSHA256,
		in.CorrelationID, method.ID, in.Now,
	))
	if err != nil {
		return BeginPaymentMethodSetupResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BeginPaymentMethodSetupResult{}, err
	}
	return BeginPaymentMethodSetupResult{Session: session, Consent: consent, Method: method}, nil
}

type CompletePaymentMethodSetupInput struct {
	AccountID                     string
	SessionID                     string
	State                         payment.PaymentIntentState
	ProviderCode                  string
	HostedURLSHA256               string
	ProviderCustomerRefCiphertext []byte
	ProviderMethodRefCiphertext   []byte
	ProviderMethodRefSHA256       string
	CardBrand                     string
	LastFour                      string
	ExpiryMonth                   *int
	ExpiryYear                    *int
	Now                           time.Time
}

type CompletePaymentMethodSetupResult struct {
	Session   PaymentMethodSetupSession
	Method    payment.PaymentMethod
	Duplicate bool
}

func (s *Store) CompletePaymentMethodSetup(ctx context.Context, in CompletePaymentMethodSetupInput) (CompletePaymentMethodSetupResult, error) {
	in.ProviderCode = payment.NormalizeProviderCode(in.ProviderCode)
	in.HostedURLSHA256 = strings.ToLower(strings.TrimSpace(in.HostedURLSHA256))
	in.ProviderMethodRefSHA256 = strings.ToLower(strings.TrimSpace(in.ProviderMethodRefSHA256))
	if !required(in.AccountID) || !required(in.SessionID) || !validLowerSHA256(in.HostedURLSHA256) {
		return CompletePaymentMethodSetupResult{}, ErrConflict
	}
	switch in.State {
	case payment.PaymentIntentStateRequiresAction, payment.PaymentIntentStateFailed:
		if len(in.ProviderCustomerRefCiphertext) != 0 || len(in.ProviderMethodRefCiphertext) != 0 || in.ProviderMethodRefSHA256 != "" {
			return CompletePaymentMethodSetupResult{}, ErrConflict
		}
	case payment.PaymentIntentStateSucceeded:
		if len(in.ProviderCustomerRefCiphertext) == 0 || len(in.ProviderMethodRefCiphertext) == 0 || !validLowerSHA256(in.ProviderMethodRefSHA256) {
			return CompletePaymentMethodSetupResult{}, ErrConflict
		}
	default:
		return CompletePaymentMethodSetupResult{}, ErrConflict
	}
	if in.LastFour != "" && (len(in.LastFour) != 4 || strings.Trim(in.LastFour, "0123456789") != "") {
		return CompletePaymentMethodSetupResult{}, ErrConflict
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	} else {
		in.Now = in.Now.UTC()
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CompletePaymentMethodSetupResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := getAccountForUpdate(ctx, tx, in.AccountID); err != nil {
		return CompletePaymentMethodSetupResult{}, err
	}
	session, err := getPaymentMethodSetupByID(ctx, tx, in.SessionID)
	if err != nil {
		return CompletePaymentMethodSetupResult{}, err
	}
	if session.AccountID != in.AccountID {
		return CompletePaymentMethodSetupResult{}, ErrNotFound
	}
	method, err := getPaymentMethodForUpdate(ctx, tx, session.PaymentMethodID)
	if err != nil {
		return CompletePaymentMethodSetupResult{}, err
	}
	var invalidated bool
	if err := tx.QueryRow(ctx, `SELECT invalidated_by_handoff IS NOT NULL FROM payment_method_setup_sessions WHERE id=$1`, session.ID).Scan(&invalidated); err != nil {
		return CompletePaymentMethodSetupResult{}, err
	}
	if invalidated {
		// Retain deduplicated evidence, not provider credentials or card details.
		// The method/consent remain revoked even if the operation is later aborted.
		digest, err := handoffDigest(struct{ State, Code, HostedSHA, MethodSHA string }{
			string(in.State), in.ProviderCode, in.HostedURLSHA256, in.ProviderMethodRefSHA256,
		})
		if err != nil {
			return CompletePaymentMethodSetupResult{}, err
		}
		observed, err := tx.Exec(ctx, `INSERT INTO billing_handoff_setup_observations(session_id,result_sha256,provider_state)
			VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, session.ID, digest, in.State)
		if err != nil {
			return CompletePaymentMethodSetupResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return CompletePaymentMethodSetupResult{}, err
		}
		return CompletePaymentMethodSetupResult{Session: session, Method: method, Duplicate: observed.RowsAffected() == 0}, ErrSetupInvalidated
	}
	if err := requireNoHandoffTx(ctx, tx, in.AccountID); err != nil {
		return CompletePaymentMethodSetupResult{}, err
	}
	if session.State != payment.PaymentIntentStateCreated {
		var methodSHA string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(provider_method_ref_sha256, '') FROM payment_methods WHERE id = $1`, method.ID).Scan(&methodSHA); err != nil {
			return CompletePaymentMethodSetupResult{}, err
		}
		if session.State == in.State && session.ProviderCode == in.ProviderCode && session.HostedURLSHA256 == in.HostedURLSHA256 &&
			(in.State != payment.PaymentIntentStateSucceeded || methodSHA == in.ProviderMethodRefSHA256) {
			return CompletePaymentMethodSetupResult{Session: session, Method: method, Duplicate: true}, nil
		}
		if session.State != payment.PaymentIntentStateRequiresAction ||
			(in.State != payment.PaymentIntentStateSucceeded && in.State != payment.PaymentIntentStateFailed) ||
			session.HostedURLSHA256 != in.HostedURLSHA256 {
			return CompletePaymentMethodSetupResult{}, ErrIdempotencyConflict
		}
	}
	status := payment.PaymentMethodStatusPending
	if in.State == payment.PaymentIntentStateSucceeded {
		status = payment.PaymentMethodStatusActive
	} else if in.State == payment.PaymentIntentStateFailed {
		status = payment.PaymentMethodStatusFailed
	}
	method, err = scanPaymentMethod(tx.QueryRow(ctx, `
		UPDATE payment_methods
		SET provider_customer_ref_ciphertext = NULLIF($2, ''::bytea),
			provider_method_ref_ciphertext = NULLIF($3, ''::bytea),
			provider_method_ref_sha256 = NULLIF($4, ''),
			card_brand = NULLIF($5, ''), last_four = NULLIF($6, ''),
			expiry_month = $7, expiry_year = $8, status = $9
		WHERE id = $1
		RETURNING `+paymentMethodColumns,
		method.ID, in.ProviderCustomerRefCiphertext, in.ProviderMethodRefCiphertext,
		in.ProviderMethodRefSHA256, strings.TrimSpace(in.CardBrand), strings.TrimSpace(in.LastFour),
		in.ExpiryMonth, in.ExpiryYear, status,
	))
	if err != nil {
		return CompletePaymentMethodSetupResult{}, err
	}
	session, err = scanPaymentMethodSetup(tx.QueryRow(ctx, `
		UPDATE payment_method_setup_sessions
		SET state = $2, provider_code = NULLIF($3, ''), hosted_url_sha256 = $4, updated_at = $5
		WHERE id = $1
		RETURNING `+paymentMethodSetupColumns,
		session.ID, in.State, in.ProviderCode, in.HostedURLSHA256, in.Now,
	))
	if err != nil {
		return CompletePaymentMethodSetupResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CompletePaymentMethodSetupResult{}, err
	}
	return CompletePaymentMethodSetupResult{Session: session, Method: method}, nil
}

func getPaymentMethodSetupByKey(ctx context.Context, tx pgx.Tx, accountID, provider, key string) (PaymentMethodSetupSession, error) {
	return scanPaymentMethodSetup(tx.QueryRow(ctx, `
		SELECT `+paymentMethodSetupColumns+`
		FROM payment_method_setup_sessions
		WHERE account_id = $1 AND provider = $2 AND idempotency_key = $3
		FOR UPDATE
	`, accountID, provider, key))
}

func getPaymentMethodSetupByID(ctx context.Context, tx pgx.Tx, id string) (PaymentMethodSetupSession, error) {
	return scanPaymentMethodSetup(tx.QueryRow(ctx, `
		SELECT `+paymentMethodSetupColumns+`
		FROM payment_method_setup_sessions
		WHERE id = $1
		FOR UPDATE
	`, id))
}

func scanPaymentMethodSetup(row rowScanner) (PaymentMethodSetupSession, error) {
	var out PaymentMethodSetupSession
	var providerCode, hostedURLSHA256 *string
	err := row.Scan(
		&out.ID, &out.AccountID, &out.Provider, &out.IdempotencyKey,
		&out.RequestSHA256, &out.CorrelationID, &out.PaymentMethodID,
		&out.State, &providerCode, &hostedURLSHA256, &out.CreatedAt, &out.UpdatedAt,
	)
	if providerCode != nil {
		out.ProviderCode = *providerCode
	}
	if hostedURLSHA256 != nil {
		out.HostedURLSHA256 = *hostedURLSHA256
	}
	return out, mapNotFound(err)
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

const paymentMethodSetupColumns = `
	id::text, account_id::text, provider, idempotency_key, request_sha256,
	correlation_id, payment_method_id::text, state, provider_code,
	hosted_url_sha256, created_at, updated_at`
