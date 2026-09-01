package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

type ownershipEligibilityReader interface {
	CheckOwnershipEligibility(context.Context, paymentstore.OwnershipEligibilityRequest) (paymentstore.OwnershipEligibility, error)
}

func ownershipEligibilityHandler(reader ownershipEligibilityReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := cloudPreflightScope(c)
		if !ok {
			return
		}
		var body struct {
			TargetUserID string `json:"target_user_id"`
			TransferID   string `json:"transfer_id"`
			Action       string `json:"action"`
		}
		if !bindHandoff(c, &body) {
			return
		}
		in := paymentstore.OwnershipEligibilityRequest{CloudID: scope.OrganizationID, SourceUserID: scope.OwnerUserID, TargetUserID: body.TargetUserID, TransferID: body.TransferID, Action: body.Action, OwnershipVersion: scope.OwnershipVersion}
		if !in.Valid() {
			writeError(c, http.StatusBadRequest, "BILLING_HANDOFF_CONTEXT_INVALID", "Distinct global users and an exact request or acceptance binding are required")
			return
		}
		out, err := reader.CheckOwnershipEligibility(c.Request.Context(), in)
		if err != nil {
			cloudPreflightError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
