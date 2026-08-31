package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/jackc/pgx/v5/pgtype"
)

// This credential belongs only to Account Manager's durable coordinator, not
// Cloud Admin, a tenant, usage ingestion or a payment worker. The coordinator
// authenticates participant sessions and verifies its own committed/canceled
// decisions before forwarding them. It cannot mint settlement/hold-release
// evidence or initialize historical responsibility through this interface.
type handoffPersistence interface {
	PrepareOwnershipHandoff(context.Context, paymentstore.PrepareOwnershipHandoffInput) (paymentstore.OwnershipHandoff, error)
	GetHandoffSettlementStatus(context.Context, paymentstore.HandoffScope) (paymentstore.HandoffSettlementStatus, error)
	ConfirmHandoffSnapshot(context.Context, paymentstore.ConfirmHandoffSnapshotInput) (paymentstore.HandoffSettlementStatus, error)
	AuthorizeHandoffCommit(context.Context, paymentstore.AuthorizeHandoffCommitInput) (paymentstore.HandoffCommitAuthorization, error)
	FinalizeOwnershipHandoff(context.Context, paymentstore.FinalizeHandoffInput) (paymentstore.HandoffProtocolAck, error)
	BeginOwnershipHandoffAbort(context.Context, paymentstore.BeginHandoffAbortInput) (paymentstore.HandoffProtocolAck, error)
}

type HandoffAPIOptions struct {
	Token string
	Store handoffPersistence
}
type handoffRuntime struct {
	token string
	store handoffPersistence
}

// Configure once before serving. Absent configuration means route-level 404;
// incomplete configuration is an error, never an unauthenticated fallback.
func (s *Server) ConfigureHandoff(in HandoffAPIOptions) error {
	in.Token = strings.TrimSpace(in.Token)
	if s.handoff != nil || in.Store == nil || len(in.Token) < 32 || strings.ContainsAny(in.Token, " \t\r\n") || in.Token == s.serviceToken || in.Token == s.internalToken {
		return fmt.Errorf("handoff requires a store and a new dedicated credential of at least 32 characters")
	}
	if s.payments != nil && (in.Token == s.payments.billingDebitToken || in.Token == string(s.payments.simulatorCallbackSecret)) {
		return fmt.Errorf("handoff credential must be distinct from payment credentials")
	}
	s.handoff = &handoffRuntime{token: in.Token, store: in.Store}
	cloud := s.router.Group("/v1/internal/billing/clouds/:orgId", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, requireBearerToken(in.Token))
	if reader, ok := in.Store.(cloudDeletionPreflightReader); ok {
		cloud.GET("/deletion-preflight", cloudDeletionPreflightHandler(reader))
	}
	r := cloud.Group("/ownership-handoffs/:operationId")
	r.POST("/prepare", s.prepareHandoff)
	r.GET("/settlement", s.getHandoffSettlement)
	r.POST("/confirm", s.confirmHandoff)
	r.POST("/authorize-commit", s.authorizeHandoffCommit)
	r.POST("/finalize", s.finalizeHandoff)
	r.POST("/abort", s.beginHandoffAbort)
	return nil
}

func handoffUUID(value string) bool {
	var id pgtype.UUID
	return id.Scan(value) == nil && id.Valid && id.Bytes != [16]byte{} && id.String() == value
}

func handoffScope(c *gin.Context) (paymentstore.HandoffScope, bool) {
	raw := c.GetHeader("X-Billing-Ownership-Version")
	version, err := strconv.ParseInt(raw, 10, 64)
	in := paymentstore.HandoffScope{OrganizationID: c.Param("orgId"), OperationID: c.Param("operationId"), OwnershipVersion: version}
	if err != nil || version < 1 || strconv.FormatInt(version, 10) != raw || !handoffUUID(in.OrganizationID) || !handoffUUID(in.OperationID) {
		writeError(c, http.StatusBadRequest, "BILLING_HANDOFF_CONTEXT_INVALID", "Canonical cloud, operation and source ownership version are required")
		return in, false
	}
	return in, true
}

