package payment

import "time"

type Currency string

const CurrencyTWD Currency = "TWD"

type AccountState string

const (
	AccountStateActive            AccountState = "active"
	AccountStateAttentionRequired AccountState = "attention_required"
	AccountStateSuspended         AccountState = "suspended"
	AccountStateClosed            AccountState = "closed"
)

type CommercialAccount struct {
	ID                    string       `json:"id"`
	OrganizationID        string       `json:"organization_id"`
	Currency              Currency     `json:"currency"`
	AvailableBalanceMinor int64        `json:"available_balance_minor"`
	State                 AccountState `json:"state"`
	Version               int64        `json:"version"`
	CreatedAt             time.Time    `json:"created_at"`
	UpdatedAt             time.Time    `json:"updated_at"`
}

type LedgerDirection string

const (
	LedgerDirectionCredit LedgerDirection = "credit"
	LedgerDirectionDebit  LedgerDirection = "debit"
)

type LedgerReason string

const (
	LedgerReasonInvoiceDebit           LedgerReason = "invoice_debit"
	LedgerReasonUsageAdjustmentDebit   LedgerReason = "usage_adjustment_debit"
	LedgerReasonManualAdjustmentDebit  LedgerReason = "manual_adjustment_debit"
	LedgerReasonPaymentTopUpCredit     LedgerReason = "payment_top_up_credit"
	LedgerReasonManualAdjustmentCredit LedgerReason = "manual_adjustment_credit"
	LedgerReasonRefundDebit            LedgerReason = "refund_debit"
	LedgerReasonChargebackDebit        LedgerReason = "chargeback_debit"
)

type LedgerEntry struct {
	ID                string          `json:"id"`
	AccountID         string          `json:"account_id"`
	Direction         LedgerDirection `json:"direction"`
	AmountMinor       int64           `json:"amount_minor"`
	Currency          Currency        `json:"currency"`
	Reason            LedgerReason    `json:"reason"`
	IdempotencyScope  string          `json:"idempotency_scope"`
	IdempotencyKey    string          `json:"idempotency_key"`
	ExternalType      string          `json:"external_type,omitempty"`
	ExternalID        string          `json:"external_id,omitempty"`
	BalanceAfterMinor int64           `json:"balance_after_minor"`
	ActorType         string          `json:"actor_type,omitempty"`
	ActorID           string          `json:"actor_id,omitempty"`
	RequestID         string          `json:"request_id,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

type ProviderCapabilities struct {
	HostedSetup             bool `json:"hosted_setup"`
	VaultedMethod           bool `json:"vaulted_method"`
	MerchantInitiatedCharge bool `json:"merchant_initiated_charge"`
	StatusQuery             bool `json:"status_query"`
	Webhook                 bool `json:"webhook"`
	Refund                  bool `json:"refund"`
	RequiresCustomerAction  bool `json:"requires_customer_action"`
}

type PaymentConsent struct {
	ID                string     `json:"id"`
	AccountID         string     `json:"account_id"`
	ConsentType       string     `json:"consent_type"`
	TextVersion       string     `json:"text_version"`
	TextSHA256        string     `json:"text_sha256"`
	AcceptedActorType string     `json:"accepted_actor_type"`
	AcceptedActorID   string     `json:"accepted_actor_id"`
	AcceptedAt        time.Time  `json:"accepted_at"`
	Locale            string     `json:"locale"`
	Source            string     `json:"source"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	RevocationReason  string     `json:"revocation_reason,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type PaymentMethodStatus string

const (
	PaymentMethodStatusPending PaymentMethodStatus = "pending"
	PaymentMethodStatusActive  PaymentMethodStatus = "active"
	PaymentMethodStatusExpired PaymentMethodStatus = "expired"
	PaymentMethodStatusRevoked PaymentMethodStatus = "revoked"
	PaymentMethodStatusFailed  PaymentMethodStatus = "failed"
)

type PaymentMethod struct {
	ID           string               `json:"id"`
	AccountID    string               `json:"account_id"`
	Provider     string               `json:"provider"`
	Status       PaymentMethodStatus  `json:"status"`
	CardBrand    string               `json:"card_brand,omitempty"`
	LastFour     string               `json:"last_four,omitempty"`
	ExpiryMonth  *int                 `json:"expiry_month,omitempty"`
	ExpiryYear   *int                 `json:"expiry_year,omitempty"`
	Capabilities ProviderCapabilities `json:"capabilities"`
	ConsentID    string               `json:"consent_id"`
	CreatedAt    time.Time            `json:"created_at"`
	UpdatedAt    time.Time            `json:"updated_at"`
}

type AutoTopUpPolicy struct {
	ID                      string     `json:"id"`
	AccountID               string     `json:"account_id"`
	Enabled                 bool       `json:"enabled"`
	ThresholdMinor          int64      `json:"threshold_minor"`
	TopUpAmountMinor        int64      `json:"top_up_amount_minor"`
	Currency                Currency   `json:"currency"`
	PaymentMethodID         string     `json:"payment_method_id"`
	DailyAttemptLimit       int        `json:"daily_attempt_limit"`
	DailyAmountLimitMinor   int64      `json:"daily_amount_limit_minor"`
	CooldownSeconds         int64      `json:"cooldown_seconds"`
	Generation              int64      `json:"generation"`
	Version                 int64      `json:"version"`
	Armed                   bool       `json:"armed"`
	ConsecutiveFailureCount int        `json:"consecutive_failure_count"`
	LastTriggeredAt         *time.Time `json:"last_triggered_at,omitempty"`
	LastSucceededAt         *time.Time `json:"last_succeeded_at,omitempty"`
	ConsentID               string     `json:"consent_id"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
}

type PaymentIntentReason string

const (
	PaymentIntentReasonManualTopUp PaymentIntentReason = "manual_top_up"
	PaymentIntentReasonAutoTopUp   PaymentIntentReason = "auto_top_up"
)

type PaymentIntentState string

const (
	PaymentIntentStateCreated        PaymentIntentState = "created"
	PaymentIntentStateProcessing     PaymentIntentState = "processing"
	PaymentIntentStateAuthorized     PaymentIntentState = "authorized"
	PaymentIntentStateRequiresAction PaymentIntentState = "requires_action"
	PaymentIntentStateUnknown        PaymentIntentState = "unknown"
	PaymentIntentStateSucceeded      PaymentIntentState = "succeeded"
	PaymentIntentStateFailed         PaymentIntentState = "failed"
	PaymentIntentStateCanceled       PaymentIntentState = "canceled"
)

type PaymentIntent struct {
	ID                           string              `json:"id"`
	AccountID                    string              `json:"account_id"`
	AmountMinor                  int64               `json:"amount_minor"`
	Currency                     Currency            `json:"currency"`
	Reason                       PaymentIntentReason `json:"reason"`
	PolicyGeneration             *int64              `json:"policy_generation,omitempty"`
	TriggerLedgerEntryID         string              `json:"trigger_ledger_entry_id,omitempty"`
	Provider                     string              `json:"provider"`
	PaymentMethodID              string              `json:"payment_method_id"`
	State                        PaymentIntentState  `json:"state"`
	IdempotencyKey               string              `json:"idempotency_key"`
	MerchantOrderReference       string              `json:"merchant_order_reference,omitempty"`
	ProviderTransactionReference string              `json:"provider_transaction_reference,omitempty"`
	CorrelationID                string              `json:"correlation_id"`
	CreatedAt                    time.Time           `json:"created_at"`
	UpdatedAt                    time.Time           `json:"updated_at"`
	CompletedAt                  *time.Time          `json:"completed_at,omitempty"`
}
