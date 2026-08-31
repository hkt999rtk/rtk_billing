package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func privacyDay(day int) time.Time { return time.Date(2026, 8, day, 0, 0, 0, 0, time.UTC) }

func privacyWrite(env integrationAPI, method, path, user, version, etag, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+integrationServiceToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Billing-Actor-Type", "user")
	req.Header.Set("X-Billing-Actor-ID", user)
	req.Header.Set("X-Billing-Ownership-Version", version)
	req.Header.Set("X-Billing-Permissions", allBillingPermissions)
	req.Header.Set("X-Request-ID", "privacy-write-test")
	req.Header.Set("If-Match", etag)
	res := httptest.NewRecorder()
	env.server.Router().ServeHTTP(res, req)
	return res
}

// These fixtures exercise real local stores and HTTP reads. Collector/AM receipts
// remain synthetic; they do not assert production producer completeness or migration.
func privacyTransfer(t *testing.T, env integrationAPI, org, source, target string, version int64, at time.Time) {
	t.Helper()
	ctx := context.Background()
	store := paymentstore.New(env.db)
	prepare := paymentstore.PrepareOwnershipHandoffInput{OrganizationID: org, OperationID: testutil.OrganizationID(fmt.Sprintf("privacy-transfer-%d", version)), SourceUserID: source, TargetUserID: target, OwnershipVersion: version, Cutoff: at}
	if _, err := store.PrepareOwnershipHandoff(ctx, prepare); err != nil {
		t.Fatal(err)
	}
	scope := paymentstore.HandoffScope{OrganizationID: org, OperationID: prepare.OperationID, OwnershipVersion: version}
	state, err := store.CaptureHandoffSettlementState(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	state.Financial.UsageSettled, state.Financial.InvoicesReconciled, state.Financial.ProviderWorkReconciled = true, true, true
	status, err := store.RecordHandoffSettlement(ctx, paymentstore.RecordSettlementInput{Scope: scope, ReceiptID: testutil.OrganizationID(prepare.OperationID + "-receipt"), StateSHA256: state.SHA256, Financial: state.Financial, UsageCheckpointSHA256: strings.Repeat("a", 64), InvoiceCheckpointSHA256: strings.Repeat("b", 64), ProviderCheckpointSHA256: strings.Repeat("c", 64)})
	if err != nil || status.Snapshot == nil {
		t.Fatalf("snapshot=%+v err=%v", status, err)
	}
	for _, user := range []string{source, target} {
		if _, err := store.ConfirmHandoffSnapshot(ctx, paymentstore.ConfirmHandoffSnapshotInput{Scope: scope, UserID: user, SnapshotVersion: status.Snapshot.Version, BalanceMinor: status.Snapshot.BalanceMinor, Currency: status.Snapshot.Currency}); err != nil {
			t.Fatal(err)
		}
	}
	grant, err := store.AuthorizeHandoffCommit(ctx, paymentstore.AuthorizeHandoffCommitInput{Scope: scope, AuthorizationID: testutil.OrganizationID(prepare.OperationID + "-grant"), SnapshotVersion: status.Snapshot.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeOwnershipHandoff(ctx, paymentstore.FinalizeHandoffInput{Scope: scope, AuthorizationID: grant.AuthorizationID, CommittedOwnerUserID: target, CommittedOwnershipVersion: version + 1, CommittedAt: at, AMCommitSHA256: strings.Repeat("d", 64)}); err != nil {
		t.Fatal(err)
	}
}

type privatePeriodRecords struct{ invoiceID, invoiceNumber, intentID, methodID, activityID, ledgerID string }

func privacyRecords(t *testing.T, env integrationAPI, org, accountID, user, label string, version int64, start time.Time, quantity int64) privatePeriodRecords {
	t.Helper()
	ctx := context.Background()
	b := billingstore.New(env.db)
	p := paymentstore.New(env.db)
	tenant := ctx
	if version > 0 {
		claims, err := billingidentity.New(env.db).AuthorizeOwner(ctx, org, user, version)
		if err != nil {
			t.Fatal(err)
		}
		tenant = billingidentity.WithScope(ctx, claims)
	}
	profile, _, err := b.EnsureBillingProfile(tenant, org, start)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.PutBillingProfile(tenant, billingstore.PutProfileInput{OrganizationID: org, LegalName: label + "-private-payer", TaxIdentifier: label + "-private-tax", ContactEmail: label + "@example.test", Locale: "zh-TW", Timezone: "UTC", DeliveryPreference: "portal", ExpectedVersion: profile.Version}); err != nil {
		t.Fatal(err)
	}
	consent, err := p.CreateConsent(tenant, paymentstore.CreateConsentInput{AccountID: accountID, ConsentType: "payment_method", TextVersion: "v1", TextSHA256: strings.Repeat("a", 64), AcceptedActorType: "user", AcceptedActorID: user, AcceptedAt: start, Locale: "zh-TW", Source: "privacy-test"})
	if err != nil {
		t.Fatal(err)
	}
	method, err := p.CreatePaymentMethod(tenant, paymentstore.CreatePaymentMethodInput{AccountID: accountID, Provider: "fake", ProviderCustomerRefCiphertext: []byte("encrypted-customer-" + label), ProviderMethodRefCiphertext: []byte("encrypted-method-" + label), ProviderMethodRefSHA256: strings.Repeat(fmt.Sprint(version), 64), CardBrand: label + "-private-card", LastFour: "4242", Capabilities: payment.ProviderCapabilities{MerchantInitiatedCharge: true}, Status: payment.PaymentMethodStatusActive, ConsentID: consent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if label == "middle" {
		policyConsent, err := p.CreateConsent(tenant, paymentstore.CreateConsentInput{AccountID: accountID, ConsentType: "auto_topup", TextVersion: "v1", TextSHA256: strings.Repeat("b", 64), AcceptedActorType: "user", AcceptedActorID: user, AcceptedAt: start, Locale: "zh-TW", Source: "privacy-test"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.PutAutoTopUpPolicy(tenant, paymentstore.PutAutoTopUpPolicyInput{AccountID: accountID, Enabled: true, ThresholdMinor: 777, TopUpAmountMinor: 10000, Currency: payment.CurrencyTWD, PaymentMethodID: method.ID, DailyAttemptLimit: 2, DailyAmountLimitMinor: 20000, CooldownSeconds: 3600, ConsentID: policyConsent.ID, ActorID: user}); err != nil {
			t.Fatal(err)
		}
	}
	intent, err := p.CreateManualTopUp(tenant, paymentstore.CreateManualTopUpInput{AccountID: accountID, PaymentMethodID: method.ID, AmountMinor: 10000, Currency: payment.CurrencyTWD, IdempotencyKey: label + "-topup", CorrelationID: label + "-private-intent", Now: start})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []payment.PaymentIntentState{payment.PaymentIntentStateProcessing, payment.PaymentIntentStateSucceeded} {
		if _, err := p.TransitionIntent(ctx, paymentstore.TransitionIntentInput{IntentID: intent.Intent.ID, ToState: state, ProviderTransactionReference: label + "-private-provider", Now: start}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.db.Exec(ctx, `UPDATE payment_reconciliation_jobs SET status='completed' WHERE intent_id=$1`, intent.Intent.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.PutUsageFact(ctx, billing.UsageFact{UsageID: label + "-usage", OrganizationID: org, ServiceCode: "video", MetricCode: "minutes", Quantity: quantity, Unit: "minute", WindowStart: start, WindowEnd: start.Add(24 * time.Hour), Source: "privacy-fixture", SourceSHA256: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	invoice, _, err := b.PrepareInvoice(ctx, billingstore.PrepareInvoiceInput{OrganizationID: org, AccountID: accountID, Currency: billing.CurrencyTWD, PeriodStart: start, PeriodEnd: start.Add(24 * time.Hour), Now: start.Add(25 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := p.PostLedgerEntry(ctx, paymentstore.PostLedgerEntryInput{AccountID: accountID, Direction: payment.LedgerDirectionDebit, AmountMinor: invoice.TotalMinor, Currency: payment.CurrencyTWD, Reason: payment.LedgerReasonInvoiceDebit, IdempotencyScope: "billing_invoice", IdempotencyKey: invoice.ID, ExternalType: "invoice", ExternalID: invoice.ID, Now: start.Add(25 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.RecordInvoiceSettlement(ctx, org, invoice.ID, entry.Entry.ID, start.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Deliberately identifying bytes test the authorization gate, not PDF rendering.
	data := []byte("%PDF-fixture-" + label + "-private-payer")
	digest := sha256.Sum256(data)
	if err := b.PutInvoiceDocument(ctx, org, invoice.ID, billing.InvoiceDocument{ContentType: "application/pdf", ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), InvoiceVersion: invoice.Version, RendererVersion: "privacy-test", GeneratedAt: start.Add(25 * time.Hour)}, data); err != nil {
		t.Fatal(err)
	}
	var activityID string
	if err := env.db.QueryRow(ctx, `SELECT id::text FROM billing_activity_events WHERE resource_id=$1`, invoice.ID).Scan(&activityID); err != nil {
		t.Fatal(err)
	}
	return privatePeriodRecords{invoice.ID, invoice.InvoiceNumber, intent.Intent.ID, method.ID, activityID, entry.Entry.ID}
}

func TestTenantFinancialHistoryScopesCountsDownloadsAndReturningOwner(t *testing.T) {
	env := newIntegrationAPI(t)
	org := testutil.OrganizationID("privacy-cloud")
	env.provisionOwner(t, org)
	ctx := context.Background()
	p := paymentstore.New(env.db)
	b := billingstore.New(env.db)
	account, err := p.GetCommercialAccountByOrganization(ctx, org, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := b.CreatePricingVersion(ctx, billingstore.CreatePricingVersionInput{PlanKey: "privacy", Version: 1, Currency: billing.CurrencyTWD, EffectiveFrom: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), CreatedBy: "test", Now: privacyDay(1), Rates: []billing.PricingRate{{ServiceCode: "video", MetricCode: "minutes", Description: "Video", Unit: "minute", UnitPriceMinor: 2, RoundingMode: billing.RoundingHalfUp}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ActivatePricingVersion(ctx, pricing.ID, privacyDay(1)); err != nil {
		t.Fatal(err)
	}
	a := testutil.OrganizationID("integration-owner")
	other := testutil.OrganizationID("intervening-owner")
	first := privacyRecords(t, env, org, account.ID, a, "first", 1, privacyDay(2), 11)
	privacyTransfer(t, env, org, a, other, 1, privacyDay(10))
	base := "/v1/orgs/" + org
	for _, path := range []string{"/billing/invoices/" + first.invoiceID, "/billing/invoices/" + first.invoiceID + "/pdf", "/billing/activity/" + first.activityID, "/payment-intents/" + first.intentID} {
		res := ownerRequest(env, "GET", base+path, other, "2")
		if res.Code != http.StatusNotFound {
			t.Fatalf("successor predecessor read %s=%d %s", path, res.Code, res.Body.String())
		}
	}
	for _, path := range []string{"/billing/ledger", "/payment-methods", "/payment-intents", "/billing/activity", "/billing/invoices"} {
		res := ownerRequest(env, "GET", base+path, other, "2")
		if res.Code != http.StatusOK || strings.Contains(res.Body.String(), first.invoiceID) || strings.Contains(res.Body.String(), first.methodID) || strings.Contains(res.Body.String(), first.intentID) || !strings.Contains(res.Body.String(), `"total":0`) {
			t.Fatalf("successor list %s=%d %s", path, res.Code, res.Body.String())
		}
	}
	middle := privacyRecords(t, env, org, account.ID, other, "middle", 2, privacyDay(12), 997)
	privacyTransfer(t, env, org, other, a, 2, privacyDay(20))
	last := privacyRecords(t, env, org, account.ID, a, "last", 3, privacyDay(22), 23)
	if res := ownerRequest(env, "GET", base+"/auto-topup", a, "3"); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"auto_topup":null`) {
		t.Fatalf("predecessor policy exposed=%d %s", res.Code, res.Body.String())
	}
	if res := privacyWrite(env, "DELETE", base+"/auto-topup", a, "3", `"0"`, `{"reason":"disable my policy"}`); res.Code != http.StatusNotFound {
		t.Fatalf("predecessor policy disable exposed=%d %s", res.Code, res.Body.String())
	}
	claims, err := billingidentity.New(env.db).AuthorizeOwner(ctx, org, a, 3)
	if err != nil {
		t.Fatal(err)
	}
	tenant := billingidentity.WithScope(ctx, claims)
	if _, err := p.GetPaymentMethod(tenant, account.ID, middle.methodID); err != paymentstore.ErrNotFound {
		t.Fatalf("predecessor method ID read: %v", err)
	}
	for _, methodID := range []string{first.methodID, middle.methodID} {
		if res := privacyWrite(env, "DELETE", base+"/payment-methods/"+methodID, a, "3", "", `{"reason":"revoke old method"}`); res.Code != http.StatusNotFound {
			t.Fatalf("old period method mutation=%d %s", res.Code, res.Body.String())
		}
	}
	for label, methodID := range map[string]string{"first": first.methodID, "middle": middle.methodID, "last": last.methodID} {
		replay, err := p.CreateManualTopUp(tenant, paymentstore.CreateManualTopUpInput{AccountID: account.ID, PaymentMethodID: methodID, AmountMinor: 10000, Currency: payment.CurrencyTWD, IdempotencyKey: label + "-topup", CorrelationID: label + "-private-intent", Now: privacyDay(25)})
		if label == "last" {
			if err != nil || !replay.Duplicate || replay.Intent.ID != last.intentID {
				t.Fatalf("current retry=%+v err=%v", replay, err)
			}
		} else if err != paymentstore.ErrIdempotencyConflict || replay.Intent.ID != "" {
			t.Fatalf("historical key %s exposed=%+v err=%v", label, replay, err)
		}
	}
	env.server.billing.now = func() time.Time { return privacyDay(25) }
	for _, path := range []string{"/billing/invoices", "/billing/statements", "/billing/ledger", "/payment-methods", "/payment-intents", "/billing/activity", "/billing/summary"} {
		res := ownerRequest(env, "GET", base+path, a, "3")
		if res.Code != http.StatusOK {
			t.Fatalf("current list %s=%d %s", path, res.Code, res.Body.String())
		}
		for _, secret := range []string{middle.invoiceID, middle.invoiceNumber, middle.intentID, middle.methodID, middle.activityID, middle.ledgerID, "middle-private"} {
			if strings.Contains(res.Body.String(), secret) {
				t.Fatalf("%s leaked %s: %s", path, secret, res.Body.String())
			}
		}
		counts := map[string]int{"/billing/invoices": 2, "/billing/ledger": 4, "/payment-methods": 2, "/payment-intents": 2, "/billing/activity": 4}
		if count, ok := counts[path]; ok && !strings.Contains(res.Body.String(), fmt.Sprintf(`"total":%d`, count)) {
			t.Fatalf("incorrect visible count %s: %s", path, res.Body.String())
		}
	}
	res := ownerRequest(env, "GET", base+"/billing/invoices?limit=1&offset=1", a, "3")
	var page struct {
		Invoices []billing.Invoice `json:"invoices"`
		Page     billingstore.Page `json:"pagination"`
	}
	if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &page) != nil || page.Page.Total != 2 || len(page.Invoices) != 1 || page.Invoices[0].ID != first.invoiceID {
		t.Fatalf("scoped pagination=%d %s", res.Code, res.Body.String())
	}
	for _, records := range []privatePeriodRecords{first, last} {
		for _, path := range []string{"/billing/invoices/" + records.invoiceID, "/billing/invoices/" + records.invoiceID + "/pdf", "/billing/activity/" + records.activityID, "/payment-intents/" + records.intentID} {
			res := ownerRequest(env, "GET", base+path, a, "3")
			if res.Code != http.StatusOK {
				t.Fatalf("own period %s=%d %s", path, res.Code, res.Body.String())
			}
		}
	}
	for _, path := range []string{"/billing/invoices/" + middle.invoiceID, "/billing/invoices/" + middle.invoiceID + "/pdf", "/billing/activity/" + middle.activityID, "/payment-intents/" + middle.intentID} {
		res := ownerRequest(env, "GET", base+path, a, "3")
		if res.Code != http.StatusNotFound {
			t.Fatalf("intervening owner %s=%d %s", path, res.Code, res.Body.String())
		}
	}
	if res := ownerRequest(env, "GET", base+"/billing/invoices/"+middle.invoiceID, other, "2"); res.Code != http.StatusForbidden {
		t.Fatalf("departed owner retained own history=%d %s", res.Code, res.Body.String())
	}
	for _, window := range []struct {
		name       string
		start, end time.Time
	}{{"mixed", privacyDay(9), privacyDay(11)}, {"unproven", privacyDay(-40), privacyDay(-39)}} {
		if _, _, err := b.PutUsageFact(ctx, billing.UsageFact{UsageID: window.name, OrganizationID: org, ServiceCode: "video", MetricCode: "minutes", Quantity: 100000, Unit: "minute", WindowStart: window.start, WindowEnd: window.end, Source: "import-fixture", SourceSHA256: strings.Repeat("f", 64)}); err != nil {
			t.Fatal(err)
		}
	}
	res = ownerRequest(env, "GET", base+"/billing/usage?period_start=2026-06-01T00:00:00Z&period_end=2026-09-01T00:00:00Z", a, "3")
	var usage struct {
		Total int64 `json:"total_minor"`
		Count int   `json:"fact_count"`
	}
	if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &usage) != nil || usage.Total != 68 || usage.Count != 2 {
		t.Fatalf("period-scoped usage=%d %s", res.Code, res.Body.String())
	}
	res = ownerRequest(env, "GET", base+"/billing/summary", a, "3")
	var summary struct {
		Period struct {
			Start time.Time `json:"period_start"`
			Total int64     `json:"total_minor"`
		} `json:"current_period"`
		Forecast struct {
			Average *int64 `json:"average_daily_cost_minor"`
		} `json:"forecast"`
	}
	if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &summary) != nil || !summary.Period.Start.Equal(privacyDay(20)) || summary.Period.Total != 46 || summary.Forecast.Average == nil || *summary.Forecast.Average != 15 {
		t.Fatalf("summary/forecast used hidden history=%d %s", res.Code, res.Body.String())
	}
	// A current recipient version cannot make a mixed historical invoice visible.
	mixed, _, err := b.PrepareInvoice(ctx, billingstore.PrepareInvoiceInput{OrganizationID: org, AccountID: account.ID, Currency: billing.CurrencyTWD, PeriodStart: privacyDay(9), PeriodEnd: privacyDay(11), Now: privacyDay(25)})
	if err != nil {
		t.Fatal(err)
	}
	if res := ownerRequest(env, "GET", base+"/billing/invoices/"+mixed.ID, a, "3"); res.Code != http.StatusNotFound {
		t.Fatalf("mixed invoice visible=%d %s", res.Code, res.Body.String())
	}
	if res := ownerRequest(env, "GET", base+"/billing/invoices", a, "3"); res.Code != http.StatusOK || strings.Contains(res.Body.String(), mixed.ID) || !strings.Contains(res.Body.String(), `"total":2`) {
		t.Fatalf("mixed invoice counted=%d %s", res.Code, res.Body.String())
	}
	for _, usageID := range []string{"mixed", "unproven", "middle-usage"} {
		if _, err := b.GetUsageFact(tenant, usageID); err != billingstore.ErrNotFound {
			t.Fatalf("hidden usage ID %s: %v", usageID, err)
		}
	}
	if _, err := b.GetUsageFact(tenant, "first-usage"); err != nil {
		t.Fatalf("returning owner's own usage: %v", err)
	}
	// The same account-level policy row can be freshly configured only with a new
	// current-period method/consent. Preserve the predecessor snapshot internally.
	policyBody := fmt.Sprintf(`{"enabled":true,"threshold_minor":123,"top_up_amount_minor":10000,"currency":"TWD","payment_method_id":%q,"daily_attempt_limit":2,"daily_amount_limit_minor":20000,"cooldown_seconds":3600,"consent":{"accepted":true,"text_version":"v2","text_sha256":%q,"locale":"zh-TW"}}`, last.methodID, strings.Repeat("e", 64))
	res = privacyWrite(env, "PUT", base+"/auto-topup", a, "3", `"0"`, policyBody)
	if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "777") || !strings.Contains(res.Body.String(), last.methodID) {
		t.Fatalf("new owner policy configuration=%d %s", res.Code, res.Body.String())
	}
	if res := privacyWrite(env, "PUT", base+"/auto-topup", a, "3", `"0"`, policyBody); res.Code != http.StatusConflict {
		t.Fatalf("blind overwrite current policy=%d %s", res.Code, res.Body.String())
	}
	var actor, archivedActor string
	var version, archivedVersion, archivedThreshold int64
	var created, archivedCreated time.Time
	if err := env.db.QueryRow(ctx, `SELECT created_by,version,created_at FROM auto_topup_policies WHERE account_id=$1`, account.ID).Scan(&actor, &version, &created); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT snapshot->>'created_by',policy_version,(snapshot->>'threshold_minor')::bigint,(snapshot->>'created_at')::timestamptz FROM billing_retired_policy_evidence WHERE account_id=$1`, account.ID).Scan(&archivedActor, &archivedVersion, &archivedThreshold, &archivedCreated); err != nil {
		t.Fatal(err)
	}
	if actor != a || archivedActor != other || archivedThreshold != 777 || version != archivedVersion+1 || !created.After(archivedCreated) {
		t.Fatalf("policy replacement lost provenance: actor=%s archived=%s versions=%d/%d threshold=%d", actor, archivedActor, version, archivedVersion, archivedThreshold)
	}
	if _, err := env.db.Exec(ctx, `DELETE FROM billing_retired_policy_evidence WHERE account_id=$1`, account.ID); err == nil {
		t.Fatal("retired policy evidence was mutable")
	}
}

func TestTenantFinancialLegacyRecordsAreNotInferredFromCurrentOwnership(t *testing.T) {
	env := newIntegrationAPI(t)
	ctx := context.Background()
	org := testutil.OrganizationID("unproven-financial-records")
	b := billingstore.New(env.db)
	p := paymentstore.New(env.db)
	account, _, err := p.EnsureCommercialAccount(ctx, org, payment.CurrencyTWD)
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := b.CreatePricingVersion(ctx, billingstore.CreatePricingVersionInput{PlanKey: "legacy", Version: 1, Currency: billing.CurrencyTWD, EffectiveFrom: privacyDay(1), CreatedBy: "test", Now: privacyDay(1), Rates: []billing.PricingRate{{ServiceCode: "video", MetricCode: "minutes", Description: "Video", Unit: "minute", UnitPriceMinor: 2, RoundingMode: billing.RoundingHalfUp}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ActivatePricingVersion(ctx, pricing.ID, privacyDay(1)); err != nil {
		t.Fatal(err)
	}
	legacy := privacyRecords(t, env, org, account.ID, testutil.OrganizationID("unproven-old-payer"), "unproven", 0, privacyDay(2), 11)
	env.provisionOwner(t, org)
	base := "/v1/orgs/" + org
	owner := testutil.OrganizationID("integration-owner")
	for _, path := range []string{"/billing/invoices", "/billing/ledger", "/billing/activity", "/payment-methods", "/payment-intents"} {
		res := ownerRequest(env, "GET", base+path, owner, "1")
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"total":0`) {
			t.Fatalf("unknown legacy %s=%d %s", path, res.Code, res.Body.String())
		}
	}
	for _, path := range []string{"/billing/invoices/" + legacy.invoiceID, "/billing/invoices/" + legacy.invoiceID + "/pdf", "/billing/activity/" + legacy.activityID, "/payment-intents/" + legacy.intentID} {
		res := ownerRequest(env, "GET", base+path, owner, "1")
		if res.Code != http.StatusNotFound {
			t.Fatalf("unknown detail %s=%d %s", path, res.Code, res.Body.String())
		}
	}
	claims, err := billingidentity.New(env.db).AuthorizeOwner(ctx, org, owner, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetPaymentMethod(billingidentity.WithScope(ctx, claims), account.ID, legacy.methodID); err != paymentstore.ErrNotFound {
		t.Fatalf("unknown method id: %v", err)
	}
	if _, err := p.ListPaymentAttempts(billingidentity.WithScope(ctx, claims), legacy.intentID); err != paymentstore.ErrNotFound {
		t.Fatalf("unknown attempts id: %v", err)
	}
}
