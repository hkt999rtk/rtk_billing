package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
)

type otaPeriodSealPersistence interface {
	PutOTAPeriodSeal(context.Context, billingstore.OTAPeriodSeal) (billingstore.OTAPeriodSeal, bool, error)
}

type OTAPeriodSealAPIOptions struct {
	PlatformToken string
	ProducerToken string
	Store         otaPeriodSealPersistence
}

// ConfigureOTAPeriodSeals installs a dedicated source-attestation endpoint.
// The general Billing internal credential cannot impersonate either issuer.
func (s *Server) ConfigureOTAPeriodSeals(in OTAPeriodSealAPIOptions) error {
	if s.otaSealConfigured || in.Store == nil || !otaSealToken(in.PlatformToken) || !otaSealToken(in.ProducerToken) ||
		in.PlatformToken == in.ProducerToken || in.PlatformToken == s.serviceToken || in.ProducerToken == s.serviceToken ||
		in.PlatformToken == s.internalToken || in.ProducerToken == s.internalToken ||
		in.PlatformToken == s.cloudCreationToken || in.ProducerToken == s.cloudCreationToken ||
		s.handoff != nil && (in.PlatformToken == s.handoff.token || in.ProducerToken == s.handoff.token) ||
		s.payments != nil && (in.PlatformToken == s.payments.billingDebitToken || in.ProducerToken == s.payments.billingDebitToken) {
		return fmt.Errorf("OTA period seals require distinct Platform and producer credentials and a store")
	}
	s.otaSealConfigured = true
	s.router.POST("/v1/internal/billing/ota-period-seals", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		provided := strings.TrimSpace(c.GetHeader("Authorization"))
		issuer := ""
		switch {
		case subtle.ConstantTimeCompare([]byte(provided), []byte("Bearer "+in.PlatformToken)) == 1:
			issuer = billingstore.OTAIssuerPlatformGrants
		case subtle.ConstantTimeCompare([]byte(provided), []byte("Bearer "+in.ProducerToken)) == 1:
			issuer = billingstore.OTAIssuerProducer
		default:
			writeError(c, http.StatusUnauthorized, "BILLING_OTA_SEAL_UNAUTHORIZED", "OTA period seal credential is required")
			return
		}
		media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || media != "application/json" {
			writeError(c, http.StatusBadRequest, "BILLING_OTA_SEAL_INVALID", "A JSON period seal is required")
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1024*1024))
		decoder.DisallowUnknownFields()
		var seal billingstore.OTAPeriodSeal
		if err := decoder.Decode(&seal); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) || seal.IssuerKind != issuer {
			writeError(c, http.StatusBadRequest, "BILLING_OTA_SEAL_INVALID", "OTA period seal body or issuer is invalid")
			return
		}
		stored, created, err := in.Store.PutOTAPeriodSeal(c.Request.Context(), seal)
		if err != nil {
			if errors.Is(err, billingstore.ErrConflict) {
				writeError(c, http.StatusConflict, "BILLING_OTA_SEAL_CONFLICT", "OTA period seal conflicts with immutable evidence")
				return
			}
			writeError(c, http.StatusServiceUnavailable, "BILLING_OTA_SEAL_UNAVAILABLE", "OTA period seal could not be stored")
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		c.JSON(status, gin.H{"ota_period_seal": stored, "duplicate": !created})
	})
	return nil
}

func otaSealToken(token string) bool {
	return len(token) >= 32 && strings.TrimSpace(token) == token && !strings.ContainsAny(token, " \t\r\n")
}