func bindHandoff(c *gin.Context, out any) bool {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(c, http.StatusBadRequest, "BILLING_HANDOFF_REQUEST_INVALID", "A bounded JSON object is required")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		writeError(c, http.StatusBadRequest, "BILLING_HANDOFF_REQUEST_INVALID", "Invalid handoff request")
		return false
	}
	return true
}

func handoffReply(c *gin.Context, scope paymentstore.HandoffScope, field string, result any, err error) {
	if err != nil {
		switch {
		case errors.Is(err, paymentstore.ErrNotFound):
			writeError(c, http.StatusNotFound, "BILLING_HANDOFF_NOT_FOUND", "Handoff scope not found")
		case errors.Is(err, paymentstore.ErrHandoffParticipant):
			writeError(c, http.StatusForbidden, "BILLING_HANDOFF_PARTICIPANT_REQUIRED", "An authenticated source or target is required")
		case errors.Is(err, paymentstore.ErrOwnershipVersionConflict):
			writeError(c, http.StatusConflict, "BILLING_HANDOFF_VERSION_CONFLICT", "Ownership context has changed")
		case errors.Is(err, paymentstore.ErrSettlementEvidenceStale), errors.Is(err, paymentstore.ErrHandoffNotConfirmable):
			writeError(c, http.StatusConflict, "BILLING_HANDOFF_SNAPSHOT_CONFLICT", "Fresh settled evidence and exact participant confirmations are required")
		case errors.Is(err, paymentstore.ErrOwnershipEvidenceMissing), errors.Is(err, paymentstore.ErrHandoffFenced),
			errors.Is(err, paymentstore.ErrConflict), errors.Is(err, paymentstore.ErrIdempotencyConflict), errors.Is(err, paymentstore.ErrAccountClosed):
			writeError(c, http.StatusConflict, "BILLING_HANDOFF_CONFLICT", "Handoff state, evidence or idempotency payload conflicts")
		default:
			writeError(c, http.StatusServiceUnavailable, "BILLING_HANDOFF_UNAVAILABLE", "Handoff evidence is unavailable; retain the fence and retry")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"cloud_id": scope.OrganizationID, "operation_id": scope.OperationID,
		"ownership_version": scope.OwnershipVersion, field: result})
}

func (s *Server) prepareHandoff(c *gin.Context) {
	scope, ok := handoffScope(c)
	if !ok {
		return
	}
	var req struct {
		SourceUserID string    `json:"source_user_id"`
		TargetUserID string    `json:"target_user_id"`
		Cutoff       time.Time `json:"cutoff"`
	}
	if !bindHandoff(c, &req) {
		return
	}
	if !handoffUUID(req.SourceUserID) || !handoffUUID(req.TargetUserID) || req.SourceUserID == req.TargetUserID || req.Cutoff.IsZero() || req.Cutoff.After(time.Now().UTC()) {
		writeError(c, http.StatusBadRequest, "BILLING_HANDOFF_REQUEST_INVALID", "Valid participants and a persisted cutoff are required")
		return
	}
	result, err := s.handoff.store.PrepareOwnershipHandoff(c.Request.Context(), paymentstore.PrepareOwnershipHandoffInput{
		OperationID: scope.OperationID, OrganizationID: scope.OrganizationID, OwnershipVersion: scope.OwnershipVersion,
		SourceUserID: req.SourceUserID, TargetUserID: req.TargetUserID, Cutoff: req.Cutoff})
	handoffReply(c, scope, "operation", result, err)
}

func (s *Server) getHandoffSettlement(c *gin.Context) {
	scope, ok := handoffScope(c)
	if !ok {
		return
	}
	result, err := s.handoff.store.GetHandoffSettlementStatus(c.Request.Context(), scope)
	handoffReply(c, scope, "settlement", result, err)
}

