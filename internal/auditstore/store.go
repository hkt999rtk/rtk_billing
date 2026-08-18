package auditstore

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Event struct {
	EventType      string
	OrganizationID string
	ActorType      string
	ActorID        string
	SubjectType    string
	SubjectID      string
	RequestID      string
	Payload        map[string]any
}

type Store struct{ db *pgxpool.Pool }

func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

func (s *Store) Create(ctx context.Context, event Event) error {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO billing_audit_events
		(organization_id, event_type, actor_type, actor_id, subject_type, subject_id, request_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)
	`, event.OrganizationID, event.EventType, event.ActorType, event.ActorID, event.SubjectType, event.SubjectID, event.RequestID, payload)
	return err
}
