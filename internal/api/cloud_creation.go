package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

type cloudCreationPersistence interface {
	BootstrapBrandCloud(context.Context, paymentstore.CloudCreation) (paymentstore.CloudCreationReceipt, error)
}
type CloudCreationAPIOptions struct {
	Token string
	Store cloudCreationPersistence
}

// This is new-cloud provisioning only, under a credential separate from the
// handoff coordinator. Existing account/history adoption is always rejected.
func (s *Server) ConfigureCloudCreation(in CloudCreationAPIOptions) error {
	if s.cloudCreationToken != "" || in.Store == nil || len(in.Token) < 32 || strings.TrimSpace(in.Token) != in.Token || strings.ContainsAny(in.Token, " \t\r\n") || in.Token == s.serviceToken || in.Token == s.internalToken || (s.handoff != nil && in.Token == s.handoff.token) || (s.payments != nil && (in.Token == s.payments.billingDebitToken || in.Token == string(s.payments.simulatorCallbackSecret))) {
		return fmt.Errorf("cloud creation requires a distinct dedicated credential and store")
	}
	s.cloudCreationToken = in.Token
	s.router.POST("/v1/internal/billing/cloud-creations", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, requireBearerToken(in.Token), func(c *gin.Context) {
		var event paymentstore.CloudCreation
		if !bindHandoff(c, &event) {
			return
		}
		if !event.Valid() {
			writeError(c, 400, "BILLING_CLOUD_CREATION_INVALID", "Canonical new-cloud event binding is required")
			return
		}
		out, err := in.Store.BootstrapBrandCloud(c.Request.Context(), event)
		if err != nil {
			status, code := http.StatusServiceUnavailable, "BILLING_CLOUD_CREATION_UNAVAILABLE"
			if errors.Is(err, paymentstore.ErrConflict) || errors.Is(err, paymentstore.ErrIdempotencyConflict) || errors.Is(err, paymentstore.ErrOwnershipEvidenceMissing) {
				status, code = http.StatusConflict, "BILLING_CLOUD_CREATION_CONFLICT"
			}
			writeError(c, status, code, "Cloud creation evidence could not be applied")
			return
		}
		c.JSON(http.StatusOK, out)
	})
	return nil
}
