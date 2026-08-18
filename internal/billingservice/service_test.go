package billingservice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

type fakeStore struct {
	invoice      billing.Invoice
	created      bool
	document     *billingstore.InvoiceDocumentRecord
	settleCalls  int
	documentPuts int
}

func (s *fakeStore) PrepareInvoice(context.Context, billingstore.PrepareInvoiceInput) (billing.Invoice, bool, error) {
	return s.invoice, s.created, nil
}
func (s *fakeStore) RecordInvoiceSettlement(_ context.Context, _, _, ledgerID string, now time.Time) (billing.Invoice, error) {
	s.settleCalls++
	s.invoice.State = billing.InvoiceStateSettled
	s.invoice.AmountSettledMinor = s.invoice.TotalMinor
	s.invoice.AmountDueMinor = 0
	s.invoice.SettlementLedgerID = ledgerID
	s.invoice.SettledAt = &now
	return s.invoice, nil
}
func (s *fakeStore) PutInvoiceDocument(_ context.Context, _, _ string, metadata billing.InvoiceDocument, data []byte) error {
	s.documentPuts++
	s.document = &billingstore.InvoiceDocumentRecord{Metadata: metadata, Bytes: data}
	return nil
}
func (s *fakeStore) GetInvoiceDocument(context.Context, string, string) (billingstore.InvoiceDocumentRecord, error) {
	if s.document == nil {
		return billingstore.InvoiceDocumentRecord{}, billingstore.ErrNotFound
	}
	return *s.document, nil
}

type fakePaymentStore struct {
	postCalls int
	duplicate bool
	postErr   error
}

func (s *fakePaymentStore) EnsureCommercialAccount(context.Context, string, payment.Currency) (payment.CommercialAccount, bool, error) {
	return payment.CommercialAccount{ID: "account-1", Currency: payment.CurrencyTWD}, false, nil
}
func (s *fakePaymentStore) PostLedgerEntry(_ context.Context, in paymentstore.PostLedgerEntryInput) (paymentstore.PostLedgerEntryResult, error) {
	s.postCalls++
	if s.postErr != nil {
		return paymentstore.PostLedgerEntryResult{}, s.postErr
	}
	if in.IdempotencyScope != "billing_invoice" || in.IdempotencyKey != "invoice-1" || in.ExternalID != "invoice-1" {
		return paymentstore.PostLedgerEntryResult{}, errors.New("wrong idempotency anchor")
	}
	return paymentstore.PostLedgerEntryResult{Entry: payment.LedgerEntry{ID: "ledger-1"}, Duplicate: s.duplicate}, nil
}

func testInvoice(now time.Time) billing.Invoice {
	return billing.Invoice{
		ID: "invoice-1", InvoiceNumber: "INV-2026-000001", OrganizationID: "org-1",
		Currency: billing.CurrencyTWD, State: billing.InvoiceStateIssued, IssuedAt: &now,
		PeriodStart: now.AddDate(0, -1, 0), PeriodEnd: now,
		SubtotalMinor: 100, TaxMinor: 5, TotalMinor: 105, AmountDueMinor: 105,
		Recipient: billing.BillingProfile{LegalName: "ACME"},
		Lines:     []billing.InvoiceLine{{SubtotalMinor: 100, TaxMinor: 5, TotalMinor: 105}},
	}
}

func TestClosePeriodPostsOneDebitSettlesAndRendersDocument(t *testing.T) {
	now := time.Date(2026, 8, 17, 1, 0, 0, 0, time.UTC)
	store := &fakeStore{invoice: testInvoice(now), created: true}
	payments := &fakePaymentStore{}
	service, err := New(Options{Store: store, PaymentStore: payments, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ClosePeriod(context.Background(), ClosePeriodInput{OrganizationID: "org-1", PeriodStart: now.AddDate(0, -1, 0), PeriodEnd: now})
	if err != nil {
		t.Fatal(err)
	}
	if payments.postCalls != 1 || store.settleCalls != 1 || store.documentPuts != 1 || result.Invoice.State != billing.InvoiceStateSettled {
		t.Fatalf("result=%+v payments=%d settle=%d docs=%d", result, payments.postCalls, store.settleCalls, store.documentPuts)
	}
}

func TestClosePeriodRetryReusesSettlementAndDocument(t *testing.T) {
	now := time.Date(2026, 8, 17, 1, 0, 0, 0, time.UTC)
	invoice := testInvoice(now)
	invoice.State = billing.InvoiceStateSettled
	invoice.AmountSettledMinor = invoice.TotalMinor
	invoice.AmountDueMinor = 0
	invoice.SettlementLedgerID = "ledger-1"
	data := []byte("existing")
	store := &fakeStore{invoice: invoice, document: &billingstore.InvoiceDocumentRecord{Bytes: data}, created: false}
	payments := &fakePaymentStore{}
	service, _ := New(Options{Store: store, PaymentStore: payments, Now: func() time.Time { return now }})
	result, err := service.ClosePeriod(context.Background(), ClosePeriodInput{OrganizationID: "org-1", PeriodStart: now.AddDate(0, -1, 0), PeriodEnd: now})
	if err != nil {
		t.Fatal(err)
	}
	if payments.postCalls != 0 || store.settleCalls != 0 || store.documentPuts != 0 || !result.Duplicate {
		t.Fatalf("result=%+v payments=%d settle=%d docs=%d", result, payments.postCalls, store.settleCalls, store.documentPuts)
	}
}
