package payment

import "time"

type PaymentAttempt struct {
	ID                   string            `json:"id"`
	IntentID             string            `json:"intent_id"`
	Operation            ProviderOperation `json:"operation"`
	AttemptNumber        int               `json:"attempt_number"`
	StartedAt            time.Time         `json:"started_at"`
	CompletedAt          *time.Time        `json:"completed_at,omitempty"`
	NormalizedResult     string            `json:"normalized_result"`
	ProviderCode         string            `json:"provider_code,omitempty"`
	RequestSHA256        string            `json:"request_sha256,omitempty"`
	ResponseSHA256       string            `json:"response_sha256,omitempty"`
	NextReconciliationAt *time.Time        `json:"next_reconciliation_at,omitempty"`
	CreatedAt            time.Time         `json:"created_at"`
}

type ReconciliationReason string

const (
	ReconciliationReasonCharge  ReconciliationReason = "charge"
	ReconciliationReasonUnknown ReconciliationReason = "unknown"
	ReconciliationReasonWebhook ReconciliationReason = "webhook"
	ReconciliationReasonCredit  ReconciliationReason = "credit"
	ReconciliationReasonRefund  ReconciliationReason = "refund"
)

type ReconciliationStatus string

const (
	ReconciliationStatusPending   ReconciliationStatus = "pending"
	ReconciliationStatusLeased    ReconciliationStatus = "leased"
	ReconciliationStatusCompleted ReconciliationStatus = "completed"
	ReconciliationStatusFailed    ReconciliationStatus = "failed"
)

type ReconciliationJob struct {
	ID           string               `json:"id"`
	IntentID     string               `json:"intent_id"`
	Reason       ReconciliationReason `json:"reason"`
	Status       ReconciliationStatus `json:"status"`
	DueAt        time.Time            `json:"due_at"`
	AttemptCount int                  `json:"attempt_count"`
	LeasedAt     *time.Time           `json:"leased_at,omitempty"`
	LeaseOwner   string               `json:"lease_owner,omitempty"`
	LastError    string               `json:"last_error,omitempty"`
	CreatedAt    time.Time            `json:"created_at"`
	UpdatedAt    time.Time            `json:"updated_at"`
}

type WebhookReceipt struct {
	ID                     string            `json:"id"`
	Provider               string            `json:"provider"`
	ProviderEventReference string            `json:"provider_event_reference,omitempty"`
	PayloadSHA256          string            `json:"payload_sha256"`
	VerificationResult     string            `json:"verification_result"`
	IntentID               string            `json:"intent_id,omitempty"`
	NormalizedEventType    string            `json:"normalized_event_type,omitempty"`
	ProcessingState        string            `json:"processing_state"`
	SafeSummary            map[string]string `json:"safe_summary,omitempty"`
	ReceivedAt             time.Time         `json:"received_at"`
	ProcessedAt            *time.Time        `json:"processed_at,omitempty"`
	CreatedAt              time.Time         `json:"created_at"`
	UpdatedAt              time.Time         `json:"updated_at"`
}
