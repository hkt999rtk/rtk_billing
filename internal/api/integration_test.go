package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hkt999rtk/rtk_billing/internal/accessstore"
	"github.com/hkt999rtk/rtk_billing/internal/auditstore"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentcrypto"
	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/fake"
	simulatorprovider "github.com/hkt999rtk/rtk_billing/internal/paymentprovider/simulator"
	"github.com/hkt999rtk/rtk_billing/internal/paymentsimulator"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

const integrationServiceToken = "billing-integration-service-token-0001"

type integrationAPI struct {
	server *Server
	db     *pgxpool.Pool
}

func newIntegrationAPI(t *testing.T, providers ...payment.PaymentProvider) integrationAPI {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	testutil.LockIntegrationDatabase(t, db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		TRUNCATE
			billing_audit_events,
			billing_access_states,
			payment_simulator_operations,
			payment_simulator_setup_sessions,
			payment_reconciliation_jobs,
			payment_webhook_inbox,
			payment_method_setup_sessions,
			payment_attempts,
			payment_intents,
			auto_topup_policies,
			payment_methods,
			payment_consents,
			balance_ledger_entries,
			commercial_accounts
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatal(err)
	}
	server, err := New(Options{
		ServiceToken: integrationServiceToken,
		Audit:        AuditAdapter{Store: auditstore.New(db)},
		Access:       accessstore.New(db),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	protector, err := paymentcrypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ConfigurePayments(PaymentAPIOptions{
		Store:              paymentstore.New(db),
		Providers:          providers,
		ReferenceProtector: protector,
		BillingDebitToken:  integrationServiceToken,
		BillingDebitSource: "rtk_billing",
		Now: func() time.Time {
			return time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC)
		},
	}); err != nil {
		t.Fatal(err)
	}
	return integrationAPI{server: server, db: db}
}

func (a integrationAPI) request(t *testing.T, method, path, permission, idempotencyKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+integrationServiceToken)
	req.Header.Set("X-Billing-Permissions", permission)
	req.Header.Set("X-Billing-Actor-Type", "user")
	req.Header.Set("X-Billing-Actor-ID", "integration-admin")
	req.Header.Set("X-Request-Id", "integration-request")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	a.server.Router().ServeHTTP(response, req)
	return response
}

func TestIntegrationInternalBillingDebitAuthenticationAndIdempotency(t *testing.T) {
	env := newIntegrationAPI(t)
	organizationID := testutil.OrganizationID("billing-api-debit")
	path := "/v1/internal/billing/debits"
	body := map[string]any{
		"organization_id": organizationID,
		"amount_minor":    842,
		"currency":        "TWD",
		"reason":          "invoice_debit",
		"external_id":     "INV-2026-000128",
	}

	unauthorized := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
	unauthorized.Header.Set("Content-Type", "application/json")
	unauthorizedResponse := httptest.NewRecorder()
	env.server.Router().ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", unauthorizedResponse.Code, unauthorizedResponse.Body.String())
	}

	created := env.request(t, http.MethodPost, path, "", "invoice-debit-001", body)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"balance_after_minor":-842`) {
		t.Fatalf("created status=%d body=%s", created.Code, created.Body.String())
	}
	replayed := env.request(t, http.MethodPost, path, "", "invoice-debit-001", body)
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), `"duplicate":true`) {
		t.Fatalf("replay status=%d body=%s", replayed.Code, replayed.Body.String())
	}
	conflictBody := map[string]any{}
	for key, value := range body {
		conflictBody[key] = value
	}
	conflictBody["amount_minor"] = 843
	conflict := env.request(t, http.MethodPost, path, "", "invoice-debit-001", conflictBody)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	var entries, accounts int
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger_entries`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(context.Background(), `SELECT count(*) FROM commercial_accounts WHERE organization_id = $1`, organizationID).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if entries != 1 || accounts != 1 {
		t.Fatalf("expected one account and one immutable debit, accounts=%d entries=%d", accounts, entries)
	}
}

