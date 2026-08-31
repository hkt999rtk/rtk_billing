package api

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/billingservice"
	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

type billingPersistence interface {
	EnsureBillingProfile(context.Context, string, time.Time) (billing.BillingProfile, bool, error)
	GetBillingProfile(context.Context, string) (billing.BillingProfile, error)
	PutBillingProfile(context.Context, billingstore.PutProfileInput) (billing.BillingProfile, error)
	CreatePricingVersion(context.Context, billingstore.CreatePricingVersionInput) (billing.PricingVersion, error)
	ActivatePricingVersion(context.Context, string, time.Time) (billing.PricingVersion, error)
	ActivePricingVersion(context.Context, time.Time, billing.Currency) (billing.PricingVersion, error)
	PutUsageFact(context.Context, billing.UsageFact) (billing.UsageFact, bool, error)
	ListUsageFacts(context.Context, string, time.Time, time.Time) ([]billing.UsageFact, error)
	ListInvoices(context.Context, string, billingstore.InvoiceFilter) (billingstore.InvoicePage, error)
	GetInvoice(context.Context, string, string) (billing.Invoice, error)
	GetInvoiceDocument(context.Context, string, string) (billingstore.InvoiceDocumentRecord, error)
	ListActivities(context.Context, string, billingstore.ActivityFilter) (billingstore.ActivityPage, error)
	GetActivity(context.Context, string, string) (billing.Activity, error)
}

type billingPeriodCloser interface {
	ClosePeriod(context.Context, billingservice.ClosePeriodInput) (billingservice.ClosePeriodResult, error)
}

type BillingAPIOptions struct {
	Store   billingPersistence
	Service billingPeriodCloser
	Now     func() time.Time
}

type billingRuntime struct {
	store   billingPersistence
	service billingPeriodCloser
	now     func() time.Time
}

func (s *Server) ConfigureBilling(options BillingAPIOptions) error {
	if options.Store == nil || options.Service == nil {
		return errors.New("billing store and service are required")
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	s.billing = &billingRuntime{store: options.Store, service: options.Service, now: options.Now}
	return nil
}

func (s *Server) requireBilling(c *gin.Context) bool {
	if s.billing == nil || s.billing.store == nil {
		writeError(c, http.StatusServiceUnavailable, "BILLING_NOT_CONFIGURED", "Billing service is not configured")
		return false
	}
	return true
}

type billingUsageResponse struct {
	PeriodStart  time.Time             `json:"period_start"`
	PeriodEnd    time.Time             `json:"period_end"`
	Currency     billing.Currency      `json:"currency"`
	Subtotal     int64                 `json:"subtotal_minor"`
	Tax          int64                 `json:"tax_minor"`
	Total        int64                 `json:"total_minor"`
	Lines        []billing.InvoiceLine `json:"lines"`
	Estimated    bool                  `json:"estimated"`
	FactCount    int                   `json:"fact_count"`
	UsageThrough *time.Time            `json:"usage_through,omitempty"`
}

type billingForecast struct {
	State                     string     `json:"state"`
	ProjectedPeriodTotalMinor *int64     `json:"projected_period_total_minor"`
	ProjectedRemainingMinor   *int64     `json:"projected_remaining_minor"`
	AverageDailyCostMinor     *int64     `json:"average_daily_cost_minor"`
	ObservationDays           int        `json:"observation_days"`
	UsageThrough              *time.Time `json:"usage_through,omitempty"`
	Confidence                string     `json:"confidence"`
	CalculatedAt              time.Time  `json:"calculated_at"`
}

func billingUsageContractLines(usage billingUsageResponse, pricingVersionID string) []gin.H {
	lines := make([]gin.H, 0, len(usage.Lines))
	for _, line := range usage.Lines {
		lines = append(lines, gin.H{
			"service_code": line.ServiceCode, "metric_code": line.MetricCode, "description": line.Description,
			"quantity": line.Quantity, "quantity_scale": line.QuantityScale, "unit": line.Unit,
			"estimated_cost_minor": line.TotalMinor, "currency": usage.Currency, "pricing_version_id": pricingVersionID,
		})
	}
	return lines
}

func (s *Server) currentBillingUsage(ctx context.Context, organizationID string) (billingUsageResponse, error) {
	now := s.billing.now().UTC()
	profile, _, err := s.billing.store.EnsureBillingProfile(ctx, organizationID, now)
	if err != nil {
		return billingUsageResponse{}, err
	}
	location, err := time.LoadLocation(profile.Timezone)
	if err != nil {
		return billingUsageResponse{}, billingstore.ErrConflict
	}
	localNow := now.In(location)
	startLocal := time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, location)
	endLocal := startLocal.AddDate(0, 1, 0)
	start, end := startLocal.UTC(), endLocal.UTC()
	if scope, ok := billingidentity.FromContext(ctx); ok && scope.CurrentPeriodStart.After(start) {
		start = scope.CurrentPeriodStart
	}
	return s.billingUsageForPeriod(ctx, organizationID, profile, start, end)
}

