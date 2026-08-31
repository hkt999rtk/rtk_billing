package paymentstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

type PutAutoTopUpPolicyInput struct {
	AccountID             string
	Enabled               bool
	ThresholdMinor        int64
	TopUpAmountMinor      int64
	Currency              payment.Currency
	PaymentMethodID       string
	DailyAttemptLimit     int
	DailyAmountLimitMinor int64
	CooldownSeconds       int64
	ConsentID             string
	ActorID               string
	ExpectedVersion       int64
}

func (s *Store) PutAutoTopUpPolicy(ctx context.Context, in PutAutoTopUpPolicyInput) (payment.AutoTopUpPolicy, error) {
	if !required(in.AccountID) || !required(in.PaymentMethodID) || !required(in.ConsentID) || !required(in.ActorID) {
		return payment.AutoTopUpPolicy{}, ErrConflict
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	defer tx.Rollback(ctx)

	account, err := getAccountForUpdate(ctx, tx, in.AccountID)
	if err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	if account.State == payment.AccountStateClosed {
		return payment.AutoTopUpPolicy{}, ErrAccountClosed
	}
	if err := requireNoHandoffTx(ctx, tx, account.ID); err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	if account.Currency != in.Currency {
		return payment.AutoTopUpPolicy{}, ErrConflict
	}

	method, err := getPaymentMethodForUpdate(ctx, tx, in.PaymentMethodID)
	if err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	if method.AccountID != in.AccountID || method.Status != payment.PaymentMethodStatusActive {
		return payment.AutoTopUpPolicy{}, payment.ErrPaymentMethodInactive
	}
	if !method.Capabilities.MerchantInitiatedCharge {
		return payment.AutoTopUpPolicy{}, payment.ErrCapabilityUnsupported
	}

	consent, err := getConsentForUpdate(ctx, tx, in.ConsentID)
	if err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	if consent.AccountID != in.AccountID || consent.ConsentType != "auto_topup" || consent.RevokedAt != nil {
		return payment.AutoTopUpPolicy{}, ErrConflict
	}

	current, currentErr := getPolicyForUpdate(ctx, tx, in.AccountID)
	if currentErr != nil && !errors.Is(currentErr, ErrNotFound) {
		return payment.AutoTopUpPolicy{}, currentErr
	}
	createdBy := in.ActorID
	replacesRetired := false
	generation, version := int64(1), int64(1)
	if currentErr == nil {
		if scope, scoped := billingidentity.FromContext(ctx); scoped {
			_, methodErr := getPaymentMethodForUpdate(ctx, tx, current.PaymentMethodID)
			if errors.Is(methodErr, ErrNotFound) {
				// Logical creation may replace only proven, fully revoked predecessor
				// configuration. Unknown legacy policies require reviewed migration.
				if err := tx.QueryRow(ctx, `SELECT EXISTS(
					SELECT 1 FROM billing_payment_method_responsibility binding
					JOIN billing_responsibility_periods period ON period.id=binding.period_id
					JOIN payment_methods method ON method.id=binding.method_id
					JOIN payment_consents consent ON consent.id=$3
					WHERE binding.account_id=$1 AND binding.method_id=$2
					AND period.effective_until IS NOT NULL AND period.ownership_version<$4
					AND method.status='revoked' AND consent.account_id=$1 AND consent.revoked_at IS NOT NULL
				)`, in.AccountID, current.PaymentMethodID, current.ConsentID, scope.OwnershipVersion).Scan(&replacesRetired); err != nil {
					return payment.AutoTopUpPolicy{}, err
				}
				if !replacesRetired || current.Enabled || current.Armed || in.ExpectedVersion != 0 {
					return payment.AutoTopUpPolicy{}, ErrConflict
				}
			} else if methodErr != nil {
				return payment.AutoTopUpPolicy{}, methodErr
			} else if in.ExpectedVersion != current.Version {
				return payment.AutoTopUpPolicy{}, ErrConflict
			}
		}
		if !replacesRetired && in.ExpectedVersion > 0 && current.Version != in.ExpectedVersion {
			return payment.AutoTopUpPolicy{}, ErrConflict
		}
		generation = current.Generation + 1
		version = current.Version + 1
		createdBy = ""
	} else if in.ExpectedVersion > 0 {
		return payment.AutoTopUpPolicy{}, ErrConflict
	}

	candidate := payment.AutoTopUpPolicy{
		AccountID:             in.AccountID,
		Enabled:               in.Enabled,
		ThresholdMinor:        in.ThresholdMinor,
		TopUpAmountMinor:      in.TopUpAmountMinor,
		Currency:              in.Currency,
		PaymentMethodID:       in.PaymentMethodID,
		DailyAttemptLimit:     in.DailyAttemptLimit,
		DailyAmountLimitMinor: in.DailyAmountLimitMinor,
		CooldownSeconds:       in.CooldownSeconds,
		Generation:            generation,
		Version:               version,
		Armed:                 true,
		ConsentID:             in.ConsentID,
	}
	if err := payment.ValidatePolicy(candidate); err != nil {
		return payment.AutoTopUpPolicy{}, err
	}

	var policy payment.AutoTopUpPolicy
	if currentErr == nil {
		policy, err = scanPolicy(tx.QueryRow(ctx, `
			UPDATE auto_topup_policies
			SET enabled = $2,
				threshold_minor = $3,
				top_up_amount_minor = $4,
				currency = $5,
				payment_method_id = $6,
				daily_attempt_limit = $7,
				daily_amount_limit_minor = $8,
				cooldown_seconds = $9,
				generation = $10,
				version = $11,
				armed = true,
				consecutive_failure_count = 0,
				last_triggered_at = NULL,
				last_succeeded_at = NULL,
				consent_id = $12,
				updated_by = $13,
				created_by = CASE WHEN $14 THEN $13 ELSE created_by END,
				created_at = CASE WHEN $14 THEN now() ELSE created_at END
			WHERE account_id = $1
			RETURNING `+policyColumns,
			in.AccountID, in.Enabled, in.ThresholdMinor, in.TopUpAmountMinor,
			in.Currency, in.PaymentMethodID, in.DailyAttemptLimit,
			in.DailyAmountLimitMinor, in.CooldownSeconds, generation, version,
			in.ConsentID, in.ActorID, replacesRetired,
		))
	} else {
		policy, err = scanPolicy(tx.QueryRow(ctx, `
			INSERT INTO auto_topup_policies (
				account_id, enabled, threshold_minor, top_up_amount_minor,
				currency, payment_method_id, daily_attempt_limit,
				daily_amount_limit_minor, cooldown_seconds, generation,
				version, armed, consent_id, created_by, updated_by
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, true, $12, $13, $13)
			RETURNING `+policyColumns,
			in.AccountID, in.Enabled, in.ThresholdMinor, in.TopUpAmountMinor,
			in.Currency, in.PaymentMethodID, in.DailyAttemptLimit,
			in.DailyAmountLimitMinor, in.CooldownSeconds, generation, version,
			in.ConsentID, createdBy,
		))
	}
	if err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return payment.AutoTopUpPolicy{}, err
	}
	return policy, nil
}

