package paymentstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func (s *Store) EnsureCommercialAccount(ctx context.Context, organizationID string, currency payment.Currency) (payment.CommercialAccount, bool, error) {
	if scope, ok := billingidentity.FromContext(ctx); ok {
		if scope.OrganizationID != organizationID || currency != payment.CurrencyTWD {
			return payment.CommercialAccount{}, false, billingidentity.ErrDenied
		}
		account, err := s.GetCommercialAccount(ctx, scope.AccountID)
		return account, false, err
	}
	if !required(organizationID) {
		return payment.CommercialAccount{}, false, ErrConflict
	}
	if err := payment.ValidateCurrency(currency); err != nil {
		return payment.CommercialAccount{}, false, err
	}

	account, err := scanAccount(s.db.QueryRow(ctx, `
		INSERT INTO commercial_accounts (organization_id, currency)
		VALUES ($1, $2)
		ON CONFLICT (organization_id, currency) DO NOTHING
		RETURNING `+accountColumns, organizationID, currency))
	if err == nil {
		return account, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return payment.CommercialAccount{}, false, err
	}

	account, err = scanAccount(s.db.QueryRow(ctx, `
		SELECT `+accountColumns+`
		FROM commercial_accounts
		WHERE organization_id = $1 AND currency = $2
	`, organizationID, currency))
	return account, false, err
}

func (s *Store) GetCommercialAccount(ctx context.Context, accountID string) (payment.CommercialAccount, error) {
	if _, ok := billingidentity.FromContext(ctx); ok {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return payment.CommercialAccount{}, err
		}
		defer tx.Rollback(ctx)
		account, err := getAccountForUpdate(ctx, tx, accountID)
		if err != nil {
			return payment.CommercialAccount{}, err
		}
		return account, tx.Commit(ctx)
	}
	return scanAccount(s.db.QueryRow(ctx, `
		SELECT `+accountColumns+`
		FROM commercial_accounts
		WHERE id = $1
	`, accountID))
}

func (s *Store) GetCommercialAccountByOrganization(ctx context.Context, organizationID string, currency payment.Currency) (payment.CommercialAccount, error) {
	if scope, ok := billingidentity.FromContext(ctx); ok {
		if scope.OrganizationID != organizationID || currency != payment.CurrencyTWD {
			return payment.CommercialAccount{}, billingidentity.ErrDenied
		}
		return s.GetCommercialAccount(ctx, scope.AccountID)
	}
	if !required(organizationID) || payment.ValidateCurrency(currency) != nil {
		return payment.CommercialAccount{}, ErrConflict
	}
	return scanAccount(s.db.QueryRow(ctx, `
		SELECT `+accountColumns+`
		FROM commercial_accounts
		WHERE organization_id = $1 AND currency = $2
	`, organizationID, currency))
}

func getAccountForUpdate(ctx context.Context, tx pgx.Tx, accountID string) (payment.CommercialAccount, error) {
	if err := billingidentity.LockAccount(ctx, tx, accountID); err != nil {
		return payment.CommercialAccount{}, err
	}
	return scanAccount(tx.QueryRow(ctx, `
		SELECT `+accountColumns+`
		FROM commercial_accounts
		WHERE id = $1
		FOR UPDATE
	`, accountID))
}
