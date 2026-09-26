package billingdocument

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/signintech/gopdf"
)

//go:embed fonts/NotoSansTC-Regular.ttf
var topUpRegularFont []byte

//go:embed fonts/NotoSansTC-Bold.ttf
var topUpBoldFont []byte

type topUpColor struct{ r, g, b uint8 }

var (
	topUpNavy  = topUpColor{14, 35, 62}
	topUpInk   = topUpColor{23, 46, 66}
	topUpMuted = topUpColor{98, 115, 134}
	topUpPale  = topUpColor{191, 211, 225}
	topUpTeal  = topUpColor{37, 195, 170}
)

type topUpCanvas struct {
	pdf *gopdf.GoPdf
	err error
}

func (c *topUpCanvas) fill(x, y, width, height int, color topUpColor) {
	c.pdf.SetFillColor(color.r, color.g, color.b)
	c.pdf.RectFromUpperLeftWithStyle(float64(x), float64(842-y-height), float64(width), float64(height), "F")
}

func (c *topUpCanvas) line(x1, y1, x2, y2 int, color topUpColor) {
	c.pdf.SetStrokeColor(color.r, color.g, color.b)
	c.pdf.Line(float64(x1), float64(842-y1), float64(x2), float64(842-y2))
}

func (c *topUpCanvas) text(x, y, size int, value string, bold bool, color topUpColor) {
	if c.err != nil {
		return
	}
	family := "NotoSansTC-Regular"
	if bold {
		family = "NotoSansTC-Bold"
	}
	c.err = c.pdf.SetFont(family, "", size)
	if c.err != nil {
		return
	}
	c.pdf.SetTextColor(color.r, color.g, color.b)
	c.pdf.SetXY(float64(x), float64(842-y))
	c.err = c.pdf.Text(value)
}