func (s *Server) billingUsageForPeriod(ctx context.Context, organizationID string, profile billing.BillingProfile, start, end time.Time) (billingUsageResponse, error) {
	if !end.After(start) {
		return billingUsageResponse{}, billingstore.ErrConflict
	}
	pricing, err := s.billing.store.ActivePricingVersion(ctx, start, billing.CurrencyTWD)
	if errors.Is(err, billingstore.ErrPricingUnavailable) {
		return billingUsageResponse{PeriodStart: start, PeriodEnd: end, Currency: billing.CurrencyTWD, Lines: []billing.InvoiceLine{}, Estimated: true}, nil
	}
	if err != nil {
		return billingUsageResponse{}, err
	}
	facts, err := s.billing.store.ListUsageFacts(ctx, organizationID, start, end)
	if err != nil {
		return billingUsageResponse{}, err
	}
	var usageThrough *time.Time
	for _, fact := range facts {
		if usageThrough == nil || fact.WindowEnd.After(*usageThrough) {
			value := fact.WindowEnd.UTC()
			usageThrough = &value
		}
	}
	draft, err := billing.BuildDraftInvoice(billing.Invoice{
		OrganizationID: organizationID, PricingVersionID: pricing.ID, Currency: billing.CurrencyTWD,
		PeriodStart: start, PeriodEnd: end, Recipient: profile,
	}, facts, pricing.Rates)
	if err != nil {
		return billingUsageResponse{}, err
	}
	return billingUsageResponse{PeriodStart: start, PeriodEnd: end, Currency: draft.Currency,
		Subtotal: draft.SubtotalMinor, Tax: draft.TaxMinor, Total: draft.TotalMinor, Lines: draft.Lines, Estimated: true,
		FactCount: len(facts), UsageThrough: usageThrough}, nil
}

func forecastBillingUsage(usage billingUsageResponse, calculatedAt time.Time) billingForecast {
	out := billingForecast{State: "unavailable", UsageThrough: usage.UsageThrough, Confidence: "low", CalculatedAt: calculatedAt.UTC()}
	if usage.UsageThrough == nil || usage.FactCount == 0 || usage.Total <= 0 || usage.UsageThrough.After(calculatedAt) {
		return out
	}
	elapsed := usage.UsageThrough.Sub(usage.PeriodStart)
	period := usage.PeriodEnd.Sub(usage.PeriodStart)
	if elapsed < 24*time.Hour || period <= 0 || elapsed > period {
		return out
	}
	observationDays := int(elapsed / (24 * time.Hour))
	projected, ok := checkedRatio(usage.Total, int64(period), int64(elapsed))
	if !ok || projected < usage.Total {
		return out
	}
	remaining := projected - usage.Total
	average, ok := checkedRatio(usage.Total, int64(24*time.Hour), int64(elapsed))
	if !ok || average <= 0 {
		return out
	}
	out.State, out.ObservationDays = "available", observationDays
	out.ProjectedPeriodTotalMinor, out.ProjectedRemainingMinor, out.AverageDailyCostMinor = &projected, &remaining, &average
	if observationDays >= 7 {
		out.Confidence = "medium"
	}
	return out
}

