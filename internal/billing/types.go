package billing

import "time"

type Currency string

const CurrencyTWD Currency = "TWD"

type RoundingMode string

const (
	RoundingHalfUp RoundingMode = "half_up"
	RoundingDown   RoundingMode = "down"
	RoundingUp     RoundingMode = "up"
)

type InvoiceState string

const (
	InvoiceStateDraft            InvoiceState = "draft"
	InvoiceStateIssued           InvoiceState = "issued"
	InvoiceStateSettled          InvoiceState = "settled"
	InvoiceStatePartiallySettled InvoiceState = "partially_settled"
	InvoiceStateOverdue          InvoiceState = "overdue"
	InvoiceStateVoid             InvoiceState = "void"
)

type BillingProfile struct {
	OwnershipVersion      *int64    `json:"ownership_version,omitempty"`
	RequiresConfiguration bool      `json:"requires_configuration"`
	OrganizationID        string    `json:"organization_id,omitempty"`
	LegalName             string    `json:"legal_name"`
	TaxIdentifier         string    `json:"tax_identifier,omitempty"`
	BillingAddress        string    `json:"billing_address,omitempty"`
	ContactEmail          string    `json:"contact_email,omitempty"`
	Locale                string    `json:"locale"`
	Timezone              string    `json:"timezone"`
	DeliveryPreference    string    `json:"delivery_preference"`
	Version               int64     `json:"version"`
	CreatedAt             time.Time `json:"created_at,omitempty"`
	UpdatedAt             time.Time `json:"updated_at,omitempty"`
}

type PricingVersion struct {
	ID             string        `json:"id"`
	PlanKey        string        `json:"plan_key"`
	Version        int64         `json:"version"`
	Currency       Currency      `json:"currency"`
	Status         string        `json:"status"`
	EffectiveFrom  time.Time     `json:"effective_from"`
	EffectiveUntil *time.Time    `json:"effective_until,omitempty"`
	Rates          []PricingRate `json:"rates"`
	ActivatedAt    *time.Time    `json:"activated_at,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
}

type PricingRate struct {
	ID                 string       `json:"id"`
	PricingVersionID   string       `json:"pricing_version_id"`
	ServiceCode        string       `json:"service_code"`
	MetricCode         string       `json:"metric_code"`
	Description        string       `json:"description"`
	Unit               string       `json:"unit"`
	UnitPriceMinor     int64        `json:"unit_price_minor"`
	UnitPriceScale     int          `json:"unit_price_scale"`
	RoundingMode       RoundingMode `json:"rounding_mode"`
	TaxRateBasisPoints int64        `json:"tax_rate_basis_points"`
}

type UsageFact struct {
	ID             string    `json:"id,omitempty"`
	UsageID        string    `json:"usage_id"`
	OrganizationID string    `json:"organization_id"`
	ServiceCode    string    `json:"service_code"`
	MetricCode     string    `json:"metric_code"`
	Quantity       int64     `json:"quantity"`
	QuantityScale  int       `json:"quantity_scale"`
	Unit           string    `json:"unit"`
	WindowStart    time.Time `json:"window_start"`
	WindowEnd      time.Time `json:"window_end"`
	Source         string    `json:"source"`
	SourceSHA256   string    `json:"source_sha256"`
}

type InvoiceLine struct {
	ID             string       `json:"id"`
	PricingRateID  string       `json:"pricing_rate_id,omitempty"`
	ServiceCode    string       `json:"service_code"`
	MetricCode     string       `json:"metric_code"`
	Description    string       `json:"description"`
	Quantity       int64        `json:"quantity"`
	QuantityScale  int          `json:"quantity_scale"`
	Unit           string       `json:"unit"`
	UnitPriceMinor int64        `json:"unit_price_minor"`
	UnitPriceScale int          `json:"unit_price_scale"`
	SubtotalMinor  int64        `json:"subtotal_minor"`
	TaxMinor       int64        `json:"tax_minor"`
	TotalMinor     int64        `json:"total_minor"`
	RoundingMode   RoundingMode `json:"rounding_mode"`
	UsageFactRefs  []string     `json:"usage_fact_refs"`
}

type InvoiceDocument struct {
	ContentType     string    `json:"content_type"`
	ByteLength      int64     `json:"byte_length"`
	SHA256          string    `json:"sha256"`
	RendererVersion string    `json:"renderer_version,omitempty"`
	InvoiceVersion  int64     `json:"invoice_version"`
	GeneratedAt     time.Time `json:"generated_at"`
}

type Invoice struct {
	ID                   string           `json:"id"`
	InvoiceNumber        string           `json:"invoice_number"`
	OrganizationID       string           `json:"organization_id"`
	AccountID            string           `json:"account_id,omitempty"`
	PeriodID             string           `json:"period_id,omitempty"`
	PricingVersionID     string           `json:"pricing_version_id"`
	Currency             Currency         `json:"currency"`
	State                InvoiceState     `json:"state"`
	PeriodStart          time.Time        `json:"period_start"`
	PeriodEnd            time.Time        `json:"period_end"`
	SubtotalMinor        int64            `json:"subtotal_minor"`
	TaxMinor             int64            `json:"tax_minor"`
	TotalMinor           int64            `json:"total_minor"`
	AmountSettledMinor   int64            `json:"amount_settled_minor"`
	AmountDueMinor       int64            `json:"amount_due_minor"`
	Recipient            BillingProfile   `json:"recipient"`
	Lines                []InvoiceLine    `json:"lines"`
	Document             *InvoiceDocument `json:"document,omitempty"`
	SettlementLedgerID   string           `json:"settlement_ledger_id,omitempty"`
	SettlementActivityID string           `json:"settlement_activity_id,omitempty"`
	IssuedAt             *time.Time       `json:"issued_at,omitempty"`
	DueAt                *time.Time       `json:"due_at,omitempty"`
	SettledAt            *time.Time       `json:"settled_at,omitempty"`
	Version              int64            `json:"version"`
	CreatedAt            time.Time        `json:"created_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
}

