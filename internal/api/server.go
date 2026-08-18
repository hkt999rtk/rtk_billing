package api

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hkt999rtk/rtk_billing/internal/accessstore"
	"github.com/hkt999rtk/rtk_billing/internal/auditstore"
)

type AuditEventInput struct {
	EventType, OrganizationID, ActorType, ActorID string
	SubjectType, SubjectID, RequestID             string
	Payload                                       map[string]any
}

type auditPersistence interface {
	CreateAuditEvent(context.Context, AuditEventInput) error
}

type accessPersistence interface {
	GetOrCreate(context.Context, string) (accessstore.State, error)
	Put(context.Context, string, string, string, string, int64) (accessstore.State, error)
}

type Server struct {
	router        *gin.Engine
	serviceToken  string
	internalToken string
	audit         auditPersistence
	access        accessPersistence
	payments      *paymentRuntime
	billing       *billingRuntime
}

type Options struct {
	ServiceToken  string
	InternalToken string
	Audit         auditPersistence
	Access        accessPersistence
}

func New(options Options) (*Server, error) {
	options.ServiceToken = strings.TrimSpace(options.ServiceToken)
	options.InternalToken = strings.TrimSpace(options.InternalToken)
	if len(options.ServiceToken) < 32 || len(options.InternalToken) < 32 ||
		options.ServiceToken == options.InternalToken || options.Audit == nil || options.Access == nil {
		return nil, ErrInvalidServerOptions
	}
	s := &Server{serviceToken: options.ServiceToken, internalToken: options.InternalToken, audit: options.Audit, access: options.Access}
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/readyz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ready"}) })
	r.POST("/v1/payment-webhooks/:provider", s.handlePaymentWebhook)
	r.POST("/v1/internal/payment-simulator/setup-callback", s.handlePaymentSimulatorSetupCallback)

	org := r.Group("/v1/orgs/:orgId", s.requireServiceToken(), s.requireTenantContext(), s.requireBillingAccess())
	s.registerTenantRoutes(org)
	r.POST("/v1/internal/billing/debits", s.handleInternalBillingDebit)
	internal := r.Group("/v1/internal", s.requireInternalToken())
	internal.POST("/billing/pricing-versions", s.createBillingPricingVersion)
	internal.POST("/billing/pricing-versions/:pricingVersionId/activate", s.activateBillingPricingVersion)
	internal.POST("/billing/usage-facts", s.putBillingUsageFact)
	internal.POST("/billing/periods/close", s.closeBillingPeriod)
	internal.GET("/billing/access/:orgId", s.getBillingAccess)
	internal.PUT("/billing/access/:orgId", s.putBillingAccess)
	s.router = r
	return s, nil
}

var ErrInvalidServerOptions = &serverError{"distinct service/internal tokens plus audit and access stores are required"}

type serverError struct{ message string }

func (e *serverError) Error() string { return e.message }

func (s *Server) Router() http.Handler { return s.router }

func (s *Server) registerTenantRoutes(org *gin.RouterGroup) {
	org.GET("/billing/account", s.requirePermission("billing_account.read"), s.getBillingAccount)
	org.GET("/billing/summary", s.requirePermission("billing_summary.read"), s.getBillingSummary)
	org.GET("/billing/usage", s.requirePermission("billing_usage.read"), s.getBillingUsage)
	org.GET("/billing/invoices", s.requirePermission("invoice.read"), s.listBillingInvoices)
	org.GET("/billing/invoices/:invoiceId", s.requirePermission("invoice.read"), s.getBillingInvoice)
	org.GET("/billing/invoices/:invoiceId/pdf", s.requirePermission("invoice_document.read"), s.downloadBillingInvoicePDF)
	org.GET("/billing/activity", s.requirePermission("billing_activity.read"), s.listBillingActivity)
	org.GET("/billing/activity/:activityId", s.requirePermission("billing_activity.read"), s.getBillingActivity)
	org.GET("/billing/profile", s.requirePermission("billing_profile.read"), s.getBillingProfile)
	org.PUT("/billing/profile", s.requirePermission("billing_profile.manage"), s.putBillingProfile)
	org.GET("/billing/statements", s.requirePermission("billing_statement.export"), s.exportBillingStatement)
	org.GET("/billing/ledger", s.requirePermission("billing_ledger.read"), s.listBillingLedger)
	org.GET("/payment-methods", s.requirePermission("payment_method.read"), s.listPaymentMethods)
	org.POST("/payment-methods/setup", s.requirePermission("payment_method.manage"), s.setupPaymentMethod)
	org.DELETE("/payment-methods/:methodId", s.requirePermission("payment_method.manage"), s.revokePaymentMethod)
	org.GET("/auto-topup", s.requirePermission("auto_topup.read"), s.getAutoTopUpPolicy)
	org.PUT("/auto-topup", s.requirePermission("auto_topup.manage"), s.putAutoTopUpPolicy)
	org.DELETE("/auto-topup", s.requirePermission("auto_topup.manage"), s.disableAutoTopUpPolicy)
	org.POST("/topups", s.requirePermission("payment_intent.create"), s.createManualTopUp)
	org.GET("/payment-intents", s.requirePermission("payment_intent.read"), s.listPaymentIntents)
	org.GET("/payment-intents/:intentId", s.requirePermission("payment_intent.read"), s.getPaymentIntent)
}

