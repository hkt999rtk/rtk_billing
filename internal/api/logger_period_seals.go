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

type loggerPeriodSealPersistence interface {
	PutLoggerPeriodSeal(context.Context, billingstore.LoggerPeriodSeal) (billingstore.LoggerPeriodSeal, bool, error)
}

type LoggerPeriodSealAPIOptions struct {
	ProducerToken string
	Store         loggerPeriodSealPersistence
}

func (s *Server) ConfigureLoggerPeriodSeals(in LoggerPeriodSealAPIOptions) error {
	if s.loggerSealToken != "" || in.Store == nil || !otaSealToken(in.ProducerToken) {
		return fmt.Errorf("Logger seals require a dedicated producer credential and store")
	}
	secrets := append([]string{s.serviceToken, s.internalToken, s.cloudCreationToken}, s.otaSealTokens...)
	if s.handoff != nil {
		secrets = append(secrets, s.handoff.token)
	}
	if s.payments != nil {
		secrets = append(secrets, s.payments.billingDebitToken)
	}
	for _, secret := range secrets {
		if in.ProducerToken == secret {
			return fmt.Errorf("Logger seal credential must be distinct from other service credentials")
		}
	}
	s.loggerSealToken = in.ProducerToken
	s.router.POST("/v1/internal/billing/logger-period-seals", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(c.GetHeader("Authorization"))), []byte("Bearer "+in.ProducerToken)) != 1 {
			writeError(c, http.StatusUnauthorized, "BILLING_LOGGER_SEAL_UNAUTHORIZED", "Logger producer seal credential is required")
			return
		}
		media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || media != "application/json" {
			writeError(c, http.StatusBadRequest, "BILLING_LOGGER_SEAL_INVALID", "A JSON period seal is required")
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1024*1024))
		decoder.DisallowUnknownFields()
		var seal billingstore.LoggerPeriodSeal
		if err := decoder.Decode(&seal); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) || seal.IssuerKind != billingstore.LoggerIssuerProducer {
			writeError(c, http.StatusBadRequest, "BILLING_LOGGER_SEAL_INVALID", "Logger period seal body or issuer is invalid")
			return
		}
		stored, created, err := in.Store.PutLoggerPeriodSeal(c.Request.Context(), seal)
		if err != nil {
			if errors.Is(err, billingstore.ErrConflict) {
				writeError(c, http.StatusConflict, "BILLING_LOGGER_SEAL_CONFLICT", "Logger seal conflicts with immutable evidence")
				return
			}
			if errors.Is(err, billingstore.ErrIncomplete) {
				writeError(c, http.StatusServiceUnavailable, "BILLING_LOGGER_SOURCE_INCOMPLETE", "Logger source facts are incomplete")
				return
			}
			writeError(c, http.StatusServiceUnavailable, "BILLING_LOGGER_SEAL_UNAVAILABLE", "Logger period seal could not be stored")
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		c.JSON(status, gin.H{"logger_period_seal": stored, "duplicate": !created})
	})
	return nil
}
