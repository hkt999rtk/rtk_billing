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
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hkt999rtk/rtk_billing/internal/rawretention"
)

type rawRetentionPersistence interface {
	PutPolicy(context.Context, rawretention.Policy) (rawretention.PolicyState, error)
	Policy(context.Context, string, int64) (rawretention.PolicyState, error)
	ApproveRecovery(context.Context, string, int64, string, string) (rawretention.PolicyState, error)
	SetActive(context.Context, string, int64, string, bool) (rawretention.PolicyState, error)
	PutClearance(context.Context, rawretention.Clearance) (rawretention.Clearance, error)
	RevokeClearance(context.Context, string) error
	PutHold(context.Context, rawretention.Hold) (rawretention.HoldState, error)
	Hold(context.Context, string) (rawretention.HoldState, error)
	ReleaseHold(context.Context, string) (rawretention.HoldState, error)
	RequestOperation(context.Context, rawretention.Plan) (rawretention.Operation, error)
	Operation(context.Context, string) (rawretention.Operation, error)
	Operations(context.Context, rawretention.Scope) ([]rawretention.Operation, error)
	RequestAbort(context.Context, string) (rawretention.Operation, error)
	RequestAbortPlan(context.Context, rawretention.Plan) (rawretention.Operation, error)
	Resolve(context.Context, string) (rawretention.Operation, error)
}

type RawRetentionAPIOptions struct {
	FinancialToken     string
	RecoveryToken      string
	ControllerToken    string
	AuthorityReadToken string
	Store              rawRetentionPersistence
}