func (s *Server) confirmHandoff(c *gin.Context) {
	scope, ok := handoffScope(c)
	if !ok {
		return
	}
	var req struct {
		UserID          string           `json:"user_id"`
		SnapshotVersion int64            `json:"snapshot_version"`
		BalanceMinor    *int64           `json:"balance_minor"`
		Currency        billing.Currency `json:"currency"`
	}
	if !bindHandoff(c, &req) {
		return
	}
	if !handoffUUID(req.UserID) || req.SnapshotVersion < 2 || req.BalanceMinor == nil || *req.BalanceMinor < 0 || req.Currency != billing.CurrencyTWD {
		writeError(c, http.StatusBadRequest, "BILLING_HANDOFF_REQUEST_INVALID", "Exact nonnegative amount, currency, snapshot version and participant are required")
		return
	}
	result, err := s.handoff.store.ConfirmHandoffSnapshot(c.Request.Context(), paymentstore.ConfirmHandoffSnapshotInput{
		Scope: scope, UserID: req.UserID, SnapshotVersion: req.SnapshotVersion, BalanceMinor: *req.BalanceMinor, Currency: req.Currency})
	handoffReply(c, scope, "settlement", result, err)
}

func (s *Server) authorizeHandoffCommit(c *gin.Context) {
	scope, ok := handoffScope(c)
	if !ok {
		return
	}
	var req struct {
		AuthorizationID string `json:"authorization_id"`
		SnapshotVersion int64  `json:"snapshot_version"`
	}
	if !bindHandoff(c, &req) {
		return
	}
	if !handoffUUID(req.AuthorizationID) || req.SnapshotVersion < 2 {
		writeError(c, http.StatusBadRequest, "BILLING_HANDOFF_REQUEST_INVALID", "Authorization ID and exact snapshot version are required")
		return
	}
	result, err := s.handoff.store.AuthorizeHandoffCommit(c.Request.Context(), paymentstore.AuthorizeHandoffCommitInput{Scope: scope, AuthorizationID: req.AuthorizationID, SnapshotVersion: req.SnapshotVersion})
	handoffReply(c, scope, "authorization", result, err)
}

func (s *Server) finalizeHandoff(c *gin.Context) {
	scope, ok := handoffScope(c)
	if !ok {
		return
	}
	var req struct {
		AuthorizationID           string    `json:"authorization_id"`
		CommittedOwnerUserID      string    `json:"committed_owner_user_id"`
		CommittedOwnershipVersion int64     `json:"committed_ownership_version"`
		CommittedAt               time.Time `json:"committed_at"`
		AMCommitSHA256            string    `json:"am_commit_sha256"`
	}
	if !bindHandoff(c, &req) {
		return
	}
	result, err := s.handoff.store.FinalizeOwnershipHandoff(c.Request.Context(), paymentstore.FinalizeHandoffInput{
		Scope: scope, AuthorizationID: req.AuthorizationID, CommittedOwnerUserID: req.CommittedOwnerUserID,
		CommittedOwnershipVersion: req.CommittedOwnershipVersion, CommittedAt: req.CommittedAt, AMCommitSHA256: req.AMCommitSHA256})
	handoffReply(c, scope, "operation", result, err)
}

func (s *Server) beginHandoffAbort(c *gin.Context) {
	scope, ok := handoffScope(c)
	if !ok {
		return
	}
	var req struct {
		CancellationID       string `json:"cancellation_id"`
		AMCancellationSHA256 string `json:"am_cancellation_sha256"`
		AuthorizationID      string `json:"authorization_id"`
	}
	if !bindHandoff(c, &req) {
		return
	}
	result, err := s.handoff.store.BeginOwnershipHandoffAbort(c.Request.Context(), paymentstore.BeginHandoffAbortInput{
		Scope: scope, CancellationID: req.CancellationID, AMCancellationSHA256: req.AMCancellationSHA256, AuthorizationID: req.AuthorizationID})
	handoffReply(c, scope, "operation", result, err)
}