type ActivityState string

const (
	ActivityActionRequired        ActivityState = "action_required"
	ActivityProcessing            ActivityState = "processing"
	ActivityPendingReconciliation ActivityState = "pending_reconciliation"
	ActivityCompleted             ActivityState = "completed"
	ActivityFailed                ActivityState = "failed"
	ActivityUnavailable           ActivityState = "unavailable"
)

type ActivityStep struct {
	Kind              string    `json:"kind"`
	State             string    `json:"state"`
	OccurredAt        time.Time `json:"occurred_at"`
	CustomerReference string    `json:"customer_reference"`
	MessageKey        string    `json:"message_key,omitempty"`
}

type Activity struct {
	ID                 string         `json:"id"`
	CustomerReference  string         `json:"customer_reference"`
	Type               string         `json:"type"`
	State              ActivityState  `json:"state"`
	Currency           Currency       `json:"currency"`
	AmountMinor        int64          `json:"amount_minor"`
	BalanceEffect      string         `json:"balance_effect"`
	Action             string         `json:"action"`
	MessageKey         string         `json:"message_key,omitempty"`
	RetryScheduled     bool           `json:"retry_scheduled"`
	NextRetryAt        *time.Time     `json:"next_retry_at,omitempty"`
	PaymentMethodLabel string         `json:"payment_method_label,omitempty"`
	OccurredAt         time.Time      `json:"occurred_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	Steps              []ActivityStep `json:"steps"`
}

type Runway struct {
	State                 string    `json:"state"`
	ProjectedDays         *int64    `json:"projected_days"`
	AverageDailyCostMinor *int64    `json:"average_daily_cost_minor"`
	LookbackDays          int       `json:"lookback_days"`
	Confidence            string    `json:"confidence"`
	CalculatedAt          time.Time `json:"calculated_at"`
}