func checkedRatio(value, numerator, denominator int64) (int64, bool) {
	if value < 0 || numerator < 0 || denominator <= 0 {
		return 0, false
	}
	result := new(big.Int).Mul(big.NewInt(value), big.NewInt(numerator))
	result.Quo(result, big.NewInt(denominator))
	return result.Int64(), result.IsInt64()
}

func (s *Server) getBillingSummary(c *gin.Context) {
	if !s.requireBilling(c) {
		return
	}
	account, ok := s.paymentAccount(c)
	if !ok {
		return
	}
	usage, err := s.currentBillingUsage(c.Request.Context(), c.Param("orgId"))
	if err != nil {
		writeBillingError(c, err)
		return
	}
	page, err := s.billing.store.ListInvoices(c.Request.Context(), c.Param("orgId"), billingstore.InvoiceFilter{Limit: 1})
	if err != nil {
		writeBillingError(c, err)
		return
	}
	var latest any
	if len(page.Invoices) > 0 {
		latest = page.Invoices[0]
	}
	generatedAt := s.billing.now().UTC()
	forecast := forecastBillingUsage(usage, generatedAt)
	runway := billing.Runway{State: "unavailable", LookbackDays: forecast.ObservationDays, Confidence: forecast.Confidence, CalculatedAt: generatedAt}
	if forecast.State == "available" && forecast.AverageDailyCostMinor != nil && *forecast.AverageDailyCostMinor > 0 {
		projected := account.AvailableBalanceMinor / *forecast.AverageDailyCostMinor
		runway.State, runway.ProjectedDays, runway.AverageDailyCostMinor = "available", &projected, forecast.AverageDailyCostMinor
	}
	autoTopUp := gin.H{"state": "unconfigured"}
	if policy, err := s.payments.store.GetAutoTopUpPolicy(c.Request.Context(), account.ID); err == nil {
		state := "disabled"
		if policy.Enabled && policy.ConsecutiveFailureCount > 0 {
			state = "retrying"
		} else if policy.Enabled {
			state = "monitoring"
		}
		autoTopUp = gin.H{"state": state, "threshold_minor": policy.ThresholdMinor, "top_up_amount_minor": policy.TopUpAmountMinor, "consecutive_failure_count": policy.ConsecutiveFailureCount}
	} else if !errors.Is(err, billingstore.ErrNotFound) && !errors.Is(err, paymentstore.ErrNotFound) {
		writeBillingError(c, err)
		return
	}
	currentPeriod := gin.H{"period_start": usage.PeriodStart, "period_end": usage.PeriodEnd, "estimated_cost_minor": usage.Total, "amount_due_minor": 0, "next_invoice_at": usage.PeriodEnd, "currency": usage.Currency, "total_minor": usage.Total, "lines": usage.Lines, "estimated": true}
	c.JSON(http.StatusOK, gin.H{"account": account, "current_period": currentPeriod, "forecast": forecast, "auto_topup": autoTopUp, "runway": runway, "latest_invoice": latest, "generated_at": generatedAt, "calculated_at": generatedAt})
}

func (s *Server) getBillingUsage(c *gin.Context) {
	if !s.requireBilling(c) {
		return
	}
	start, err := optionalBillingTime(c.Query("period_start"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "INVALID_PERIOD_START", "period_start must be RFC3339")
		return
	}
	end, err := optionalBillingTime(c.Query("period_end"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "INVALID_PERIOD_END", "period_end must be RFC3339")
		return
	}
	var usage billingUsageResponse
	if start == nil && end == nil {
		usage, err = s.currentBillingUsage(c.Request.Context(), c.Param("orgId"))
	} else if start == nil || end == nil {
		writeError(c, http.StatusBadRequest, "BILLING_PERIOD_INCOMPLETE", "period_start and period_end must be provided together")
		return
	} else {
		profile, _, profileErr := s.billing.store.EnsureBillingProfile(c.Request.Context(), c.Param("orgId"), s.billing.now())
		if profileErr != nil {
			writeBillingError(c, profileErr)
			return
		}
		usage, err = s.billingUsageForPeriod(c.Request.Context(), c.Param("orgId"), profile, *start, *end)
	}
	if err != nil {
		writeBillingError(c, err)
		return
	}
	pricingVersionID := ""
	if pricing, pricingErr := s.billing.store.ActivePricingVersion(c.Request.Context(), usage.PeriodStart, billing.CurrencyTWD); pricingErr == nil {
		pricingVersionID = pricing.ID
	}
	c.JSON(http.StatusOK, gin.H{"usage": billingUsageContractLines(usage, pricingVersionID), "period_start": usage.PeriodStart, "period_end": usage.PeriodEnd, "currency": usage.Currency, "subtotal_minor": usage.Subtotal, "tax_minor": usage.Tax, "total_minor": usage.Total, "lines": usage.Lines, "estimated": usage.Estimated, "fact_count": usage.FactCount, "usage_through": usage.UsageThrough})
}

