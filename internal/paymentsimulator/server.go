package paymentsimulator

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

const signatureHeader = "X-Payment-Simulator-Signature"

type Config struct {
	Environment    string
	PublicBaseURL  string
	CallbackURL    string
	SharedSecret   string
	CallbackSecret string
	Retention      time.Duration
	HTTPClient     *http.Client
	Now            func() time.Time
}

type Server struct {
	db             *pgxpool.Pool
	publicBaseURL  string
	callbackURL    string
	sharedSecret   []byte
	callbackSecret []byte
	retention      time.Duration
	http           *http.Client
	now            func() time.Time
	mux            *http.ServeMux
}

type setupRequest struct {
	RunID          string `json:"run_id"`
	AccountID      string `json:"account_id"`
	SetupSessionID string `json:"setup_session_id"`
	IdempotencyKey string `json:"idempotency_key"`
	CorrelationID  string `json:"correlation_id"`
	Scenario       string `json:"scenario"`
}

type operationRequest struct {
	RunID                        string           `json:"run_id"`
	IntentID                     string           `json:"intent_id"`
	AmountMinor                  int64            `json:"amount_minor"`
	Currency                     payment.Currency `json:"currency"`
	OpaqueMethodReference        string           `json:"opaque_method_reference"`
	MerchantOrderReference       string           `json:"merchant_order_reference"`
	ProviderTransactionReference string           `json:"provider_transaction_reference"`
	IdempotencyKey               string           `json:"idempotency_key"`
	CorrelationID                string           `json:"correlation_id"`
	Scenario                     string           `json:"scenario"`
}

type setupSession struct {
	RunID               string
	AccountID           string
	SetupSessionID      string
	Scenario            string
	State               payment.PaymentIntentState
	ProviderCustomerRef string
	ProviderMethodRef   string
	ExpiresAt           time.Time
}

type response struct {
	State                        payment.PaymentIntentState `json:"state"`
	HostedURL                    string                     `json:"hosted_url,omitempty"`
	ProviderCustomerRef          string                     `json:"provider_customer_ref,omitempty"`
	ProviderMethodRef            string                     `json:"provider_method_ref,omitempty"`
	ProviderTransactionReference string                     `json:"provider_transaction_reference,omitempty"`
	ProviderCode                 string                     `json:"provider_code"`
	RequiresUserAction           bool                       `json:"requires_user_action,omitempty"`
	CardBrand                    string                     `json:"card_brand,omitempty"`
	LastFour                     string                     `json:"last_four,omitempty"`
	ExpiryMonth                  *int                       `json:"expiry_month,omitempty"`
	ExpiryYear                   *int                       `json:"expiry_year,omitempty"`
	Evidence                     map[string]string          `json:"evidence,omitempty"`
}

