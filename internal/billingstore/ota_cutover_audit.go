package billingstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hkt999rtk/rtk_billing/internal/database"
)

// OTACutoverAudit inventories the boundary between profile-local billing months
// and a proposed UTC OTA month. It is evidence for a cutover plan, not permission
// to publish a price card or rewrite historical financial records.
type OTACutoverAudit struct {
	Status        string                   `json:"status"`
	ObservedAt    time.Time                `json:"observed_at"`
	EffectiveFrom time.Time                `json:"effective_from"`
	Accounts      []OTACutoverAccountAudit `json:"accounts"`
}

type OTACutoverAccountAudit struct {
	OrganizationSHA256  string     `json:"organization_sha256"`
	AccountState        string     `json:"account_state"`
	Timezone            string     `json:"timezone,omitempty"`
	LocalMonthBoundary  *time.Time `json:"local_month_boundary,omitempty"`
	BridgeKind          string     `json:"bridge_kind"`
	BridgeStart         *time.Time `json:"bridge_start,omitempty"`
	BridgeEnd           *time.Time `json:"bridge_end,omitempty"`
	BridgeUsageFacts    int64      `json:"bridge_usage_facts"`
	BridgeClosedPeriods int64      `json:"bridge_closed_periods"`
	BridgeInvoices      int64      `json:"bridge_invoices"`
	TargetMonthPeriods  int64      `json:"target_month_periods"`
	TargetMonthInvoices int64      `json:"target_month_invoices"`
	OwnerEvidence       string     `json:"owner_evidence"`
}

// AuditOTACutover takes a single read-only repeatable-read snapshot. It reports
// every TWD account, including closed accounts, because historical invoices and
// retained usage cannot be inferred from the currently active account set.
func (s *Store) AuditOTACutover(ctx context.Context, effectiveFrom time.Time) (OTACutoverAudit, error) {
	cutover := effectiveFrom.UTC()
	monthStart := time.Date(cutover.Year(), cutover.Month(), 1, 0, 0, 0, 0, time.UTC)
	if effectiveFrom.IsZero() || !cutover.Equal(monthStart) {
		return OTACutoverAudit{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return OTACutoverAudit{}, err
	}
	defer tx.Rollback(ctx)
	view := *s
	view.db = database.TransactionConnection{Tx: tx}
	var observedAt time.Time
	if err := view.db.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&observedAt); err != nil {
		return OTACutoverAudit{}, err
	}
	if !cutover.After(observedAt.UTC()) {
		return OTACutoverAudit{}, ErrConflict
	}
	end := cutover.AddDate(0, 1, 0)
	rows, err := view.db.Query(ctx, `
		WITH account_view AS (
			SELECT a.organization_id, a.state AS account_state, p.timezone, p.ownership_version AS profile_owner_version,
			       r.ownership_version AS current_owner_version, r.effective_from AS current_owner_start,
			       CASE WHEN p.timezone IS NULL THEN NULL ELSE
			           CASE WHEN date_trunc('month', $1::timestamptz AT TIME ZONE p.timezone)
			                     = date_trunc('month', $1::timestamptz AT TIME ZONE 'UTC')
			                THEN date_trunc('month', $1::timestamptz AT TIME ZONE p.timezone) AT TIME ZONE p.timezone
			                ELSE (date_trunc('month', $1::timestamptz AT TIME ZONE p.timezone) + interval '1 month') AT TIME ZONE p.timezone
			           END
			       END AS local_boundary
			FROM commercial_accounts a
			LEFT JOIN billing_profiles p ON p.organization_id=a.organization_id
			LEFT JOIN billing_responsibility_periods r ON r.account_id=a.id AND r.effective_until IS NULL
			WHERE a.currency='TWD'
		), bridges AS (
			SELECT *, LEAST(local_boundary,$1::timestamptz) AS bridge_start,
			          GREATEST(local_boundary,$1::timestamptz) AS bridge_end FROM account_view
		)
		SELECT b.organization_id::text,b.account_state,b.timezone,b.profile_owner_version,
		       b.current_owner_version,b.current_owner_start,b.local_boundary,b.bridge_start,b.bridge_end,
		       (SELECT count(*) FROM billing_usage_facts f WHERE f.organization_id=b.organization_id
		          AND f.window_start<b.bridge_end AND f.window_end>b.bridge_start),
		       (SELECT count(*) FROM billing_periods p WHERE p.organization_id=b.organization_id AND p.currency='TWD'
		          AND p.state='closed' AND p.period_start<b.bridge_end AND p.period_end>b.bridge_start),
		       (SELECT count(*) FROM billing_invoices i WHERE i.organization_id=b.organization_id AND i.currency='TWD'
		          AND i.period_start<b.bridge_end AND i.period_end>b.bridge_start),
		       (SELECT count(*) FROM billing_periods p WHERE p.organization_id=b.organization_id AND p.currency='TWD'
		          AND p.period_start<$2 AND p.period_end>$1),
		       (SELECT count(*) FROM billing_invoices i WHERE i.organization_id=b.organization_id AND i.currency='TWD'
		          AND i.period_start<$2 AND i.period_end>$1)
		FROM bridges b ORDER BY b.organization_id`, cutover, end)
	if err != nil {
		return OTACutoverAudit{}, err
	}
	defer rows.Close()
	report := OTACutoverAudit{Status: "technical_inventory_only", ObservedAt: observedAt.UTC(),
		EffectiveFrom: cutover, Accounts: []OTACutoverAccountAudit{}}
	for rows.Next() {
		var orgID string
		var timezone sql.NullString
		var profileVersion, ownerVersion sql.NullInt64
		var ownerStart, boundary, bridgeStart, bridgeEnd sql.NullTime
		item := OTACutoverAccountAudit{}
		if err := rows.Scan(&orgID, &item.AccountState, &timezone, &profileVersion, &ownerVersion,
			&ownerStart, &boundary, &bridgeStart, &bridgeEnd, &item.BridgeUsageFacts,
			&item.BridgeClosedPeriods, &item.BridgeInvoices, &item.TargetMonthPeriods,
			&item.TargetMonthInvoices); err != nil {
			return OTACutoverAudit{}, err
		}
		item.OrganizationSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(orgID)))
		if timezone.Valid {
			item.Timezone = timezone.String
			local, start, finish := boundary.Time.UTC(), bridgeStart.Time.UTC(), bridgeEnd.Time.UTC()
			item.LocalMonthBoundary, item.BridgeStart, item.BridgeEnd = &local, &start, &finish
			item.BridgeKind = "none"
			if local.Before(cutover) {
				item.BridgeKind = "gap_risk"
			} else if local.After(cutover) {
				item.BridgeKind = "overlap_risk"
			}
		} else {
			item.BridgeKind = "unknown_profile"
		}
		switch {
		case !timezone.Valid:
			item.OwnerEvidence = "profile_missing"
		case !ownerVersion.Valid || !ownerStart.Valid:
			item.OwnerEvidence = "current_owner_missing"
		case !profileVersion.Valid || profileVersion.Int64 != ownerVersion.Int64:
			item.OwnerEvidence = "profile_owner_mismatch"
		case ownerStart.Time.After(cutover):
			item.OwnerEvidence = "owner_month_incomplete"
		default:
			item.OwnerEvidence = "complete_at_cutover"
		}
		report.Accounts = append(report.Accounts, item)
	}
	if err := rows.Err(); err != nil {
		return OTACutoverAudit{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OTACutoverAudit{}, err
	}
	return report, nil
}