func TestIntegrationPaymentAPIAuthorizationLifecycleAndRedaction(t *testing.T) {
	provider := fake.New("integration-webhook-secret")
	provider.QueueSetup(fake.SetupOutcome{Result: payment.SetupResult{
		State:               payment.PaymentIntentStateSucceeded,
		HostedURL:           "https://payments.invalid/hosted/setup-token-sensitive",
		ProviderCode:        "approved",
		ProviderCustomerRef: "customer-reference-sensitive",
		ProviderMethodRef:   "method-reference-sensitive",
		CardBrand:           "TEST",
		LastFour:            "4242",
	}})
	env := newIntegrationAPI(t, provider)
	organizationID := testutil.OrganizationID("billing-api-setup")
	path := "/v1/orgs/" + organizationID + "/payment-methods/setup"
	body := map[string]any{
		"provider": "fake",
		"consent": map[string]any{
			"accepted": true, "text_version": "payment-method-v1",
			"text_sha256": strings.Repeat("a", 64), "locale": "zh-TW",
		},
	}

	wrongPermission := env.request(t, http.MethodPost, path, "payment_method.read", "setup-001", body)
	if wrongPermission.Code != http.StatusForbidden {
		t.Fatalf("wrong permission status=%d body=%s", wrongPermission.Code, wrongPermission.Body.String())
	}
	created := env.request(t, http.MethodPost, path, "payment_method.manage", "setup-001", body)
	if created.Code != http.StatusAccepted || strings.Contains(created.Body.String(), "reference-sensitive") {
		t.Fatalf("created status=%d body=%s", created.Code, created.Body.String())
	}
	replayed := env.request(t, http.MethodPost, path, "payment_method.manage", "setup-001", body)
	if replayed.Code != http.StatusAccepted || !strings.Contains(replayed.Body.String(), `"duplicate":true`) {
		t.Fatalf("replay status=%d body=%s", replayed.Code, replayed.Body.String())
	}

	unsafe := map[string]any{}
	for key, value := range body {
		unsafe[key] = value
	}
	unsafe["card_number"] = "4111111111111111"
	rejected := env.request(t, http.MethodPost, path, "payment_method.manage", "setup-unsafe", unsafe)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("unsafe request status=%d body=%s", rejected.Code, rejected.Body.String())
	}

	var methods, consents, audits int
	var storedURL string
	var encryptedMethod []byte
	ctx := context.Background()
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM payment_methods`).Scan(&methods); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM payment_consents`).Scan(&consents); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*) FROM billing_audit_events`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT hosted_url_sha256 FROM payment_method_setup_sessions LIMIT 1`).Scan(&storedURL); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT provider_method_ref_ciphertext FROM payment_methods LIMIT 1`).Scan(&encryptedMethod); err != nil {
		t.Fatal(err)
	}
	if methods != 1 || consents != 1 || audits != 2 || len(storedURL) != 64 || strings.Contains(string(encryptedMethod), "method-reference-sensitive") {
		t.Fatalf("unexpected durable state methods=%d consents=%d audits=%d url_digest=%q", methods, consents, audits, storedURL)
	}
}

func TestIntegrationPaymentSimulatorHostedSetupActivatesMethodWithoutPersistingRawToken(t *testing.T) {
	env := newIntegrationAPI(t)
	const sharedSecret = "simulator-shared-secret-0123456789abcdef"
	const callbackSecret = "simulator-callback-secret-0123456789abcdef"
	now := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	callbackServer := httptest.NewServer(env.server.Router())
	t.Cleanup(callbackServer.Close)
	simulatorServer, err := paymentsimulator.New(env.db, paymentsimulator.Config{
		Environment: "test", PublicBaseURL: "https://payment-simulator.invalid",
		CallbackURL:  callbackServer.URL + "/v1/internal/payment-simulator/setup-callback",
		SharedSecret: sharedSecret, CallbackSecret: callbackSecret, Retention: time.Hour,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	simulatorHTTP := httptest.NewServer(simulatorServer.Handler())
	t.Cleanup(simulatorHTTP.Close)
	provider, err := simulatorprovider.New(simulatorprovider.Config{
		BaseURL: simulatorHTTP.URL, SharedSecret: sharedSecret,
		RunID: "billing-api-simulator", Scenario: "success",
	})
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, 32))
	protector, err := paymentcrypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.server.ConfigurePayments(PaymentAPIOptions{
		Store: paymentstore.New(env.db), Providers: []payment.PaymentProvider{provider},
		ReferenceProtector: protector, BillingDebitToken: integrationServiceToken,
		BillingDebitSource: "rtk_billing", SimulatorCallbackSecret: callbackSecret,
		Now: func() time.Time { return now },
	}); err != nil {
		t.Fatal(err)
	}

	organizationID := testutil.OrganizationID("billing-api-simulator")
	path := "/v1/orgs/" + organizationID + "/payment-methods/setup"
	body := map[string]any{
		"provider": "simulator",
		"consent": map[string]any{
			"accepted": true, "text_version": "payment-method-v1",
			"text_sha256": strings.Repeat("b", 64), "locale": "zh-TW",
		},
	}
	setup := env.request(t, http.MethodPost, path, "payment_method.manage", "simulator-setup-001", body)
	if setup.Code != http.StatusAccepted {
		t.Fatalf("setup status=%d body=%s", setup.Code, setup.Body.String())
	}
	var setupBody struct {
		PaymentMethod payment.PaymentMethod `json:"payment_method"`
		HostedURL     string                `json:"hosted_url"`
	}
	if err := json.Unmarshal(setup.Body.Bytes(), &setupBody); err != nil {
		t.Fatal(err)
	}
	if setupBody.PaymentMethod.Status != payment.PaymentMethodStatusPending {
		t.Fatalf("initial method status=%s", setupBody.PaymentMethod.Status)
	}
	hostedURL, err := url.Parse(setupBody.HostedURL)
	if err != nil {
		t.Fatal(err)
	}
	rawToken := strings.TrimPrefix(hostedURL.Path, "/setup/")
	if len(rawToken) != 64 {
		t.Fatalf("unexpected hosted token length=%d", len(rawToken))
	}
	page, err := http.Get(simulatorHTTP.URL + hostedURL.Path) // #nosec G107 -- local test server.
	if err != nil {
		t.Fatal(err)
	}
	pageBody, _ := io.ReadAll(page.Body)
	_ = page.Body.Close()
	if page.StatusCode != http.StatusOK || strings.Contains(strings.ToLower(string(pageBody)), "card number") || strings.Contains(strings.ToLower(string(pageBody)), "cvv") {
		t.Fatalf("unsafe hosted page status=%d body=%s", page.StatusCode, string(pageBody))
	}
	completed, err := http.Post(simulatorHTTP.URL+hostedURL.Path+"/complete", "application/x-www-form-urlencoded", strings.NewReader("")) // #nosec G107 -- local test server.
	if err != nil {
		t.Fatal(err)
	}
	_ = completed.Body.Close()
	if completed.StatusCode != http.StatusOK {
		t.Fatalf("completion status=%d", completed.StatusCode)
	}

	replay := env.request(t, http.MethodPost, path, "payment_method.manage", "simulator-setup-001", body)
	if replay.Code != http.StatusAccepted || !strings.Contains(replay.Body.String(), `"duplicate":true`) {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	var methodStatus payment.PaymentMethodStatus
	var persistedSession string
	var methodCount, consentCount int
	ctx := context.Background()
	if err := env.db.QueryRow(ctx, `SELECT status FROM payment_methods WHERE id = $1`, setupBody.PaymentMethod.ID).Scan(&methodStatus); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT row_to_json(s)::text FROM payment_simulator_setup_sessions s LIMIT 1`).Scan(&persistedSession); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT count(*), count(DISTINCT consent_id) FROM payment_methods WHERE account_id = $1 AND provider = 'simulator'`, setupBody.PaymentMethod.AccountID).Scan(&methodCount, &consentCount); err != nil {
		t.Fatal(err)
	}
	if methodStatus != payment.PaymentMethodStatusActive || strings.Contains(persistedSession, rawToken) || methodCount != 1 || consentCount != 1 {
		t.Fatalf("status=%s raw_token_persisted=%t methods=%d consents=%d", methodStatus, strings.Contains(persistedSession, rawToken), methodCount, consentCount)
	}
}
