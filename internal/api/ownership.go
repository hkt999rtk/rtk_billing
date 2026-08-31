package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
)

type ownerAuthorizer interface {
	AuthorizeOwner(context.Context, string, string, int64) (billingidentity.Scope, error)
}

func (s *Server) requireCurrentOwner() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		value := c.GetHeader("X-Billing-Ownership-Version")
		version, err := strconv.ParseInt(value, 10, 64)
		if err != nil || version < 1 || strconv.FormatInt(version, 10) != value {
			writeError(c, http.StatusBadRequest, "BILLING_OWNERSHIP_CONTEXT_REQUIRED", "An exact positive ownership version is required")
			c.Abort()
			return
		}
		scope, err := s.ownership.AuthorizeOwner(c.Request.Context(), c.Param("orgId"), paymentActorID(c), version)
		if err != nil {
			if !writeOwnershipError(c, err) {
				writeError(c, http.StatusServiceUnavailable, "BILLING_OWNERSHIP_UNAVAILABLE", "Billing ownership evidence is unavailable")
			}
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(billingidentity.WithScope(c.Request.Context(), scope))
		c.Next()
	}
}

func writeOwnershipError(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, billingidentity.ErrInvalid):
		writeError(c, http.StatusBadRequest, "BILLING_OWNERSHIP_CONTEXT_REQUIRED", "Valid global user, cloud and ownership version are required")
	case errors.Is(err, billingidentity.ErrDenied):
		writeError(c, http.StatusForbidden, "BILLING_OWNER_REQUIRED", "Current cloud owner authority is required")
	case errors.Is(err, billingidentity.ErrVersion):
		writeError(c, http.StatusConflict, "BILLING_OWNERSHIP_VERSION_CONFLICT", "Billing ownership version has changed")
	case errors.Is(err, billingidentity.ErrTransition):
		writeError(c, http.StatusConflict, "BILLING_OWNERSHIP_TRANSITION", "Billing ownership commit is in progress")
	case errors.Is(err, billingidentity.ErrUnavailable):
		writeError(c, http.StatusServiceUnavailable, "BILLING_OWNERSHIP_UNAVAILABLE", "Billing ownership evidence is unavailable")
	default:
		return false
	}
	return true
}

// Provider work may outlive the admitted tenant request. Reconciliation is not
// permission to expose its hosted action to a departed actor.
func (s *Server) revalidateOwnerResponse(c *gin.Context) bool {
	scope, ok := billingidentity.FromContext(c.Request.Context())
	if !ok {
		writeError(c, http.StatusForbidden, "BILLING_OWNER_REQUIRED", "Current cloud owner authority is required")
		return false
	}
	if _, err := s.ownership.AuthorizeOwner(c.Request.Context(), scope.OrganizationID, scope.UserID, scope.OwnershipVersion); err != nil {
		if !writeOwnershipError(c, err) {
			writeError(c, http.StatusServiceUnavailable, "BILLING_OWNERSHIP_UNAVAILABLE", "Billing ownership evidence is unavailable")
		}
		return false
	}
	return true
}
