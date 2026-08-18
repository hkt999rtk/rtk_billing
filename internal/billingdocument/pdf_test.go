package billingdocument

import (
	"bytes"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

func TestRenderInvoiceIsDeterministicAndHasMatchingDigest(t *testing.T) {
	now := time.Date(2026, 8, 17, 1, 2, 3, 0, time.UTC)
	invoice := billing.Invoice{
		InvoiceNumber: "INV-2026-000001", State: billing.InvoiceStateIssued, Currency: billing.CurrencyTWD,
		PeriodStart: now.AddDate(0, -1, 0), PeriodEnd: now, IssuedAt: &now,
		SubtotalMinor: 802, TaxMinor: 40, TotalMinor: 842, AmountDueMinor: 842,
		Recipient: billing.BillingProfile{LegalName: "測試企業有限公司"},
		Lines: []billing.InvoiceLine{
			{ServiceCode: "video", MetricCode: "stream_gb", Description: "Video 串流", Quantity: 1862, QuantityScale: 1, Unit: "GB", SubtotalMinor: 521, TaxMinor: 26, TotalMinor: 547},
			{ServiceCode: "storage", MetricCode: "storage_gb", Description: "Storage", Quantity: 5124, QuantityScale: 1, Unit: "GB", SubtotalMinor: 281, TaxMinor: 14, TotalMinor: 295},
		},
	}
	one, metadata, err := RenderInvoice(invoice, now)
	if err != nil {
		t.Fatal(err)
	}
	two, metadata2, err := RenderInvoice(invoice, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) || metadata.SHA256 != metadata2.SHA256 {
		t.Fatal("invoice PDF must be deterministic")
	}
	if !bytes.HasPrefix(one, []byte("%PDF-1.7")) || !bytes.HasSuffix(one, []byte("%%EOF\n")) {
		t.Fatal("invalid PDF envelope")
	}
	if metadata.ByteLength != int64(len(one)) || metadata.ContentType != "application/pdf" || len(metadata.SHA256) != 64 {
		t.Fatalf("metadata=%+v", metadata)
	}
}

func TestRenderInvoiceRejectsDraftAndMismatchedTotals(t *testing.T) {
	now := time.Now().UTC()
	_, _, err := RenderInvoice(billing.Invoice{State: billing.InvoiceStateDraft}, now)
	if err == nil {
		t.Fatal("draft must not render")
	}
	_, _, err = RenderInvoice(billing.Invoice{
		InvoiceNumber: "INV-2026-000001", State: billing.InvoiceStateIssued, IssuedAt: &now,
		SubtotalMinor: 10, TotalMinor: 11, AmountDueMinor: 11,
		Lines: []billing.InvoiceLine{{SubtotalMinor: 10, TotalMinor: 10}},
	}, now)
	if err == nil {
		t.Fatal("mismatched total must not render")
	}
}
