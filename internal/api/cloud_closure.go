package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

// Coordinator commands only. Collector settlement, provider revocation and
// cancellation-release acknowledgments are deliberately not HTTP capabilities.
type cloudClosurePersistence interface {
	PrepareCloudClosure(context.Context, paymentstore.PrepareCloudClosureInput) (paymentstore.CloudClosure, error)
	GetCloudClosureStatus(context.Context, paymentstore.CloudClosureScope) (paymentstore.CloudClosureStatus, error)
	CloseCloud(context.Context, paymentstore.CloseCloudInput) (paymentstore.CloudClosureAck, error)
	CancelCloudClosure(context.Context, paymentstore.CloudClosureScope, string, string) (paymentstore.CloudClosure, error)
	RetireCloudClose(context.Context, paymentstore.CloseCloudInput) (paymentstore.CloudCloseResolution, error)
}

func closureScope(c *gin.Context) (paymentstore.CloudClosureScope, bool) {
	handoff, ok := handoffScope(c)
	in := paymentstore.CloudClosureScope{CloudPreflightScope: paymentstore.CloudPreflightScope{OrganizationID: handoff.OrganizationID, OwnershipVersion: handoff.OwnershipVersion, OwnerUserID: c.GetHeader("X-Billing-Owner-User-ID")}, OperationID: handoff.OperationID}
	if !ok {
		return in, false
	}
	if !handoffUUID(in.OwnerUserID) || len(c.Request.Header.Values("X-Billing-Owner-User-ID")) != 1 || len(c.Request.Header.Values("X-Billing-Ownership-Version")) != 1 {
		writeError(c, http.StatusBadRequest, "BILLING_CLOSURE_CONTEXT_INVALID", "Exact global owner and ownership version are required")
		return in, false
	}
	return in, true
}
func closureReply(c *gin.Context, scope paymentstore.CloudClosureScope, field string, result any, err error) {
	if err != nil {
		switch {
		case errors.Is(err, paymentstore.ErrCloudClosureCommandRetired):
			writeError(c, http.StatusConflict, "BILLING_CLOSURE_COMMAND_RETIRED", "The original command is permanently retired")
		case errors.Is(err, paymentstore.ErrNotFound):
			writeError(c, http.StatusNotFound, "BILLING_CLOSURE_NOT_FOUND", "Closure scope not found")
		case errors.Is(err, paymentstore.ErrCloudClosureNotReady), errors.Is(err, paymentstore.ErrSettlementEvidenceStale):
			writeError(c, http.StatusConflict, "BILLING_CLOSURE_NOT_READY", "Current zero-balance settlement and provider revocations are required")
		case errors.Is(err, paymentstore.ErrConflict), errors.Is(err, paymentstore.ErrIdempotencyConflict), errors.Is(err, paymentstore.ErrOwnershipVersionConflict), errors.Is(err, paymentstore.ErrOwnershipEvidenceMissing), errors.Is(err, paymentstore.ErrHandoffFenced):
			writeError(c, http.StatusConflict, "BILLING_CLOSURE_CONFLICT", "Closure scope, phase or durable decision conflicts")
		default:
			writeError(c, http.StatusServiceUnavailable, "BILLING_CLOSURE_UNAVAILABLE", "Closure evidence is unavailable; retain the lifecycle fence")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"cloud_id": scope.OrganizationID, "owner_user_id": scope.OwnerUserID, "ownership_version": scope.OwnershipVersion, "operation_id": scope.OperationID, field: result})
}
func validClosureDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func prepareCloudClosureHandler(store cloudClosurePersistence) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := closureScope(c)
		if !ok {
			return
		}
		var req struct {
			Cutoff          time.Time `json:"cutoff"`
			AMRequestSHA256 string    `json:"am_request_sha256"`
		}
		if !bindHandoff(c, &req) {
			return
		}
		if req.Cutoff.IsZero() || req.Cutoff.After(time.Now()) || !validClosureDigest(req.AMRequestSHA256) {
			writeError(c, 400, "BILLING_CLOSURE_REQUEST_INVALID", "Persisted cutoff and AM deletion request digest are required")
			return
		}
		out, err := store.PrepareCloudClosure(c.Request.Context(), paymentstore.PrepareCloudClosureInput{Scope: scope, Cutoff: req.Cutoff, AMRequestSHA256: req.AMRequestSHA256})
		closureReply(c, scope, "operation", out, err)
	}
}
func cloudClosureStatusHandler(store cloudClosurePersistence) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := closureScope(c)
		if !ok {
			return
		}
		out, err := store.GetCloudClosureStatus(c.Request.Context(), scope)
		closureReply(c, scope, "status", out, err)
	}
}
func closeCloudHandler(store cloudClosurePersistence) gin.HandlerFunc {
	return cloudCloseCommandHandler(store, false)
}
func retireCloudCloseHandler(store cloudClosurePersistence) gin.HandlerFunc {
	return cloudCloseCommandHandler(store, true)
}
func cloudCloseCommandHandler(store cloudClosurePersistence, retire bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := closureScope(c)
		if !ok {
			return
		}
		var req struct {
			SettlementID      string `json:"settlement_id"`
			AMReadinessSHA256 string `json:"am_readiness_sha256"`
		}
		if !bindHandoff(c, &req) {
			return
		}
		if !handoffUUID(req.SettlementID) || !validClosureDigest(req.AMReadinessSHA256) {
			writeError(c, 400, "BILLING_CLOSURE_REQUEST_INVALID", "Exact settlement and AM resource readiness decision are required")
			return
		}
		in := paymentstore.CloseCloudInput{Scope: scope, SettlementID: req.SettlementID, AMReadinessSHA256: req.AMReadinessSHA256}
		if retire {
			out, err := store.RetireCloudClose(c.Request.Context(), in)
			closureReply(c, scope, "resolution", out, err)
			return
		}
		out, err := store.CloseCloud(c.Request.Context(), in)
		closureReply(c, scope, "acknowledgment", out, err)
	}
}
func cancelCloudClosureHandler(store cloudClosurePersistence) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := closureScope(c)
		if !ok {
			return
		}
		var req struct {
			CancellationID       string `json:"cancellation_id"`
			AMCancellationSHA256 string `json:"am_cancellation_sha256"`
		}
		if !bindHandoff(c, &req) {
			return
		}
		if !handoffUUID(req.CancellationID) || !validClosureDigest(req.AMCancellationSHA256) {
			writeError(c, 400, "BILLING_CLOSURE_REQUEST_INVALID", "Durable cancellation ID and decision digest are required")
			return
		}
		out, err := store.CancelCloudClosure(c.Request.Context(), scope, req.CancellationID, req.AMCancellationSHA256)
		closureReply(c, scope, "operation", out, err)
	}
}
