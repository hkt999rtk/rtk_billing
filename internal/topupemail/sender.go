package topupemail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

// Sender uses the same SendMail HTTP contract as account registration emails.
type Sender struct {
	endpoint, token, portalBase, from string
	client                            *http.Client
}

func New(baseURL, token, portalBase, from string) (*Sender, error) {
	mailURL, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || mailURL.Host == "" || (mailURL.Scheme != "http" && mailURL.Scheme != "https") || mailURL.User != nil || mailURL.RawQuery != "" || mailURL.Fragment != "" || (mailURL.Path != "" && mailURL.Path != "/") {
		return nil, errors.New("invalid SendMail origin")
	}
	portal, err := url.Parse(strings.TrimSpace(portalBase))
	if err != nil || portal.Scheme != "https" || portal.Host == "" || portal.User != nil || portal.RawQuery != "" || portal.Fragment != "" || (portal.Path != "" && portal.Path != "/") {
		return nil, errors.New("invalid Billing portal origin")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("SendMail token is required")
	}
	if from != "" {
		address, err := mail.ParseAddress(strings.TrimSpace(from))
		if err != nil || address.Address != strings.TrimSpace(from) {
			return nil, errors.New("invalid SendMail sender")
		}
		from = address.Address
	}
	mailURL.Path = "/send"
	return &Sender{endpoint: mailURL.String(), token: token, portalBase: strings.TrimRight(portal.String(), "/"), from: from, client: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (s *Sender) Send(ctx context.Context, item paymentstore.TopUpEmail) error {
	to, err := mail.ParseAddress(strings.TrimSpace(item.RecipientEmail))
	if err != nil || to.Address != strings.TrimSpace(item.RecipientEmail) || item.AmountMinor <= 0 || item.Currency != "TWD" || item.Provider != "paypal" || item.IntentID == "" {
		return errors.New("invalid top-up email")
	}
	link := s.portalBase + "/console/clouds/" + url.PathEscape(item.OrganizationID) + "/billing/settings?intent=" + url.QueryEscape(item.IntentID)
	subject := "RTK Cloud top-up transaction detail"
	text := fmt.Sprintf("RTK Cloud top-up confirmed\n\nTransaction reference: %s\nPayment provider: PayPal\nAmount paid: TWD %d\nConfirmed: %s\n\nView and download your transaction detail: %s\n\nThis is a transaction record, not a Taiwan uniform invoice.\n", item.IntentID, item.AmountMinor, item.PaymentCompletedAt.UTC().Format(time.RFC3339), link)
	htmlBody := fmt.Sprintf(`<!doctype html><html><body style="margin:0;background:#edf2f7;font-family:Arial,sans-serif;color:#20354c"><div style="max-width:600px;margin:32px auto;background:#fff;border:1px solid #dce5ef;border-radius:12px;overflow:hidden"><div style="background:#245e9f;color:white;padding:24px 32px;font-size:22px;font-weight:700">RTK Cloud</div><div style="padding:32px"><p style="color:#52739a;font-size:12px;letter-spacing:.1em;text-transform:uppercase">Payment confirmed</p><h1 style="font-size:26px;line-height:1.25;margin:0 0 22px">Top-up transaction detail</h1><p style="font-size:15px">Your manual top-up has been confirmed.</p><table style="width:100%%;border-collapse:collapse;font-size:14px"><tr><td style="padding:12px 0;color:#62748a">Transaction reference</td><td style="padding:12px 0;text-align:right">%s</td></tr><tr><td style="padding:12px 0;color:#62748a">Payment provider</td><td style="padding:12px 0;text-align:right">PayPal</td></tr><tr><td style="padding:12px 0;color:#62748a">Confirmed</td><td style="padding:12px 0;text-align:right">%s</td></tr><tr style="border-top:1px solid #dce5ef"><td style="padding:18px 0;font-weight:700">Amount paid</td><td style="padding:18px 0;text-align:right;font-weight:700;font-size:20px">TWD %d</td></tr></table><p><a href="%s" style="display:inline-block;background:#245e9f;color:#fff;padding:13px 18px;border-radius:6px;text-decoration:none;font-weight:600">View transaction detail</a></p><p style="color:#62748a;font-size:12px;line-height:1.6">For transaction reconciliation only. This is not a Taiwan uniform invoice.</p></div></div></body></html>`, html.EscapeString(item.IntentID), html.EscapeString(item.PaymentCompletedAt.UTC().Format(time.RFC3339)), item.AmountMinor, html.EscapeString(link))
	payload := map[string]any{"to": []string{to.Address}, "subject": subject, "text": text, "html": htmlBody}
	if s.from != "" {
		payload["from"] = s.from
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Idempotency-Key", "rtk-topup-"+item.IntentID)
	response, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 || response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("SendMail did not accept transaction detail: HTTP %d", response.StatusCode)
	}
	var accepted struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &accepted); err != nil || accepted.Status != "sent" {
		return errors.New("SendMail returned an invalid accepted response")
	}
	return nil
}
