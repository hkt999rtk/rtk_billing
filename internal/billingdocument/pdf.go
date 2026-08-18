package billingdocument

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

const RendererVersion = "rtk-billing-pdf-v1"

func RenderInvoice(invoice billing.Invoice, generatedAt time.Time) ([]byte, billing.InvoiceDocument, error) {
	if invoice.State == billing.InvoiceStateDraft || invoice.InvoiceNumber == "" || invoice.IssuedAt == nil {
		return nil, billing.InvoiceDocument{}, billing.ErrInvalidInvoice
	}
	if err := billing.ValidateInvoiceTotals(invoice); err != nil {
		return nil, billing.InvoiceDocument{}, err
	}
	lines := append([]billing.InvoiceLine(nil), invoice.Lines...)
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].ServiceCode == lines[j].ServiceCode {
			return lines[i].MetricCode < lines[j].MetricCode
		}
		return lines[i].ServiceCode < lines[j].ServiceCode
	})
	content := &strings.Builder{}
	writeText(content, 50, 790, 18, "RTK Cloud 帳務發票")
	writeText(content, 50, 760, 14, "發票編號："+invoice.InvoiceNumber)
	writeText(content, 50, 738, 11, "計費期間："+invoice.PeriodStart.Format("2006-01-02")+" ~ "+invoice.PeriodEnd.Format("2006-01-02"))
	writeText(content, 50, 718, 11, "收件人："+truncate(invoice.Recipient.LegalName, 48))
	writeText(content, 50, 688, 11, "服務 / 計量 / 用量 / 金額 ("+string(invoice.Currency)+")")
	y := 666
	for _, line := range lines {
		label := fmt.Sprintf("%s / %s / %s / %s", truncate(line.Description, 24), line.MetricCode,
			formatScaled(line.Quantity, line.QuantityScale)+" "+line.Unit, formatMoney(line.TotalMinor, invoice.Currency))
		writeText(content, 50, y, 10, label)
		y -= 19
		if y < 150 {
			writeText(content, 50, y, 9, "其餘品項請參閱入口網站 JSON/CSV 明細")
			break
		}
	}
	writeText(content, 330, 122, 11, "小計："+formatMoney(invoice.SubtotalMinor, invoice.Currency))
	writeText(content, 330, 101, 11, "稅額："+formatMoney(invoice.TaxMinor, invoice.Currency))
	writeText(content, 330, 78, 14, "總計："+formatMoney(invoice.TotalMinor, invoice.Currency))
	writeText(content, 50, 48, 8, "本文件為不可變更的帳務快照。付款加值與發票餘額結算為不同交易。")

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 4 0 R >> >> /Contents 6 0 R >>",
		"<< /Type /Font /Subtype /Type0 /BaseFont /MSung-Light /Encoding /UniCNS-UCS2-H /DescendantFonts [5 0 R] >>",
		"<< /Type /Font /Subtype /CIDFontType0 /BaseFont /MSung-Light /CIDSystemInfo << /Registry (Adobe) /Ordering (CNS1) /Supplement 7 >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", content.Len(), content.String()),
	}
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
	offsets := make([]int, len(objects)+1)
	for i, object := range objects {
		offsets[i+1] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	data := pdf.Bytes()
	digest := sha256.Sum256(data)
	generatedAt = generatedAt.UTC()
	return data, billing.InvoiceDocument{
		ContentType: "application/pdf", ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
		RendererVersion: RendererVersion, InvoiceVersion: invoice.Version, GeneratedAt: generatedAt,
	}, nil
}

func writeText(builder *strings.Builder, x, y, size int, value string) {
	fmt.Fprintf(builder, "BT /F1 %d Tf %d %d Td <%s> Tj ET\n", size, x, y, utf16Hex(value))
}

func utf16Hex(value string) string {
	units := utf16.Encode([]rune(value))
	data := make([]byte, 2+len(units)*2)
	data[0], data[1] = 0xfe, 0xff
	for i, unit := range units {
		binary.BigEndian.PutUint16(data[2+i*2:], unit)
	}
	return strings.ToUpper(hex.EncodeToString(data))
}

func truncate(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max-1]) + "…"
}

func formatScaled(value int64, scale int) string {
	if scale <= 0 {
		return fmt.Sprintf("%d", value)
	}
	text := fmt.Sprintf("%0*d", scale+1, value)
	return text[:len(text)-scale] + "." + text[len(text)-scale:]
}

func formatMoney(value int64, currency billing.Currency) string {
	return fmt.Sprintf("%s %d", currency, value)
}
