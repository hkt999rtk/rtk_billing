package billingdocument

import (
	"bytes"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func TestRenderTopUpStatementRequiresConfirmedPayment(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	intent := payment.PaymentIntent{
		ID: "11111111-1111-4111-8111-111111111111", AmountMinor: 500, Currency: payment.CurrencyTWD,
		Reason: payment.PaymentIntentReasonManualTopUp, Provider: "paypal", State: payment.PaymentIntentStateSucceeded,
		MerchantOrderReference: "rtk_0123456789abcdef0123456789", CreatedAt: now.Add(-time.Minute), CompletedAt: &now,
	}
	one, err := RenderTopUpStatement(intent, billing.BillingProfile{LegalName: "瑞昱半導體股份有限公司", ContactEmail: "billing@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := RenderTopUpStatement(intent, billing.BillingProfile{LegalName: "瑞昱半導體股份有限公司", ContactEmail: "billing@example.test"})
	if err != nil || !bytes.Equal(one, two) || !bytes.HasPrefix(one, []byte("%PDF-1.7")) || !bytes.HasSuffix(one, []byte("%%EOF\n")) {
		t.Fatal("top-up statement must be a deterministic PDF")
	}
	if bytes.Count(one, []byte("/FontFile2")) != 2 || len(one) > 200_000 {
		t.Fatal("top-up statement must embed both font subsets without shipping full fonts in the PDF")
	}
	intent.State = payment.PaymentIntentStateRequiresAction
	if _, err := RenderTopUpStatement(intent, billing.BillingProfile{}); err == nil {
		t.Fatal("pending payment must not produce a paid statement")
	}
}
