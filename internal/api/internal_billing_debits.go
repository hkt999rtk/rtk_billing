package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

type internalBillingDebitRequest struct {
	OrganizationID string               `json:"organization_id" binding:"required"`
	AmountMinor    int64                `json:"amount_minor" binding:"required"`
	Currency       payment.Currency     `json:"currency" binding:"required"`
	Reason         payment.LedgerReason `json:"reason" binding:"required"`
	ExternalID     string               `json:"external_id" binding:"required,min=1,max=200"`
}

func (s *Server) handleInternalBillingDebit(c *gin.Context) {
	if !s.requireBillingDebitToken(c) {
		return
	}
	var request internalBillingDebitRequest
	if !bindPaymentStrict(c, &request) {
		return
	}
	request.OrganizationID = strings.TrimSpace(request.OrganizationID)
	request.ExternalID = strings.TrimSpace(request.ExternalID)
	if request.OrganizationID == "" || request.ExternalID == "" {
		writeError(c, http.StatusBadRequest, "invalid_request", "organization_id and external_id are required")
		return
	}
	idempotencyKey, ok := requiredIdempotencyKey(c)
	if !ok {
		return
	}
	externalType := ""
	switch request.Reason {
	case payment.LedgerReasonInvoiceDebit:
		externalType = "invoice"
	case payment.LedgerReasonUsageAdjustmentDebit:
		externalType = "usage_adjustment"
	default:
		writeError(c, http.StatusBadRequest, "BILLING_DEBIT_REASON_INVALID", "Billing debit reason must be invoice_debit or usage_adjustment_debit")
		return
	}
	// Do not race AM creation delivery by creating an account without its owner.
	account, err := s.payments.store.GetCommercialAccountByOrganization(c.Request.Context(), request.OrganizationID, request.Currency)
	if errors.Is(err, paymentstore.ErrNotFound) {
		writeError(c, http.StatusServiceUnavailable, "BILLING_ACCOUNT_NOT_READY", "Billing account provisioning is incomplete; retry later")
		return
	}
	if err != nil {
		writePaymentError(c, err)
		return
	}
	result, err := s.payments.store.PostLedgerEntry(c.Request.Context(), paymentstore.PostLedgerEntryInput{
		AccountID: account.ID, Direction: payment.LedgerDirectionDebit,
		AmountMinor: request.AmountMinor, Currency: request.Currency, Reason: request.Reason,
		IdempotencyScope: "billing_debit/" + s.payments.billingDebitSource,
		IdempotencyKey:   idempotencyKey, ExternalType: externalType, ExternalID: request.ExternalID,
		ActorType: "service", ActorID: s.payments.billingDebitSource,
		RequestID: paymentRequestID(c, idempotencyKey), Now: s.payments.now(),
	})
	if errors.Is(err, paymentstore.ErrIdempotencyConflict) {
		writeError(c, http.StatusConflict, "BILLING_DEBIT_CONFLICT", "Idempotency key was already used for a different billing debit")
		return
	}
	if err != nil {
		writePaymentError(c, err)
		return
	}
	response := gin.H{
		"account_id":          result.Account.ID,
		"ledger_entry_id":     result.Entry.ID,
		"balance_after_minor": result.Entry.BalanceAfterMinor,
		"currency":            result.Entry.Currency,
		"duplicate":           result.Duplicate,
	}
	if result.Intent != nil {
		response["payment_intent_id"] = result.Intent.ID
	}
	status := http.StatusCreated
	if result.Duplicate {
		status = http.StatusOK
	}
	c.JSON(status, response)
}