// RenderTopUpStatement produces a transaction detail for a confirmed top-up.
// It is deliberately distinct from a usage invoice and a statutory tax invoice.
func RenderTopUpStatement(intent payment.PaymentIntent, recipient billing.BillingProfile) ([]byte, error) {
	if intent.State != payment.PaymentIntentStateSucceeded || intent.Reason != payment.PaymentIntentReasonManualTopUp ||
		intent.CompletedAt == nil || intent.ID == "" || intent.AmountMinor <= 0 || intent.Currency != payment.CurrencyTWD {
		return nil, fmt.Errorf("confirmed manual top-up required")
	}
	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: gopdf.Rect{W: 595, H: 842}})
	pdf.AddPage()
	if err := pdf.AddTTFFontData("NotoSansTC-Regular", topUpRegularFont); err != nil {
		return nil, fmt.Errorf("load top-up regular font: %w", err)
	}
	if err := pdf.AddTTFFontData("NotoSansTC-Bold", topUpBoldFont); err != nil {
		return nil, fmt.Errorf("load top-up bold font: %w", err)
	}
	c := &topUpCanvas{pdf: pdf}
	c.fill(0, 632, 595, 210, topUpNavy)
	c.fill(48, 778, 4, 21, topUpTeal)
	c.text(64, 787, 9, "RTK CLOUD  /  BILLING", true, topUpPale)
	c.text(48, 735, 23, "Top-up transaction detail", true, topUpColor{255, 255, 255})
	c.text(49, 710, 10, "Payment confirmed and credited to your prepaid balance", false, topUpPale)
	c.fill(432, 769, 115, 28, topUpColor{26, 71, 88})
	c.text(448, 779, 9, "PAID  /  CONFIRMED", true, topUpColor{184, 242, 221})
	c.text(49, 674, 9, "AMOUNT PAID", true, topUpPale)
	c.text(48, 641, 29, topUpMoney(intent.AmountMinor, intent.Currency), true, topUpColor{255, 255, 255})

	c.text(49, 597, 9, "BILLED TO", true, topUpMuted)
	c.text(316, 597, 9, "PAYMENT DETAILS", true, topUpMuted)
	c.line(49, 583, 546, 583, topUpColor{216, 224, 233})
	name := strings.TrimSpace(recipient.LegalName)
	if name == "" {
		name = "Billing account holder"
	}
	c.text(49, 559, 12, fitRecipientPDF(name, 245), true, topUpInk)
	if recipient.ContactEmail != "" {
		c.text(49, 538, 10, fitPDF(recipient.ContactEmail, 39), false, topUpMuted)
	}
	c.text(49, 511, 10, "RTK Cloud prepaid balance", false, topUpMuted)
	c.paymentRow(316, 559, "Method", providerLabel(intent.Provider))
	c.paymentRow(316, 536, "Confirmed", intent.CompletedAt.UTC().Format("2006-01-02 15:04 UTC"))
	c.paymentRow(316, 513, "Created", intent.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))

	c.text(49, 467, 9, "TRANSACTION BREAKDOWN", true, topUpMuted)
	c.fill(49, 427, 497, 28, topUpColor{240, 244, 248})
	c.text(61, 437, 9, "DESCRIPTION", true, topUpMuted)
	c.text(447, 437, 9, "AMOUNT", true, topUpMuted)
	c.text(61, 402, 11, "Balance top-up", true, topUpInk)
	c.text(61, 384, 9, "One-time manual payment", false, topUpMuted)
	c.text(433, 402, 11, topUpMoney(intent.AmountMinor, intent.Currency), true, topUpInk)
	c.line(49, 365, 546, 365, topUpColor{216, 224, 233})
	c.text(357, 332, 9, "TOTAL PAID", true, topUpMuted)
	c.text(357, 307, 20, topUpMoney(intent.AmountMinor, intent.Currency), true, topUpColor{18, 93, 123})

	c.text(49, 265, 9, "RECONCILIATION REFERENCES", true, topUpMuted)
	c.line(49, 251, 546, 251, topUpColor{216, 224, 233})
	c.refRow(229, "RTK transaction", intent.ID)
	c.refRow(209, "Merchant order", intent.MerchantOrderReference)
	if intent.ProviderTransactionReference != "" {
		c.refRow(189, providerLabel(intent.Provider)+" reference", intent.ProviderTransactionReference)
	}

	c.fill(49, 81, 497, 66, topUpColor{242, 246, 248})
	c.fill(49, 81, 3, 66, topUpTeal)
	c.text(65, 125, 9, "ABOUT THIS RECORD", true, topUpInk)
	c.text(65, 108, 9, "For payment reconciliation only. This is not a Taiwan uniform invoice.", false, topUpMuted)
	c.text(65, 94, 9, "Service charges appear separately in monthly billing.", false, topUpMuted)
	c.text(49, 50, 8, "Generated by RTK Billing", false, topUpMuted)
	c.text(338, 50, 8, "Payment processed by "+providerLabel(intent.Provider), false, topUpMuted)
	if c.err != nil {
		return nil, fmt.Errorf("render top-up detail: %w", c.err)
	}
	data, err := pdf.GetBytesPdfReturnErr()
	if err != nil {
		return nil, fmt.Errorf("serialize top-up detail: %w", err)
	}
	return data, nil
}

func (c *topUpCanvas) paymentRow(x, y int, label, value string) {
	c.text(x, y, 9, label, false, topUpMuted)
	c.text(x+79, y, 9, fitPDF(value, 26), true, topUpInk)
}

func (c *topUpCanvas) refRow(y int, label, value string) {
	c.text(49, y, 9, label, false, topUpMuted)
	c.text(181, y, 9, fitPDF(value, 66), false, topUpInk)
}

func providerLabel(provider string) string {
	switch strings.ToLower(provider) {
	case "paypal":
		return "PayPal"
	case "newebpay":
		return "NewebPay"
	default:
		return fitPDF(provider, 20)
	}
}

func fitPDF(value string, max int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-3]) + "..."
}

func fitRecipientPDF(value string, maxWidth int) string {
	width := 0
	for i, character := range value {
		advance := 7
		if character > 127 {
			advance = 12
		}
		if width+advance > maxWidth {
			return value[:i] + "..."
		}
		width += advance
	}
	return value
}

func topUpMoney(amount int64, currency payment.Currency) string {
	digits := fmt.Sprintf("%d", amount)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return string(currency) + " " + digits
}
