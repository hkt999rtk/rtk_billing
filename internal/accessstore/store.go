package accessstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrConflict = errors.New("billing access state conflict")

type State struct {
	OrganizationID string    `json:"organization_id"`
	State          string    `json:"state"`
	ReasonCode     string    `json:"reason_code,omitempty"`
	Version        int64     `json:"version"`
	UpdatedBy      string    `json:"updated_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Store struct{ db *pgxpool.Pool }

func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

func (s *Store) GetOrCreate(ctx context.Context, organizationID string) (State, error) {
	var out State
	err := s.db.QueryRow(ctx, `
		INSERT INTO billing_access_states (organization_id, state, updated_by)
		VALUES ($1, 'active', 'system/default')
		ON CONFLICT (organization_id) DO UPDATE SET organization_id = EXCLUDED.organization_id
		RETURNING organization_id, state, COALESCE(reason_code, ''), version, updated_by, created_at, updated_at
	`, organizationID).Scan(&out.OrganizationID, &out.State, &out.ReasonCode, &out.Version, &out.UpdatedBy, &out.CreatedAt, &out.UpdatedAt)
	return out, err
}

func (s *Store) Put(ctx context.Context, organizationID, state, reason, actor string, expectedVersion int64) (State, error) {
	state, reason, actor = strings.TrimSpace(state), strings.TrimSpace(reason), strings.TrimSpace(actor)
	if organizationID == "" || actor == "" || expectedVersion < 1 || (state != "active" && state != "read_only" && state != "suspended" && state != "closed") {
		return State{}, ErrConflict
	}
	var out State
	err := s.db.QueryRow(ctx, `
		UPDATE billing_access_states
		SET state = $2, reason_code = NULLIF($3, ''), version = version + 1, updated_by = $4, updated_at = now()
		WHERE organization_id = $1 AND version = $5
		RETURNING organization_id, state, COALESCE(reason_code, ''), version, updated_by, created_at, updated_at
	`, organizationID, state, reason, actor, expectedVersion).Scan(&out.OrganizationID, &out.State, &out.ReasonCode, &out.Version, &out.UpdatedBy, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, ErrConflict
	}
	return out, err
}
