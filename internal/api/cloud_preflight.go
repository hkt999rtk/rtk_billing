package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

type cloudDeletionPreflightReader interface {
	GetCloudDeletionPreflight(context.Context, paymentstore.CloudPreflightScope) (paymentstore.CloudDeletionPreflight, error)
}

func cloudDeletionPreflightHandler(reader cloudDeletionPreflightReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		in, ok := cloudPreflightScope(c)
		if !ok {
			return
		}
		result, err := reader.GetCloudDeletionPreflight(c.Request.Context(), in)
		if err != nil {
			cloudPreflightError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func cloudPreflightScope(c *gin.Context) (paymentstore.CloudPreflightScope, bool) {
	versionText := c.GetHeader("X-Billing-Ownership-Version")
	version, err := strconv.ParseInt(versionText, 10, 64)
	in := paymentstore.CloudPreflightScope{OrganizationID: c.Param("orgId"), OwnerUserID: c.GetHeader("X-Billing-Owner-User-ID"), OwnershipVersion: version}
	if err != nil || version < 1 || strconv.FormatInt(version, 10) != versionText || !handoffUUID(in.OrganizationID) || !handoffUUID(in.OwnerUserID) || len(c.Request.Header.Values("X-Billing-Ownership-Version")) != 1 || len(c.Request.Header.Values("X-Billing-Owner-User-ID")) != 1 {
		writeError(c, http.StatusBadRequest, "BILLING_CLOUD_CONTEXT_INVALID", "Exact cloud, global owner and ownership version are required")
		return in, false
	}
	return in, true
}

func cloudPreflightError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, paymentstore.ErrNotFound):
		writeError(c, http.StatusNotFound, "BILLING_CLOUD_NOT_FOUND", "Billing cloud account not found")
	case errors.Is(err, paymentstore.ErrOwnershipVersionConflict):
		writeError(c, http.StatusConflict, "BILLING_CLOUD_OWNER_CONFLICT", "Billing owner context has changed")
	default:
		writeError(c, http.StatusServiceUnavailable, "BILLING_CLOUD_EVIDENCE_UNAVAILABLE", "Authoritative Billing evidence is unavailable")
	}
}
