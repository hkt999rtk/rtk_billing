package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	"github.com/hkt999rtk/rtk_billing/internal/billingservice"
	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentcrypto"
	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/fake"
	simulatorprovider "github.com/hkt999rtk/rtk_billing/internal/paymentprovider/simulator"
	"github.com/hkt999rtk/rtk_billing/internal/paymentsimulator"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

const (
	integrationServiceToken  = "billing-integration-service-token-0001"
	integrationInternalToken = "billing-integration-internal-token-001"
	integrationDebitToken    = "billing-integration-debit-token-000001"
)

type integrationAPI struct {
	server *Server
	db     *pgxpool.Pool
}

func newIntegrationAPI(t *testing.T, providers ...payment.PaymentProvider) integrationAPI {
	return newIntegrationAPIWithOptions(t, nil, providers...)
}

func newIntegrationAPIWithOptions(t *testing.T, configure func(*PaymentAPIOptions), providers ...payment.PaymentProvider) integrationAPI {
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
			billing_activity_events,
			invoice_settlement_links,
			billing_invoice_documents,
			billing_invoice_lines,
			billing_invoices,
			billing_periods,
			billing_usage_facts,
			pricing_rates,
			pricing_plan_versions,
			billing_profiles,
			payment_simulator_operations,
			payment_simulator_newebpay_transactions,
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
		ServiceToken:  integrationServiceToken,
		InternalToken: integrationInternalToken,
		Audit:         AuditAdapter{Store: auditstore.New(db)},
		Access:        accessstore.New(db),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	protector, err := paymentcrypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	paymentOptions := PaymentAPIOptions{
		Store:              paymentstore.New(db),
		Providers:          providers,
		ReferenceProtector: protector,
		BillingDebitToken:  integrationDebitToken,
		BillingDebitSource: "rtk_billing",
		Now: func() time.Time {
			return time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC)
		},
	}
	if configure != nil {
		configure(&paymentOptions)
	}
	if err := server.ConfigurePayments(paymentOptions); err != nil {
		t.Fatal(err)
	}
	billingStore := billingstore.New(db)
	billingService, err := billingservice.New(billingservice.Options{
		Store: billingStore, PaymentStore: paymentstore.New(db),
		Now: func() time.Time { return time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ConfigureBilling(BillingAPIOptions{
		Store: billingStore, Service: billingService,
		Now: func() time.Time { return time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC) },
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
	token := integrationServiceToken
	if strings.HasPrefix(path, "/v1/internal/billing/debits") {
		token = integrationDebitToken
	} else if strings.HasPrefix(path, "/v1/internal/") {
		token = integrationInternalToken
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
	if err := env.server.ConfigurePayments(PaymentAPIOptions{
		Store: paymentstore.New(env.db), BillingDebitToken: integrationServiceToken, BillingDebitSource: "reused-credential",
	}); err == nil || !strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("reused debit credential error = %v", err)
	}
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
	sharedCredential := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
	sharedCredential.Header.Set("Content-Type", "application/json")
	sharedCredential.Header.Set("Authorization", "Bearer "+integrationServiceToken)
	sharedCredentialResponse := httptest.NewRecorder()
	env.server.Router().ServeHTTP(sharedCredentialResponse, sharedCredential)
	if sharedCredentialResponse.Code != http.StatusUnauthorized {
		t.Fatalf("tenant credential crossed debit boundary: status=%d body=%s", sharedCredentialResponse.Code, sharedCredentialResponse.Body.String())
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

func TestIntegrationBillingHTTPPricingInvoiceAndTenantReadLifecycle(t *testing.T) {
	env := newIntegrationAPI(t)
	organizationID := testutil.OrganizationID("billing-api-invoice")
	periodStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	pricingBody := map[string]any{
		"plan_key": "default", "version": 1, "currency": "TWD", "effective_from": periodStart,
		"rates": []map[string]any{{
			"service_code": "video", "metric_code": "relay_minutes", "description": "Video relay",
			"unit": "minute", "unit_price_minor": 2, "unit_price_scale": 0,
			"rounding_mode": "half_up", "tax_rate_basis_points": 0,
		}},
	}
	createdPricing := env.request(t, http.MethodPost, "/v1/internal/billing/pricing-versions", "", "", pricingBody)
	if createdPricing.Code != http.StatusCreated {
		t.Fatalf("create pricing status=%d body=%s", createdPricing.Code, createdPricing.Body.String())
	}
	var pricingResponse struct {
		PricingVersion struct {
			ID string `json:"id"`
		} `json:"pricing_version"`
	}
	if err := json.Unmarshal(createdPricing.Body.Bytes(), &pricingResponse); err != nil || pricingResponse.PricingVersion.ID == "" {
		t.Fatalf("pricing response=%s err=%v", createdPricing.Body.String(), err)
	}
	activated := env.request(t, http.MethodPost, "/v1/internal/billing/pricing-versions/"+pricingResponse.PricingVersion.ID+"/activate", "", "", nil)
	if activated.Code != http.StatusOK {
		t.Fatalf("activate pricing status=%d body=%s", activated.Code, activated.Body.String())
	}

	digest := sha256.Sum256([]byte("billing-api-usage"))
	usageBody := map[string]any{
		"usage_id": "billing-api-usage", "organization_id": organizationID,
		"service_code": "video", "metric_code": "relay_minutes", "quantity": 100,
		"quantity_scale": 0, "unit": "minute", "window_start": periodStart,
		"window_end": periodStart.Add(24 * time.Hour), "source": "integration-test",
		"source_sha256": fmt.Sprintf("%x", digest),
	}
	usage := env.request(t, http.MethodPost, "/v1/internal/billing/usage-facts", "", "", usageBody)
	if usage.Code != http.StatusCreated {
		t.Fatalf("usage status=%d body=%s", usage.Code, usage.Body.String())
	}
	closed := env.request(t, http.MethodPost, "/v1/internal/billing/periods/close", "", "", map[string]any{
		"organization_id": organizationID, "period_start": periodStart, "period_end": periodEnd,
		"due_at": periodEnd.Add(15 * 24 * time.Hour),
	})
	if closed.Code != http.StatusOK || !strings.Contains(closed.Body.String(), `"state":"settled"`) || !strings.Contains(closed.Body.String(), `"total_minor":200`) {
		t.Fatalf("close status=%d body=%s", closed.Code, closed.Body.String())
	}

	invoices := env.request(t, http.MethodGet, "/v1/orgs/"+organizationID+"/billing/invoices", "invoice.read", "", nil)
	var invoicePage struct {
		Invoices []struct {
			ID string `json:"id"`
		} `json:"invoices"`
	}
	if invoices.Code != http.StatusOK || json.Unmarshal(invoices.Body.Bytes(), &invoicePage) != nil || len(invoicePage.Invoices) != 1 {
		t.Fatalf("invoice list status=%d body=%s", invoices.Code, invoices.Body.String())
	}
	invoiceID := invoicePage.Invoices[0].ID
	pdf := env.request(t, http.MethodGet, "/v1/orgs/"+organizationID+"/billing/invoices/"+invoiceID+"/pdf", "invoice_document.read", "", nil)
	if pdf.Code != http.StatusOK || !bytes.HasPrefix(pdf.Body.Bytes(), []byte("%PDF")) || pdf.Header().Get("Digest") == "" {
		t.Fatalf("invoice PDF status=%d digest=%q body=%q", pdf.Code, pdf.Header().Get("Digest"), pdf.Body.String())
	}
	summary := env.request(t, http.MethodGet, "/v1/orgs/"+organizationID+"/billing/summary", "billing_summary.read", "", nil)
	if summary.Code != http.StatusOK || !strings.Contains(summary.Body.String(), `"available_balance_minor":-200`) {
		t.Fatalf("summary status=%d body=%s", summary.Code, summary.Body.String())
	}
	activity := env.request(t, http.MethodGet, "/v1/orgs/"+organizationID+"/billing/activity", "billing_activity.read", "", nil)
	if activity.Code != http.StatusOK || !strings.Contains(activity.Body.String(), `"type":"invoice"`) || !strings.Contains(activity.Body.String(), `"state":"completed"`) {
		t.Fatalf("activity status=%d body=%s", activity.Code, activity.Body.String())
	}

	tenantCredential := httptest.NewRequest(http.MethodPost, "/v1/internal/billing/pricing-versions", bytes.NewBufferString(`{}`))
	tenantCredential.Header.Set("Authorization", "Bearer "+integrationServiceToken)
	tenantCredentialResponse := httptest.NewRecorder()
	env.server.Router().ServeHTTP(tenantCredentialResponse, tenantCredential)
	if tenantCredentialResponse.Code != http.StatusUnauthorized {
		t.Fatalf("tenant credential crossed pricing boundary: %d", tenantCredentialResponse.Code)
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
		ReferenceProtector: protector, BillingDebitToken: integrationDebitToken,
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
