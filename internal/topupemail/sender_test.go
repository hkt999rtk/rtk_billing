package topupemail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

func TestSenderUsesRegistrationMailContractForConfirmedTopUp(t *testing.T) {
	called := 0
	mailServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Method != http.MethodPost || r.URL.Path != "/send" || r.Header.Get("Authorization") != "Bearer mail-token" || r.Header.Get("Idempotency-Key") != "rtk-topup-11111111-1111-4111-8111-111111111111" {
			t.Fatalf("unexpected SendMail request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			From    string   `json:"from"`
			To      []string `json:"to"`
			Subject string   `json:"subject"`
			Text    string   `json:"text"`
			HTML    string   `json:"html"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.From != "billing@realtekconnect.com" || len(body.To) != 1 || body.To[0] != "billing@example.test" || !strings.Contains(body.Text, "TWD 500") || !strings.Contains(body.HTML, "Transaction reference") {
			t.Fatalf("invalid transaction message: %+v err=%v", body, err)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"sent"}`))
	}))
	defer mailServer.Close()
	sender, err := New(mailServer.URL, "mail-token", "https://admin.example.test", "billing@realtekconnect.com")
	if err != nil {
		t.Fatal(err)
	}
	message := paymentstore.TopUpEmail{IntentID: "11111111-1111-4111-8111-111111111111", OrganizationID: "22222222-2222-4222-8222-222222222222", RecipientEmail: "billing@example.test", AmountMinor: 500, Currency: "TWD", Provider: "paypal", PaymentCompletedAt: time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)}
	if err := sender.Send(context.Background(), message); err != nil || called != 1 {
		t.Fatalf("send err=%v called=%d", err, called)
	}
	message.RecipientEmail = "invalid\n@example.test"
	if err := sender.Send(context.Background(), message); err == nil || called != 1 {
		t.Fatal("invalid recipient must not be sent")
	}
	if _, err := New(mailServer.URL, "mail-token", "https://admin.example.test", "bad\n@example.test"); err == nil {
		t.Fatal("invalid sender must be rejected")
	}
}
