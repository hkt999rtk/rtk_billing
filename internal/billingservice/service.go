package billingservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/billingdocument"
	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

type Store interface {
	PrepareInvoice(context.Context, billingstore.PrepareInvoiceInput) (billing.Invoice, bool, error)
	RecordInvoiceSettlement(context.Context, string, string, string, time.Time) (billing.Invoice, error)
	PutInvoiceDocument(context.Context, string, string, billing.InvoiceDocument, []byte) error
	GetInvoiceDocument(context.Context, string, string) (billingstore.InvoiceDocumentRecord, error)
}

type PaymentStore interface {
	EnsureCommercialAccount(context.Context, string, payment.Currency) (payment.CommercialAccount, bool, error)
	PostLedgerEntry(context.Context, paymentstore.PostLedgerEntryInput) (paymentstore.PostLedgerEntryResult, error)
}

type Options struct {
	Store        Store
	PaymentStore PaymentStore
	ActorID      string
	Now          func() time.Time
}

type Service struct {
	store        Store
	paymentStore PaymentStore
	actorID      string
	now          func() time.Time
}

type ClosePeriodInput struct {
	OrganizationID string
	PeriodStart    time.Time
	PeriodEnd      time.Time
	DueAt          time.Time
	RequestID      string
}

type ClosePeriodResult struct {
	Invoice         billing.Invoice `json:"invoice"`
	PaymentIntentID string          `json:"payment_intent_id,omitempty"`
	Duplicate       bool            `json:"duplicate"`
}

func New(options Options) (*Service, error) {
	if options.Store == nil || options.PaymentStore == nil {
		return nil, errors.New("billing and payment stores are required")
	}
	if options.ActorID == "" {
		options.ActorID = "account-manager-billing"
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: options.Store, paymentStore: options.PaymentStore, actorID: options.ActorID, now: options.Now}, nil
}

func (s *Service) ClosePeriod(ctx context.Context, in ClosePeriodInput) (ClosePeriodResult, error) {
	if in.OrganizationID == "" || !in.PeriodEnd.After(in.PeriodStart) {
		return ClosePeriodResult{}, billingstore.ErrConflict
	}
	now := s.now().UTC()
	account, _, err := s.paymentStore.EnsureCommercialAccount(ctx, in.OrganizationID, payment.CurrencyTWD)
	if err != nil {
		return ClosePeriodResult{}, err
	}
	invoice, created, err := s.store.PrepareInvoice(ctx, billingstore.PrepareInvoiceInput{
		OrganizationID: in.OrganizationID, AccountID: account.ID, Currency: billing.CurrencyTWD,
		PeriodStart: in.PeriodStart.UTC(), PeriodEnd: in.PeriodEnd.UTC(), DueAt: in.DueAt, Now: now,
	})
	if err != nil {
		return ClosePeriodResult{}, err
	}
	ledgerID := invoice.SettlementLedgerID
	paymentIntentID := ""
	duplicate := !created
	if invoice.TotalMinor > 0 && ledgerID == "" {
		result, err := s.paymentStore.PostLedgerEntry(ctx, paymentstore.PostLedgerEntryInput{
			AccountID: account.ID, Direction: payment.LedgerDirectionDebit,
			AmountMinor: invoice.TotalMinor, Currency: payment.CurrencyTWD, Reason: payment.LedgerReasonInvoiceDebit,
			IdempotencyScope: "billing_invoice", IdempotencyKey: invoice.ID,
			ExternalType: "invoice", ExternalID: invoice.ID,
			ActorType: "service", ActorID: s.actorID, RequestID: in.RequestID, Now: now,
		})
		if err != nil {
			return ClosePeriodResult{}, fmt.Errorf("post invoice debit: %w", err)
		}
		ledgerID = result.Entry.ID
		duplicate = duplicate || result.Duplicate
		if result.Intent != nil {
			paymentIntentID = result.Intent.ID
		}
	}
	if invoice.State != billing.InvoiceStateSettled || invoice.SettlementLedgerID == "" && ledgerID != "" {
		invoice, err = s.store.RecordInvoiceSettlement(ctx, in.OrganizationID, invoice.ID, ledgerID, now)
		if err != nil {
			return ClosePeriodResult{}, fmt.Errorf("record invoice settlement: %w", err)
		}
	}
	if _, err := s.store.GetInvoiceDocument(ctx, in.OrganizationID, invoice.ID); errors.Is(err, billingstore.ErrNotFound) {
		data, document, renderErr := billingdocument.RenderInvoice(invoice, now)
		if renderErr != nil {
			return ClosePeriodResult{}, fmt.Errorf("render invoice: %w", renderErr)
		}
		if err := s.store.PutInvoiceDocument(ctx, in.OrganizationID, invoice.ID, document, data); err != nil {
			return ClosePeriodResult{}, fmt.Errorf("store invoice document: %w", err)
		}
		invoice.Document = &document
	} else if err != nil {
		return ClosePeriodResult{}, err
	}
	return ClosePeriodResult{Invoice: invoice, PaymentIntentID: paymentIntentID, Duplicate: duplicate}, nil
}
