// Package billingidentity verifies trusted global-user context against Billing's
// evidence-backed Account Manager projection. Permissions alone are not ownership.
package billingidentity

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid     = errors.New("invalid billing ownership context")
	ErrUnavailable = errors.New("billing ownership evidence unavailable")
	ErrDenied      = errors.New("current sole owner required")
	ErrVersion     = errors.New("billing ownership version conflict")
	ErrTransition  = errors.New("billing ownership commit in progress")
	ErrSnapshot    = errors.New("billing read snapshot changed; retry with current authority")
)

type Scope struct {
	OrganizationID     string
	AccountID          string
	UserID             string
	OwnershipVersion   int64
	CurrentPeriodStart time.Time
}

type scopeKey struct{}

// WithScope is only for the authenticated tenant boundary. Internal reconciliation
// deliberately has no tenant scope; it uses separate credentials and authority.
func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}
func FromContext(ctx context.Context) (Scope, bool) {
	scope, ok := ctx.Value(scopeKey{}).(Scope)
	return scope, ok
}

// ForProviderReconciliation is only for a response already obtained/verified by a
// trusted provider adapter. Losing tenant authority must not discard provider
// evidence. Original intent/session binding and handoff fences still apply in the
// reconciliation store; this context must never authorize a new customer action.
func ForProviderReconciliation(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, struct{}{})
}

func ValidUUID(value string) bool {
	var id pgtype.UUID
	if err := id.Scan(value); err != nil || !id.Valid {
		return false
	}
	canonical, err := id.Value()
	return err == nil && canonical == value
}

type Store struct{ db *pgxpool.Pool }

func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

// AuthorizeOwner never provisions accounts or infers historical owners. The BFF
// authenticates the global session; Billing verifies actor/cloud/version itself.
func (s *Store) AuthorizeOwner(ctx context.Context, organizationID, userID string, version int64) (Scope, error) {
	if !ValidUUID(organizationID) || !ValidUUID(userID) || version < 1 {
		return Scope{}, ErrInvalid
	}
	var scope Scope
	scope.OrganizationID, scope.UserID, scope.OwnershipVersion = organizationID, userID, version
	var owner *string
	var currentVersion *int64
	var periodStart *time.Time
	var state string
	var committing bool
	err := s.db.QueryRow(ctx, `SELECT a.id::text,a.state,p.owner_user_id::text,p.ownership_version,p.effective_from,
		EXISTS(SELECT 1 FROM billing_ownership_handoffs h WHERE h.account_id=a.id AND
		(h.phase IN ('commit_authorized','finalizing') OR (h.phase='abort_pending' AND EXISTS
		(SELECT 1 FROM billing_handoff_commit_authorizations g WHERE g.operation_id=h.id))))
		OR EXISTS(SELECT 1 FROM billing_cloud_closures c WHERE c.account_id=a.id AND c.phase<>'canceled')
		FROM commercial_accounts a LEFT JOIN billing_responsibility_periods p ON p.account_id=a.id AND p.effective_until IS NULL
		WHERE a.organization_id=$1 AND a.currency='TWD'`, organizationID).
		Scan(&scope.AccountID, &state, &owner, &currentVersion, &periodStart, &committing)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (owner == nil || currentVersion == nil || periodStart == nil) {
		return Scope{}, ErrUnavailable
	}
	if err != nil {
		return Scope{}, err
	}
	if *owner != userID || state == "closed" {
		return Scope{}, ErrDenied
	}
	if *currentVersion != version {
		return Scope{}, ErrVersion
	}
	if committing {
		return Scope{}, ErrTransition
	}
	scope.CurrentPeriodStart = *periodStart
	return scope, nil
}

// LockAccount revalidates scope inside each monetary write transaction, after
// locking the same account row as handoff commit/finalization. A request that
// passed HTTP authentication before a transfer cannot write using stale authority.
// Internal workers have no tenant scope and retain their separately authorized path.
func LockAccount(ctx context.Context, tx pgx.Tx, accountID string) error {
	scope, scoped := FromContext(ctx)
	if !scoped {
		return nil
	}
	if scope.AccountID != accountID || !ValidUUID(accountID) || !ValidUUID(scope.OrganizationID) || !ValidUUID(scope.UserID) || scope.OwnershipVersion < 1 {
		return ErrDenied
	}
	var org, state string
	if err := tx.QueryRow(ctx, `SELECT organization_id::text,state FROM commercial_accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&org, &state); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40001" {
			return ErrSnapshot
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDenied
		}
		return err
	}
	if org != scope.OrganizationID || state == "closed" {
		return ErrDenied
	}
	var owner string
	var version int64
	err := tx.QueryRow(ctx, `SELECT owner_user_id::text,ownership_version FROM billing_responsibility_periods WHERE account_id=$1 AND effective_until IS NULL`, accountID).Scan(&owner, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnavailable
	}
	if err != nil {
		return err
	}
	if owner != scope.UserID {
		return ErrDenied
	}
	if version != scope.OwnershipVersion {
		return ErrVersion
	}
	var committing bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_ownership_handoffs h WHERE h.account_id=$1 AND
		(h.phase IN ('commit_authorized','finalizing') OR (h.phase='abort_pending' AND EXISTS
		(SELECT 1 FROM billing_handoff_commit_authorizations g WHERE g.operation_id=h.id))))
		OR EXISTS(SELECT 1 FROM billing_cloud_closures WHERE account_id=$1 AND phase<>'canceled')`, accountID).Scan(&committing); err != nil {
		return err
	}
	if committing {
		return ErrTransition
	}
	return nil
}
