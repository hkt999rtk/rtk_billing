package paymentstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func (s *Store) EnsureCommercialAccount(ctx context.Context, organizationID string, currency payment.Currency) (payment.CommercialAccount, bool, error) {
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
	return scanAccount(s.db.QueryRow(ctx, `
		SELECT `+accountColumns+`
		FROM commercial_accounts
		WHERE id = $1
	`, accountID))
}

func (s *Store) GetCommercialAccountByOrganization(ctx context.Context, organizationID string, currency payment.Currency) (payment.CommercialAccount, error) {
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
	return scanAccount(tx.QueryRow(ctx, `
		SELECT `+accountColumns+`
		FROM commercial_accounts
		WHERE id = $1
		FOR UPDATE
	`, accountID))
}