// No configuration means no routes. The raw writer receives only ReadToken;
// it cannot approve policies, forge clearances, create decisions or resolve its
// own fence. Resolution queries Logger through a separately configured client.
// These routes are mounted only on the dedicated private listener, never Router.
func (s *Server) ConfigureRawRetention(in RawRetentionAPIOptions) error {
	tokens := []string{in.FinancialToken, in.RecoveryToken, in.ControllerToken, in.AuthorityReadToken}
	if s.rawRetentionConfigured || in.Store == nil {
		return fmt.Errorf("raw retention requires a store and four distinct credentials")
	}
	prior := []string{s.serviceToken, s.internalToken, s.cloudCreationToken}
	if s.handoff != nil {
		prior = append(prior, s.handoff.token)
	}
	if s.payments != nil {
		prior = append(prior, s.payments.billingDebitToken, string(s.payments.simulatorCallbackSecret))
	}
	for i, token := range tokens {
		if !otaSealToken(token) {
			return fmt.Errorf("raw retention credential must be a 32+ character distinct secret")
		}
		for _, other := range append(prior, tokens[:i]...) {
			if other != "" && other == token {
				return fmt.Errorf("raw retention credentials must not reuse another authority")
			}
		}
	}
	s.rawRetentionConfigured = true
	router := gin.New()
	router.Use(gin.Recovery())
	s.rawRetentionRouter = router
	group := router.Group("/v1/internal/billing/raw-retention", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	finance := group.Group("", rawRetentionAuth(in.FinancialToken))
	finance.POST("/policies", func(c *gin.Context) {
		var p rawretention.Policy
		if !rawRetentionJSON(c, &p) {
			return
		}
		v, err := in.Store.PutPolicy(c.Request.Context(), p)
		rawRetentionResult(c, v, err)
	})
	finance.POST("/policies/:policyId/:version/activate", rawPolicyActivation(in.Store, true))
	finance.POST("/policies/:policyId/:version/deactivate", rawPolicyActivation(in.Store, false))
	finance.POST("/clearances", func(c *gin.Context) {
		var p rawretention.Clearance
		if !rawRetentionJSON(c, &p) {
			return
		}
		v, err := in.Store.PutClearance(c.Request.Context(), p)
		rawRetentionResult(c, v, err)
	})
	finance.POST("/clearances/:clearanceId/revoke", func(c *gin.Context) {
		if !rawRetentionEmpty(c) {
			return
		}
		err := in.Store.RevokeClearance(c.Request.Context(), c.Param("clearanceId"))
		rawRetentionResult(c, gin.H{"revoked": true}, err)
	})
	finance.POST("/holds", func(c *gin.Context) {
		var p rawretention.Hold
		if !rawRetentionJSON(c, &p) {
			return
		}
		v, err := in.Store.PutHold(c.Request.Context(), p)
		rawRetentionResult(c, v, err)
	})
	finance.GET("/holds/:holdId", func(c *gin.Context) {
		v, err := in.Store.Hold(c.Request.Context(), c.Param("holdId"))
		rawRetentionResult(c, v, err)
	})
	finance.POST("/holds/:holdId/release", func(c *gin.Context) {
		if !rawRetentionEmpty(c) {
			return
		}
		v, err := in.Store.ReleaseHold(c.Request.Context(), c.Param("holdId"))
		rawRetentionResult(c, v, err)
	})
	recovery := group.Group("", rawRetentionAuth(in.RecoveryToken))
	recovery.POST("/policies/:policyId/:version/approve", func(c *gin.Context) {
		var body struct {
			PolicySHA256        string `json:"policy_sha256"`
			RecoveryApprovalRef string `json:"recovery_approval_ref"`
		}
		if !rawRetentionJSON(c, &body) {
			return
		}
		version, ok := rawPolicyVersion(c)
		if !ok {
			return
		}
		v, err := in.Store.ApproveRecovery(c.Request.Context(), c.Param("policyId"), version, body.PolicySHA256, body.RecoveryApprovalRef)
		rawRetentionResult(c, v, err)
	})
	group.GET("/policies/:policyId/:version", rawRetentionAuth(in.FinancialToken, in.RecoveryToken, in.ControllerToken), func(c *gin.Context) {
		version, ok := rawPolicyVersion(c)
		if !ok {
			return
		}
		v, err := in.Store.Policy(c.Request.Context(), c.Param("policyId"), version)
		rawRetentionResult(c, v, err)
	})
	controller := group.Group("", rawRetentionAuth(in.ControllerToken))
	controller.POST("/operations", func(c *gin.Context) {
		var p rawretention.Plan
		if !rawRetentionJSON(c, &p) {
			return
		}
		v, err := in.Store.RequestOperation(c.Request.Context(), p)
		rawRetentionResult(c, v, err)
	})
	controller.GET("/operations", func(c *gin.Context) {
		v, err := in.Store.Operations(c.Request.Context(), rawretention.Scope{Environment: c.Query("environment"), StoreID: c.Query("store_id")})
		rawRetentionResult(c, gin.H{"operations": v}, err)
	})
	controller.POST("/operations/:operationId/abort", func(c *gin.Context) {
		var p rawretention.Plan
		if c.Request.ContentLength != 0 {
			if !rawRetentionJSON(c, &p) {
				return
			}
		}
		var v rawretention.Operation
		var err error
		if p.OperationID == "" {
			if !reflect.DeepEqual(p, rawretention.Plan{}) {
				rawRetentionResult(c, nil, rawretention.ErrInvalid)
				return
			}
			v, err = in.Store.RequestAbort(c.Request.Context(), c.Param("operationId"))
		} else {
			if p.OperationID != c.Param("operationId") {
				rawRetentionResult(c, nil, rawretention.ErrInvalid)
				return
			}
			v, err = in.Store.RequestAbortPlan(c.Request.Context(), p)
		}
		rawRetentionResult(c, v, err)
	})
	controller.POST("/operations/:operationId/resolve", func(c *gin.Context) {
		if !rawRetentionEmpty(c) {
			return
		}
		v, err := in.Store.Resolve(c.Request.Context(), c.Param("operationId"))
		rawRetentionResult(c, v, err)
	})
	group.GET("/operations/:operationId", rawRetentionAuth(in.ControllerToken, in.AuthorityReadToken), func(c *gin.Context) {
		v, err := in.Store.Operation(c.Request.Context(), c.Param("operationId"))
		rawRetentionResult(c, v, err)
	})
	return nil
}

// RawRetentionRouter must be served by the internal-only listener. Public
// Router intentionally does not mount or forward any retention endpoint.
func (s *Server) RawRetentionRouter() http.Handler {
	if s.rawRetentionRouter == nil {
		return http.NotFoundHandler()
	}
	return s.rawRetentionRouter
}

func rawRetentionAuth(tokens ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := strings.TrimSpace(c.GetHeader("Authorization"))
		matched := 0
		for _, token := range tokens {
			matched |= subtle.ConstantTimeCompare([]byte(provided), []byte("Bearer "+token))
		}
		if matched != 1 {
			writeError(c, http.StatusUnauthorized, "BILLING_RAW_RETENTION_UNAUTHORIZED", "Dedicated raw retention authority is required")
			c.Abort()
			return
		}
		c.Next()
	}
}
func rawRetentionJSON(c *gin.Context, out any) bool {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(c, 400, "BILLING_RAW_RETENTION_INVALID", "A bounded JSON request is required")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(c, 400, "BILLING_RAW_RETENTION_INVALID", "Invalid raw retention request")
		return false
	}
	return true
}
func rawRetentionEmpty(c *gin.Context) bool {
	if c.Request.ContentLength == 0 {
		return true
	}
	var empty struct{}
	return rawRetentionJSON(c, &empty)
}
func rawRetentionResult(c *gin.Context, out any, err error) {
	if err == nil {
		c.JSON(http.StatusOK, out)
		return
	}
	code, status := "BILLING_RAW_RETENTION_UNAVAILABLE", http.StatusServiceUnavailable
	switch {
	case errors.Is(err, rawretention.ErrInvalid):
		code, status = "BILLING_RAW_RETENTION_INVALID", 400
	case errors.Is(err, rawretention.ErrNotFound):
		code, status = "BILLING_RAW_RETENTION_NOT_FOUND", 404
	case errors.Is(err, rawretention.ErrBlocked):
		code, status = "BILLING_RAW_RETENTION_BLOCKED", 409
	case errors.Is(err, rawretention.ErrConflict):
		code, status = "BILLING_RAW_RETENTION_CONFLICT", 409
	}
	writeError(c, status, code, "Raw retention request did not pass its authority and safety gates")
}
func rawPolicyVersion(c *gin.Context) (int64, bool) {
	raw := c.Param("version")
	version, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || version < 1 || strconv.FormatInt(version, 10) != raw {
		rawRetentionResult(c, nil, rawretention.ErrInvalid)
		return 0, false
	}
	return version, true
}
func rawPolicyActivation(store rawRetentionPersistence, active bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			PolicySHA256 string `json:"policy_sha256"`
		}
		if !rawRetentionJSON(c, &body) {
			return
		}
		version, ok := rawPolicyVersion(c)
		if !ok {
			return
		}
		v, err := store.SetActive(c.Request.Context(), c.Param("policyId"), version, body.PolicySHA256, active)
		rawRetentionResult(c, v, err)
	}
}