func (s *Server) listBillingInvoices(c *gin.Context) {
	if !s.requireBilling(c) {
		return
	}
	limit, offset := pagination(c)
	filter := billingstore.InvoiceFilter{State: billing.InvoiceState(c.Query("state")), InvoiceNumber: c.Query("invoice_number"), Limit: limit, Offset: offset}
	var err error
	if filter.PeriodStart, err = optionalBillingTime(c.Query("period_start")); err != nil {
		writeError(c, http.StatusBadRequest, "INVALID_PERIOD_START", "period_start must be RFC3339")
		return
	}
	if filter.PeriodEnd, err = optionalBillingTime(c.Query("period_end")); err != nil {
		writeError(c, http.StatusBadRequest, "INVALID_PERIOD_END", "period_end must be RFC3339")
		return
	}
	page, err := s.billing.store.ListInvoices(c.Request.Context(), c.Param("orgId"), filter)
	if err != nil {
		writeBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

func (s *Server) getBillingInvoice(c *gin.Context) {
	if !s.requireBilling(c) {
		return
	}
	invoice, err := s.billing.store.GetInvoice(c.Request.Context(), c.Param("orgId"), c.Param("invoiceId"))
	if err != nil {
		writeBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invoice": invoice})
}

func (s *Server) downloadBillingInvoicePDF(c *gin.Context) {
	if !s.requireBilling(c) {
		return
	}
	document, err := s.billing.store.GetInvoiceDocument(c.Request.Context(), c.Param("orgId"), c.Param("invoiceId"))
	if err != nil {
		writeBillingError(c, err)
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "invoice-"+c.Param("invoiceId")+".pdf"))
	c.Header("ETag", `"`+document.Metadata.SHA256+`"`)
	c.Header("Digest", "sha-256="+document.Metadata.SHA256)
	c.Data(http.StatusOK, document.Metadata.ContentType, document.Bytes)
}

func (s *Server) listBillingActivity(c *gin.Context) {
	if !s.requireBilling(c) {
		return
	}
	limit, offset := pagination(c)
	page, err := s.billing.store.ListActivities(c.Request.Context(), c.Param("orgId"), billingstore.ActivityFilter{
		State: billing.ActivityState(c.Query("state")), Type: c.Query("type"), Reference: c.Query("reference"), Limit: limit, Offset: offset,
	})
	if err != nil {
		writeBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

func (s *Server) getBillingActivity(c *gin.Context) {
	if !s.requireBilling(c) {
		return
	}
	activity, err := s.billing.store.GetActivity(c.Request.Context(), c.Param("orgId"), c.Param("activityId"))
	if err != nil {
		writeBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"activity": activity})
}

func (s *Server) getBillingProfile(c *gin.Context) {
	if !s.requireBilling(c) {
		return
	}
	profile, _, err := s.billing.store.EnsureBillingProfile(c.Request.Context(), c.Param("orgId"), s.billing.now())
	if err != nil {
		writeBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"billing_profile": profile, "profile": profile})
}

type putBillingProfileRequest struct {
	LegalName          string `json:"legal_name"`
	TaxIdentifier      string `json:"tax_identifier"`
	BillingAddress     string `json:"billing_address"`
	ContactEmail       string `json:"contact_email"`
	Locale             string `json:"locale"`
	Timezone           string `json:"timezone"`
	DeliveryPreference string `json:"delivery_preference"`
	Version            int64  `json:"version,omitempty"`
}

func (s *Server) putBillingProfile(c *gin.Context) {
	if !s.requireBilling(c) {
		return
	}
	var request putBillingProfileRequest
	if !bindPaymentStrict(c, &request) {
		return
	}
	if _, err := time.LoadLocation(request.Timezone); err != nil {
		writeError(c, http.StatusBadRequest, "INVALID_TIMEZONE", "timezone must be an IANA timezone")
		return
	}
	expectedVersion, ok := parseBillingVersion(c.GetHeader("If-Match"))
	if !ok {
		writeError(c, http.StatusPreconditionRequired, "BILLING_PROFILE_VERSION_REQUIRED", "If-Match billing profile version is required")
		return
	}
	if request.Version > 0 && request.Version != expectedVersion {
		writeError(c, http.StatusPreconditionFailed, "BILLING_PROFILE_VERSION_CONFLICT", "Billing profile version does not match If-Match")
		return
	}
	profile, err := s.billing.store.PutBillingProfile(c.Request.Context(), billingstore.PutProfileInput{
		OrganizationID: c.Param("orgId"), LegalName: request.LegalName, TaxIdentifier: request.TaxIdentifier,
		BillingAddress: request.BillingAddress, ContactEmail: request.ContactEmail, Locale: request.Locale,
		Timezone: request.Timezone, DeliveryPreference: request.DeliveryPreference, ExpectedVersion: expectedVersion, Now: s.billing.now(),
	})
	if err != nil {
		writeBillingError(c, err)
		return
	}
	c.Header("ETag", fmt.Sprintf(`"%d"`, profile.Version))
	c.JSON(http.StatusOK, gin.H{"billing_profile": profile, "profile": profile})
}

func (s *Server) exportBillingStatement(c *gin.Context) {
	if !s.requireBilling(c) {
		return
	}
	start, err := optionalBillingTime(c.Query("period_start"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "INVALID_PERIOD_START", "period_start must be RFC3339")
		return
	}
	end, err := optionalBillingTime(c.Query("period_end"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "INVALID_PERIOD_END", "period_end must be RFC3339")
		return
	}
	invoices := make([]billing.Invoice, 0)
	for offset := 0; ; offset += 100 {
		page, listErr := s.billing.store.ListInvoices(c.Request.Context(), c.Param("orgId"), billingstore.InvoiceFilter{PeriodStart: start, PeriodEnd: end, Limit: 100, Offset: offset})
		if listErr != nil {
			writeBillingError(c, listErr)
			return
		}
		invoices = append(invoices, page.Invoices...)
		if len(invoices) >= page.Page.Total || len(page.Invoices) == 0 {
			break
		}
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="billing-statement.csv"`)
	writer := csv.NewWriter(c.Writer)
	_ = writer.Write([]string{"invoice_number", "period_start", "period_end", "currency", "total_minor", "state", "issued_at"})
	for _, invoice := range invoices {
		issuedAt := ""
		if invoice.IssuedAt != nil {
			issuedAt = invoice.IssuedAt.UTC().Format(time.RFC3339)
		}
		_ = writer.Write([]string{invoice.InvoiceNumber, invoice.PeriodStart.UTC().Format(time.RFC3339), invoice.PeriodEnd.UTC().Format(time.RFC3339), string(invoice.Currency), strconv.FormatInt(invoice.TotalMinor, 10), string(invoice.State), issuedAt})
	}
	writer.Flush()
}

type createPricingVersionRequest struct {
	PlanKey       string                `json:"plan_key"`
	Version       int64                 `json:"version"`
	Currency      billing.Currency      `json:"currency"`
	EffectiveFrom time.Time             `json:"effective_from"`
	Rates         []billing.PricingRate `json:"rates"`
}

func (s *Server) createBillingPricingVersion(c *gin.Context) {
	if !s.requireInternalBilling(c) {
		return
	}
	var request createPricingVersionRequest
	if !bindPaymentStrict(c, &request) {
		return
	}
	version, err := s.billing.store.CreatePricingVersion(c.Request.Context(), billingstore.CreatePricingVersionInput{
		PlanKey: request.PlanKey, Version: request.Version, Currency: request.Currency,
		EffectiveFrom: request.EffectiveFrom, Rates: request.Rates, CreatedBy: "internal-api", Now: s.billing.now(),
	})
	if err != nil {
		writeBillingError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"pricing_version": version})
}

func (s *Server) activateBillingPricingVersion(c *gin.Context) {
	if !s.requireInternalBilling(c) {
		return
	}
	version, err := s.billing.store.ActivatePricingVersion(c.Request.Context(), c.Param("pricingVersionId"), s.billing.now())
	if err != nil {
		writeBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"pricing_version": version})
}

func (s *Server) putBillingUsageFact(c *gin.Context) {
	if !s.requireInternalBilling(c) {
		return
	}
	var fact billing.UsageFact
	if !bindPaymentStrict(c, &fact) {
		return
	}
	stored, created, err := s.billing.store.PutUsageFact(c.Request.Context(), fact)
	if err != nil {
		writeBillingError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, gin.H{"usage_fact": stored, "duplicate": !created})
}

type closeBillingPeriodRequest struct {
	OrganizationID string    `json:"organization_id"`
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
	DueAt          time.Time `json:"due_at"`
}

func (s *Server) closeBillingPeriod(c *gin.Context) {
	if !s.requireInternalBilling(c) {
		return
	}
	var request closeBillingPeriodRequest
	if !bindPaymentStrict(c, &request) {
		return
	}
	result, err := s.billing.service.ClosePeriod(c.Request.Context(), billingservice.ClosePeriodInput{
		OrganizationID: request.OrganizationID, PeriodStart: request.PeriodStart, PeriodEnd: request.PeriodEnd,
		DueAt: request.DueAt, RequestID: strings.TrimSpace(c.GetHeader("X-Request-ID")),
	})
	if err != nil {
		writeBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (s *Server) requireInternalBilling(c *gin.Context) bool {
	return s.requireBilling(c)
}

func optionalBillingTime(value string) (*time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func parseBillingVersion(value string) (int64, bool) {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	version, err := strconv.ParseInt(value, 10, 64)
	return version, err == nil && version > 0
}

func writeBillingError(c *gin.Context, err error) {
	if writeOwnershipError(c, err) {
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55000" && pgErr.ConstraintName == "billing_handoff_commit_barrier" {
		writeError(c, http.StatusConflict, "BILLING_OWNERSHIP_HANDOFF_FENCED", "Billing changes are paused during ownership handoff")
		return
	}
	switch {
	case errors.Is(err, billing.ErrProfileConfigurationRequired):
		writeError(c, http.StatusConflict, "BILLING_PROFILE_CONFIGURATION_REQUIRED", "The current owner must configure a billing profile")
	case errors.Is(err, billingstore.ErrNotFound):
		writeError(c, http.StatusNotFound, "BILLING_RESOURCE_NOT_FOUND", "Billing resource was not found")
	case errors.Is(err, billingstore.ErrConflict), errors.Is(err, billing.ErrInvalidInvoice), errors.Is(err, billing.ErrInvalidScale), errors.Is(err, billing.ErrRateNotFound):
		writeError(c, http.StatusConflict, "BILLING_CONFLICT", "Billing request conflicts with the current state")
	case errors.Is(err, billingstore.ErrIncomplete), errors.Is(err, billingstore.ErrPricingUnavailable):
		writeError(c, http.StatusUnprocessableEntity, "BILLING_INCOMPLETE", "Billing evidence is incomplete")
	case errors.Is(err, billingstore.ErrInvoiceImmutable), errors.Is(err, billing.ErrInvoiceIssued):
		writeError(c, http.StatusConflict, "INVOICE_IMMUTABLE", "Issued invoice is immutable")
	default:
		writeError(c, http.StatusInternalServerError, "BILLING_INTERNAL_ERROR", "Billing operation failed")
	}
}