func New(db *pgxpool.Pool, config Config) (*Server, error) {
	environment := strings.ToLower(strings.TrimSpace(config.Environment))
	if environment == "production" || environment == "prod" {
		return nil, errors.New("payment simulator is forbidden in production")
	}
	if db == nil {
		return nil, errors.New("payment simulator database is required")
	}
	if environment != "development" && environment != "dev" && environment != "test" && environment != "staging" {
		return nil, errors.New("payment simulator environment must be development, test, or staging")
	}
	publicURL, err := validBaseURL(config.PublicBaseURL)
	if err != nil {
		return nil, fmt.Errorf("public base URL: %w", err)
	}
	callbackURL, err := validBaseURL(config.CallbackURL)
	if err != nil {
		return nil, fmt.Errorf("callback URL: %w", err)
	}
	if len(strings.TrimSpace(config.SharedSecret)) < 32 || len(strings.TrimSpace(config.CallbackSecret)) < 32 {
		return nil, errors.New("payment simulator secrets must contain at least 32 characters")
	}
	if config.Retention <= 0 {
		config.Retention = 24 * time.Hour
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	s := &Server{
		db: db, publicBaseURL: publicURL, callbackURL: callbackURL,
		sharedSecret: []byte(strings.TrimSpace(config.SharedSecret)), callbackSecret: []byte(strings.TrimSpace(config.CallbackSecret)),
		retention: config.Retention, http: config.HTTPClient, now: config.Now, mux: http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /internal/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.Handle("POST /internal/v1/setup-sessions", s.authenticated(http.HandlerFunc(s.createSetup)))
	s.mux.Handle("POST /internal/v1/charges", s.authenticated(http.HandlerFunc(s.charge)))
	s.mux.Handle("POST /internal/v1/queries", s.authenticated(http.HandlerFunc(s.query)))
	s.mux.Handle("POST /internal/v1/refunds", s.authenticated(http.HandlerFunc(s.refund)))
	s.mux.HandleFunc("GET /setup/{token}", s.showSetup)
	s.mux.HandleFunc("POST /setup/{token}/complete", s.completeSetup)
}

func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 128*1024))
		if err != nil || len(body) == 0 || !validSignature(s.sharedSecret, body, r.Header.Get(signatureHeader)) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) createSetup(w http.ResponseWriter, r *http.Request) {
	var input setupRequest
	if !decodeJSON(w, r, &input) || !validRunID(input.RunID) || strings.TrimSpace(input.AccountID) == "" || strings.TrimSpace(input.SetupSessionID) == "" || strings.TrimSpace(input.IdempotencyKey) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_setup"})
		return
	}
	input.Scenario = normalizeScenario(input.Scenario)
	if !validScenario(input.Scenario) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_scenario"})
		return
	}
	if err := s.pruneExpired(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage_failed"})
		return
	}
	var existing setupSession
	err := s.db.QueryRow(r.Context(), `
		SELECT run_id, account_id::text, setup_session_id::text, scenario, state,
		       provider_customer_reference, provider_method_reference, expires_at
		FROM payment_simulator_setup_sessions
		WHERE run_id = $1 AND account_id = $2 AND idempotency_key = $3
	`, input.RunID, input.AccountID, input.IdempotencyKey).Scan(
		&existing.RunID, &existing.AccountID, &existing.SetupSessionID, &existing.Scenario, &existing.State,
		&existing.ProviderCustomerRef, &existing.ProviderMethodRef, &existing.ExpiresAt,
	)
	if err == nil {
		if existing.SetupSessionID != input.SetupSessionID || existing.Scenario != input.Scenario {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "idempotency_conflict"})
			return
		}
		token := setupToken(s.sharedSecret, input)
		result := response{State: existing.State, HostedURL: s.publicBaseURL + "/setup/" + token, ProviderCode: "simulator_setup", RequiresUserAction: existing.State == payment.PaymentIntentStateRequiresAction}
		if existing.State != payment.PaymentIntentStateRequiresAction {
			_, result.ProviderCode = scenarioResult(existing.Scenario)
		}
		if existing.State == payment.PaymentIntentStateSucceeded {
			expiryMonth, expiryYear := 12, 2099
			result.ProviderCustomerRef = existing.ProviderCustomerRef
			result.ProviderMethodRef = existing.ProviderMethodRef
			result.CardBrand, result.LastFour = "simulator", "4242"
			result.ExpiryMonth, result.ExpiryYear = &expiryMonth, &expiryYear
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage_failed"})
		return
	}
	token := setupToken(s.sharedSecret, input)
	tokenDigest := sha256.Sum256([]byte(token))
	hostedURL := s.publicBaseURL + "/setup/" + token
	customerRef := "sim_customer_" + compactID(input.AccountID)
	methodRef := "sim_method_" + compactID(input.SetupSessionID)
	_, err = s.db.Exec(r.Context(), `
		INSERT INTO payment_simulator_setup_sessions (
			run_id, account_id, setup_session_id, idempotency_key, token_sha256,
			scenario, provider_customer_reference, provider_method_reference, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, input.RunID, input.AccountID, input.SetupSessionID, input.IdempotencyKey, hex.EncodeToString(tokenDigest[:]),
		input.Scenario, customerRef, methodRef, s.now().Add(s.retention))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage_failed"})
		return
	}
	writeJSON(w, http.StatusAccepted, response{State: payment.PaymentIntentStateRequiresAction, HostedURL: hostedURL, ProviderCode: "simulator_setup", RequiresUserAction: true})
}

func (s *Server) showSetup(w http.ResponseWriter, r *http.Request) {
	session, ok := s.sessionByToken(r.Context(), r.PathValue("token"))
	if !ok || s.now().After(session.ExpiresAt) {
		http.Error(w, "Setup session not found or expired", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = setupPage.Execute(w, map[string]any{"Action": "/setup/" + url.PathEscape(r.PathValue("token")) + "/complete", "Scenario": session.Scenario})
}

func (s *Server) completeSetup(w http.ResponseWriter, r *http.Request) {
	session, ok := s.sessionByToken(r.Context(), r.PathValue("token"))
	if !ok || s.now().After(session.ExpiresAt) {
		http.Error(w, "Setup session not found or expired", http.StatusNotFound)
		return
	}
	if session.State == payment.PaymentIntentStateSucceeded {
		writeCompletionPage(w, true)
		return
	}
	if session.Scenario == "temporary_error" || session.Scenario == "unknown" {
		http.Error(w, "The simulator could not complete this setup.", http.StatusServiceUnavailable)
		return
	}
	state := payment.PaymentIntentStateSucceeded
	if session.Scenario == "declined" {
		state = payment.PaymentIntentStateFailed
	}
	if session.Scenario == "requires_action" {
		writeCompletionPage(w, false)
		return
	}
	hostedURL := s.publicBaseURL + "/setup/" + url.PathEscape(r.PathValue("token"))
	payload := map[string]any{
		"account_id": session.AccountID, "setup_session_id": session.SetupSessionID, "state": state,
		"provider_code": "simulator_" + session.Scenario, "hosted_url": hostedURL,
		"provider_customer_ref": session.ProviderCustomerRef, "provider_method_ref": session.ProviderMethodRef,
		"card_brand": "simulator", "last_four": "4242", "expiry_month": 12, "expiry_year": 2099,
	}
	if err := s.sendCallback(r.Context(), payload); err != nil {
		_, _ = s.db.Exec(r.Context(), `UPDATE payment_simulator_setup_sessions SET callback_attempts = callback_attempts + 1, callback_status = 'failed' WHERE setup_session_id = $1`, session.SetupSessionID)
		http.Error(w, "Setup callback failed; retry is safe.", http.StatusBadGateway)
		return
	}
	_, err := s.db.Exec(r.Context(), `
		UPDATE payment_simulator_setup_sessions
		SET state = $2, callback_attempts = callback_attempts + 1, callback_status = 'succeeded', completed_at = $3
		WHERE setup_session_id = $1
	`, session.SetupSessionID, state, s.now())
	if err != nil {
		http.Error(w, "Setup completion could not be recorded.", http.StatusInternalServerError)
		return
	}
	writeCompletionPage(w, state == payment.PaymentIntentStateSucceeded)
}

func (s *Server) charge(w http.ResponseWriter, r *http.Request) {
	var input operationRequest
	if !decodeJSON(w, r, &input) || !validOperation(input, true) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_charge"})
		return
	}
	s.operation(w, r, "charge", input)
}

func (s *Server) refund(w http.ResponseWriter, r *http.Request) {
	var input operationRequest
	if !decodeJSON(w, r, &input) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_refund"})
		return
	}
	if input.MerchantOrderReference == "" {
		input.MerchantOrderReference = "refund_" + input.IntentID
	}
	if !validOperation(input, false) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_refund"})
		return
	}
	s.operation(w, r, "refund", input)
}

func (s *Server) operation(w http.ResponseWriter, r *http.Request, operation string, input operationRequest) {
	if err := s.pruneExpired(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage_failed"})
		return
	}
	input.Scenario = normalizeScenario(input.Scenario)
	state, code := scenarioResult(input.Scenario)
	transactionRef := "sim_txn_" + compactID(input.IntentID) + "_" + operation
	var storedState payment.PaymentIntentState
	var storedRef, storedScenario string
	err := s.db.QueryRow(r.Context(), `
		INSERT INTO payment_simulator_operations (
			run_id, operation, intent_id, idempotency_key, merchant_order_reference,
			provider_transaction_reference, amount_minor, currency, scenario, state
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (run_id, operation, idempotency_key) DO UPDATE SET idempotency_key = EXCLUDED.idempotency_key
		RETURNING state, provider_transaction_reference, scenario
	`, input.RunID, operation, input.IntentID, input.IdempotencyKey, input.MerchantOrderReference, transactionRef,
		input.AmountMinor, input.Currency, input.Scenario, state).Scan(&storedState, &storedRef, &storedScenario)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage_failed"})
		return
	}
	if storedScenario != input.Scenario {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "idempotency_conflict"})
		return
	}
	writeJSON(w, http.StatusOK, response{
		State: storedState, ProviderTransactionReference: storedRef, ProviderCode: code,
		Evidence: map[string]string{"simulator_operation": operation, "simulator_scenario": storedScenario, "simulator_run_id": input.RunID},
	})
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	var input operationRequest
	if !decodeJSON(w, r, &input) || !validRunID(input.RunID) || strings.TrimSpace(input.IntentID) == "" || payment.ValidateChargeAmount(input.Currency, input.AmountMinor) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_query"})
		return
	}
	var state payment.PaymentIntentState
	var transactionRef, scenario string
	err := s.db.QueryRow(r.Context(), `
		SELECT state, provider_transaction_reference, scenario
		FROM payment_simulator_operations WHERE run_id = $1 AND operation = 'charge' AND intent_id = $2
		ORDER BY created_at DESC LIMIT 1
	`, input.RunID, input.IntentID).Scan(&state, &transactionRef, &scenario)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, response{State: payment.PaymentIntentStateUnknown, ProviderCode: "simulator_not_found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage_failed"})
		return
	}
	writeJSON(w, http.StatusOK, response{State: state, ProviderTransactionReference: transactionRef, ProviderCode: "simulator_" + scenario})
}

func (s *Server) sessionByToken(ctx context.Context, token string) (setupSession, bool) {
	if len(token) < 32 || len(token) > 256 {
		return setupSession{}, false
	}
	digest := sha256.Sum256([]byte(token))
	var session setupSession
	err := s.db.QueryRow(ctx, `
		SELECT run_id, account_id::text, setup_session_id::text, scenario, state,
		       provider_customer_reference, provider_method_reference, expires_at
		FROM payment_simulator_setup_sessions WHERE token_sha256 = $1
	`, hex.EncodeToString(digest[:])).Scan(
		&session.RunID, &session.AccountID, &session.SetupSessionID, &session.Scenario, &session.State,
		&session.ProviderCustomerRef, &session.ProviderMethodRef, &session.ExpiresAt,
	)
	return session, err == nil
}

func (s *Server) sendCallback(ctx context.Context, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.callbackURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(signatureHeader, sign(s.callbackSecret, body))
	response, err := s.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("callback returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (s *Server) pruneExpired(ctx context.Context) error {
	cutoff := s.now().Add(-s.retention)
	if _, err := s.db.Exec(ctx, `DELETE FROM payment_simulator_operations WHERE created_at < $1`, cutoff); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `DELETE FROM payment_simulator_setup_sessions WHERE expires_at < $1`, s.now())
	return err
}

func validOperation(input operationRequest, requireMethod bool) bool {
	return validRunID(input.RunID) && strings.TrimSpace(input.IntentID) != "" && strings.TrimSpace(input.IdempotencyKey) != "" &&
		strings.TrimSpace(input.MerchantOrderReference) != "" && payment.ValidateChargeAmount(input.Currency, input.AmountMinor) == nil &&
		(!requireMethod || strings.TrimSpace(input.OpaqueMethodReference) != "") && validScenario(normalizeScenario(input.Scenario))
}

func scenarioResult(scenario string) (payment.PaymentIntentState, string) {
	switch scenario {
	case "declined":
		return payment.PaymentIntentStateFailed, "simulator_declined"
	case "temporary_error", "unknown":
		return payment.PaymentIntentStateUnknown, "simulator_" + scenario
	case "requires_action":
		return payment.PaymentIntentStateRequiresAction, "simulator_requires_action"
	default:
		return payment.PaymentIntentStateSucceeded, "simulator_success"
	}
}

func validScenario(value string) bool {
	switch value {
	case "success", "declined", "temporary_error", "requires_action", "unknown":
		return true
	default:
		return false
	}
}

func normalizeScenario(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "success"
	}
	return value
}

func validBaseURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func setupToken(secret []byte, input setupRequest) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, input.RunID)
	_, _ = io.WriteString(mac, "\x00")
	_, _ = io.WriteString(mac, input.AccountID)
	_, _ = io.WriteString(mac, "\x00")
	_, _ = io.WriteString(mac, input.SetupSessionID)
	_, _ = io.WriteString(mac, "\x00")
	_, _ = io.WriteString(mac, input.IdempotencyKey)
	return hex.EncodeToString(mac.Sum(nil))
}

func validRunID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || index > 0 && (character == '.' || character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}

func compactID(value string) string {
	value = strings.ReplaceAll(value, "-", "")
	if len(value) > 20 {
		value = value[:20]
	}
	return value
}

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func validSignature(secret, body []byte, provided string) bool {
	decoded, err := hex.DecodeString(strings.TrimSpace(provided))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(decoded, mac.Sum(nil))
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return false
	}
	return errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeCompletionPage(w http.ResponseWriter, success bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	message := "TEST PAYMENT - NO REAL CHARGE. Setup remains pending."
	if success {
		message = "TEST PAYMENT - NO REAL CHARGE. Payment method connected. You may close this window."
	}
	_, _ = fmt.Fprintf(w, "<!doctype html><html><body><main><h1>%s</h1></main></body></html>", template.HTMLEscapeString(message))
}

var setupPage = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>RTK Payment Simulator</title><style>body{font:16px system-ui;margin:0;background:#f4f6f8;color:#17202a}main{max-width:34rem;margin:10vh auto;background:white;padding:2rem;border-radius:1rem;box-shadow:0 1rem 3rem #0002}button{font:inherit;background:#0758d8;color:white;border:0;border-radius:.5rem;padding:.8rem 1.2rem}small{color:#566573}</style></head>
<body><main><h1>Payment simulator</h1><p><strong>TEST PAYMENT - NO REAL CHARGE</strong></p><p>This non-production page stores no card data.</p><p><small>Scenario: {{.Scenario}}</small></p><form method="post" action="{{.Action}}"><button type="submit">Connect simulated payment method</button></form></main></body></html>`))