func (s *Store) GetAutoTopUpPolicy(ctx context.Context, accountID string) (payment.AutoTopUpPolicy, error) {
	if needsTenantRead(ctx, s) {
		return tenantRead(ctx, s, accountID, func(view *Store) (payment.AutoTopUpPolicy, error) { return view.GetAutoTopUpPolicy(ctx, accountID) })
	}
	args := []any{accountID}
	visibility := periodVisibility(ctx, &args, "billing_payment_method_responsibility", "privacy_binding.method_id=auto_topup_policies.payment_method_id AND privacy_binding.account_id=auto_topup_policies.account_id", true)
	return scanPolicy(s.db.QueryRow(ctx, `
		SELECT `+policyColumns+`
		FROM auto_topup_policies
		WHERE account_id = $1 AND `+visibility,
		args...))
}

func getPolicyForUpdate(ctx context.Context, tx pgx.Tx, accountID string) (payment.AutoTopUpPolicy, error) {
	return scanPolicy(tx.QueryRow(ctx, `
		SELECT `+policyColumns+`
		FROM auto_topup_policies
		WHERE account_id = $1
		FOR UPDATE
	`, accountID))
}

func policyDayStart(now time.Time) time.Time {
	start, _ := payment.DailyLimitWindow(now)
	return start
}