func (s *Server) requireServiceToken() gin.HandlerFunc {
	return requireBearerToken(s.serviceToken)
}

func (s *Server) requireInternalToken() gin.HandlerFunc {
	return requireBearerToken(s.internalToken)
}

func requireBearerToken(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		provided, expected := strings.TrimSpace(c.GetHeader("Authorization")), "Bearer "+token
		if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			writeError(c, http.StatusUnauthorized, "unauthorized", "Unauthorized")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Server) requireTenantContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		actorType := strings.TrimSpace(c.GetHeader("X-Billing-Actor-Type"))
		actorID := strings.TrimSpace(c.GetHeader("X-Billing-Actor-ID"))
		requestID := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if actorType != "brand_cloud_user" || !validContextValue(actorID, 200) || !validContextValue(requestID, 128) {
			writeError(c, http.StatusBadRequest, "BILLING_CONTEXT_REQUIRED", "Trusted billing actor and request context are required")
			c.Abort()
			return
		}
		c.Next()
	}
}

func validContextValue(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func (s *Server) requirePermission(permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, value := range strings.Split(c.GetHeader("X-Billing-Permissions"), ",") {
			if strings.TrimSpace(value) == permission {
				c.Next()
				return
			}
		}
		writeError(c, http.StatusForbidden, "forbidden", "Billing permission denied")
		c.Abort()
	}
}

func (s *Server) requireBillingAccess() gin.HandlerFunc {
	return func(c *gin.Context) {
		state, err := s.access.GetOrCreate(c.Request.Context(), c.Param("orgId"))
		if err != nil {
			writeError(c, http.StatusInternalServerError, "BILLING_ACCESS_UNAVAILABLE", "Billing access state is unavailable")
			c.Abort()
			return
		}
		if state.State == "closed" || state.State == "suspended" || (state.State == "read_only" && c.Request.Method != http.MethodGet) {
			writeError(c, http.StatusForbidden, "BILLING_ACCESS_RESTRICTED", "Billing access is restricted")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Server) getBillingAccess(c *gin.Context) {
	state, err := s.access.GetOrCreate(c.Request.Context(), c.Param("orgId"))
	if err != nil {
		writeError(c, 500, "BILLING_ACCESS_UNAVAILABLE", "Billing access state is unavailable")
		return
	}
	c.JSON(http.StatusOK, gin.H{"access": state})
}

func (s *Server) putBillingAccess(c *gin.Context) {
	var request struct {
		State      string `json:"state"`
		ReasonCode string `json:"reason_code"`
		Version    int64  `json:"version"`
	}
	if !bindPaymentStrict(c, &request) {
		return
	}
	state, err := s.access.Put(c.Request.Context(), c.Param("orgId"), request.State, request.ReasonCode, paymentActorID(c), request.Version)
	if err != nil {
		writeError(c, http.StatusConflict, "BILLING_ACCESS_CONFLICT", "Billing access state conflicts with the current version")
		return
	}
	c.JSON(http.StatusOK, gin.H{"access": state})
}

func pagination(c *gin.Context) (int, int) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit < 1 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
func writeStoreError(c *gin.Context, _ error) {
	writeError(c, http.StatusInternalServerError, "BILLING_INTERNAL_ERROR", "Billing operation failed")
}

type AuditAdapter struct{ Store *auditstore.Store }

func (a AuditAdapter) CreateAuditEvent(ctx context.Context, in AuditEventInput) error {
	return a.Store.Create(ctx, auditstore.Event{EventType: in.EventType, OrganizationID: in.OrganizationID, ActorType: in.ActorType, ActorID: in.ActorID, SubjectType: in.SubjectType, SubjectID: in.SubjectID, RequestID: in.RequestID, Payload: in.Payload})
}
