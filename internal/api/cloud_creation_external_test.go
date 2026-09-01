package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

// Child-only fixture: secrets arrive on stdin; readiness and durable witnesses
// contain only an owned loopback URL, identifiers, counts and a receipt digest.
func TestBillingCloudCreationWithExternalAccountManager(t *testing.T) {
	if os.Getenv("TEST_BILLING_EXTERNAL_BOOTSTRAP") != "1" {
		t.Skip("requires independently compiled AM creation worker")
	}
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	var in struct{ DSN, Token string }
	if decoder.Decode(&in) != nil || in.DSN == "" || in.Token == "" {
		t.Fatal("missing isolated fixture configuration")
	}
	t.Setenv("TEST_DATABASE_URL", in.DSN)
	gin.SetMode(gin.ReleaseMode)
	env := newIntegrationAPI(t)
	if err := env.server.ConfigureCloudCreation(CloudCreationAPIOptions{Token: in.Token, Store: paymentstore.New(env.db)}); err != nil {
		t.Fatal(err)
	}
	var dropped atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := httptest.NewRecorder()
		env.server.Router().ServeHTTP(out, r)
		if out.Code == 200 && dropped.CompareAndSwap(false, true) {
			w.WriteHeader(503)
			return
		}
		for k, values := range out.Header() {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(out.Code)
		w.Write(out.Body.Bytes())
	}))
	defer server.Close()
	if err := encoder.Encode(map[string]string{"URL": server.URL}); err != nil {
		t.Fatal("readiness")
	}
	for {
		var command struct{ CloudID string }
		if err := decoder.Decode(&command); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatal("fixture command")
		}
		var result struct {
			AccountID, OwnerID, Evidence       string
			Accounts, Periods, Audits, Balance int64
		}
		err := env.db.QueryRow(t.Context(), `SELECT a.id::text,r.owner_user_id::text,c.evidence_sha256,a.available_balance_minor,(SELECT count(*) FROM commercial_accounts WHERE organization_id=$1),(SELECT count(*) FROM billing_responsibility_periods WHERE account_id=a.id),(SELECT count(*) FROM billing_audit_events WHERE organization_id=$1 AND event_type='billing.cloud_creation.bootstrap') FROM commercial_accounts a JOIN billing_responsibility_periods r ON r.account_id=a.id JOIN billing_cloud_creation_receipts c ON c.account_id=a.id WHERE a.organization_id=$1`, command.CloudID).Scan(&result.AccountID, &result.OwnerID, &result.Evidence, &result.Balance, &result.Accounts, &result.Periods, &result.Audits)
		if err != nil {
			t.Fatal("durable bootstrap witness", err)
		}
		if encoder.Encode(result) != nil {
			t.Fatal("send witness")
		}
	}
}
