package paymentsimulator

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/newebpay"
)

type newebPayTransaction struct {
	ID               string    `json:"id"`
	MerchantOrderNo  string    `json:"merchant_order_no"`
	TradeNo          string    `json:"trade_no"`
	Scenario         string    `json:"scenario"`
	TradeStatus      string    `json:"trade_status"`
	NotifyURL        string    `json:"-"`
	ReturnURL        string    `json:"-"`
	CallbackStatus   string    `json:"callback_status"`
	AmountMinor      int64     `json:"amount_minor"`
	CapturedMinor    int64     `json:"captured_amount_minor"`
	RefundedMinor    int64     `json:"refunded_amount_minor"`
	CallbackAttempts int       `json:"callback_attempts"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (s *Server) newebPayRoutes() {
	s.mux.HandleFunc("POST /MPG/mpg_gateway", s.newebPayMPG)
	s.mux.HandleFunc("POST /API/QueryTradeInfo", s.newebPayQuery)
	s.mux.HandleFunc("POST /API/CreditCard/Cancel", s.newebPayCancel)
	s.mux.HandleFunc("POST /API/CreditCard/Close", s.newebPayClose)
	s.mux.HandleFunc("GET /newebpay/pay/{token}", s.newebPayHostedPage)
	s.mux.HandleFunc("POST /newebpay/pay/{token}", s.newebPayComplete)
	s.mux.HandleFunc("GET /admin/newebpay", s.newebPayAdminPage)
	s.mux.Handle("GET /internal/admin/newebpay/transactions", s.newebPayAdmin(http.HandlerFunc(s.newebPayListTransactions)))
	s.mux.Handle("PUT /internal/admin/newebpay/transactions/{id}/scenario", s.newebPayAdmin(http.HandlerFunc(s.newebPaySetScenario)))
	s.mux.Handle("POST /internal/admin/newebpay/transactions/{id}/callback", s.newebPayAdmin(http.HandlerFunc(s.newebPayRetryCallback)))
}

func (s *Server) newebPayAdminPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, newebPayAdminHTML)
}

func (s *Server) newebPayMPG(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.Form.Get("MerchantID") != s.newebPayMerchantID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"Status": "MPG01001", "Message": "invalid merchant"})
		return
	}
	tradeInfo := strings.TrimSpace(r.Form.Get("TradeInfo"))
	if tradeInfo == "" || !newebpay.VerifyDigest(newebpay.TradeSHA(tradeInfo, s.newebPayHashKey, s.newebPayHashIV), r.Form.Get("TradeSha")) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"Status": "MPG01002", "Message": "invalid trade sha"})
		return
	}
	plain, err := newebpay.DecryptTradeInfo(tradeInfo, s.newebPayHashKey, s.newebPayHashIV)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"Status": "MPG01002", "Message": "invalid trade info"})
		return
	}
	fields, err := url.ParseQuery(plain)
	amount, amountErr := strconv.ParseInt(fields.Get("Amt"), 10, 64)
	timestamp, timestampErr := strconv.ParseInt(fields.Get("TimeStamp"), 10, 64)
	returnURL, returnErr := validBaseURL(fields.Get("ReturnURL"))
	order := fields.Get("MerchantOrderNo")
	if err != nil || amountErr != nil || timestampErr != nil || amount <= 0 || len(order) == 0 || len(order) > 30 ||
		fields.Get("MerchantID") != s.newebPayMerchantID || fields.Get("Version") != "2.3" || absDuration(s.now().Sub(time.Unix(timestamp, 0))) > 120*time.Second ||
		strings.TrimSpace(fields.Get("NotifyURL")) != s.newebPayNotifyURL || returnErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"Status": "MPG01008", "Message": "invalid order"})
		return
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		http.Error(w, "simulator unavailable", 500)
		return
	}
	token := hex.EncodeToString(tokenBytes)
	digest := sha256.Sum256([]byte(token))
	tradeNo := fmt.Sprintf("SIM%017d", s.now().UnixNano()%1e17)
	var id string
	err = s.db.QueryRow(r.Context(), `INSERT INTO payment_simulator_newebpay_transactions
		(run_id, merchant_id, merchant_order_no, trade_no, amount_minor, public_token_sha256, notify_url, return_url)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (run_id, merchant_id, merchant_order_no) DO UPDATE SET
			public_token_sha256=EXCLUDED.public_token_sha256, notify_url=EXCLUDED.notify_url, return_url=EXCLUDED.return_url
		RETURNING id::text`, s.runID, s.newebPayMerchantID, order, tradeNo, amount, hex.EncodeToString(digest[:]), s.newebPayNotifyURL, returnURL).Scan(&id)
	if err != nil {
		http.Error(w, "simulator storage failed", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = newebPayRedirectPage.Execute(w, map[string]string{"URL": s.publicBaseURL + "/newebpay/pay/" + token})
}

func (s *Server) newebPayHostedPage(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.newebPayByToken(r, r.PathValue("token"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = newebPayPaymentPage.Execute(w, map[string]any{"Action": "/newebpay/pay/" + url.PathEscape(r.PathValue("token")), "Order": tx.MerchantOrderNo, "Amount": tx.AmountMinor, "Scenario": tx.Scenario})
}

func (s *Server) newebPayComplete(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.newebPayByToken(r, r.PathValue("token"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	scenario := normalizeScenario(r.Form.Get("scenario"))
	if !validScenario(scenario) {
		http.Error(w, "invalid scenario", 400)
		return
	}
	status := map[string]string{"success": "1", "declined": "2", "requires_action": "0", "temporary_error": "0", "unknown": "0"}[scenario]
	_, err := s.db.Exec(r.Context(), `UPDATE payment_simulator_newebpay_transactions SET scenario=$2, trade_status=$3, completed_at=$4 WHERE id=$1`, tx.ID, scenario, status, s.now())
	if err != nil {
		http.Error(w, "storage failed", 500)
		return
	}
	tx.Scenario, tx.TradeStatus = scenario, status
	if scenario == "temporary_error" {
		http.Error(w, "simulated temporary error", 503)
		return
	}
	if scenario != "requires_action" {
		_ = s.sendNewebPayCallback(r, tx)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = newebPayResultPage.Execute(w, map[string]string{"Scenario": scenario, "ReturnURL": tx.ReturnURL})
}

func (s *Server) newebPayQuery(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.Form.Get("MerchantID") != s.newebPayMerchantID {
		writeJSON(w, 400, map[string]string{"Status": "MPG01001"})
		return
	}
	amount, err := strconv.ParseInt(r.Form.Get("Amt"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"Status": "MPG01008"})
		return
	}
	expected := newebpay.QueryCheckValue(amount, s.newebPayMerchantID, r.Form.Get("MerchantOrderNo"), s.newebPayHashKey, s.newebPayHashIV)
	if !newebpay.VerifyDigest(expected, r.Form.Get("CheckValue")) {
		writeJSON(w, 400, map[string]string{"Status": "MPG01002"})
		return
	}
	var tx newebPayTransaction
	err = s.db.QueryRow(r.Context(), `SELECT merchant_order_no,trade_no,amount_minor,trade_status,captured_amount_minor,refunded_amount_minor FROM payment_simulator_newebpay_transactions WHERE run_id=$1 AND merchant_id=$2 AND merchant_order_no=$3`, s.runID, s.newebPayMerchantID, r.Form.Get("MerchantOrderNo")).Scan(&tx.MerchantOrderNo, &tx.TradeNo, &tx.AmountMinor, &tx.TradeStatus, &tx.CapturedMinor, &tx.RefundedMinor)
	if err != nil {
		writeJSON(w, 200, map[string]string{"Status": "TRA10002", "Message": "not found"})
		return
	}
	check := newebpay.ResponseCheckCode(map[string]string{"Amt": strconv.FormatInt(tx.AmountMinor, 10), "MerchantID": s.newebPayMerchantID, "MerchantOrderNo": tx.MerchantOrderNo, "TradeNo": tx.TradeNo}, s.newebPayHashKey, s.newebPayHashIV)
	closeStatus := "0"
	if tx.CapturedMinor > 0 {
		closeStatus = "3"
	}
	backStatus := "0"
	if tx.RefundedMinor > 0 {
		backStatus = "3"
	}
	writeJSON(w, 200, map[string]any{"Status": "SUCCESS", "Message": "success", "Result": map[string]any{"MerchantID": s.newebPayMerchantID, "Amt": tx.AmountMinor, "TradeNo": tx.TradeNo, "MerchantOrderNo": tx.MerchantOrderNo, "TradeStatus": tx.TradeStatus, "CloseAmt": tx.CapturedMinor, "CloseStatus": closeStatus, "BackBalance": tx.CapturedMinor - tx.RefundedMinor, "BackStatus": backStatus, "CheckCode": check}})
}

func (s *Server) newebPayCancel(w http.ResponseWriter, r *http.Request) {
	fields, tx, ok := s.newebPayPostData(w, r, "1.0")
	if !ok {
		return
	}
	amount, _ := strconv.ParseInt(fields.Get("Amt"), 10, 64)
	if amount != tx.AmountMinor || tx.TradeStatus != "1" || tx.CapturedMinor != 0 {
		s.newebPayOperationResponse(w, fields.Get("RespondType"), "TRA10026", "transaction cannot be canceled", tx)
		return
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE payment_simulator_newebpay_transactions SET trade_status='3' WHERE id=$1`, tx.ID); err != nil {
		http.Error(w, "storage failed", http.StatusInternalServerError)
		return
	}
	tx.TradeStatus = "3"
	s.newebPayOperationResponse(w, fields.Get("RespondType"), "SUCCESS", "authorization canceled", tx)
}

func (s *Server) newebPayClose(w http.ResponseWriter, r *http.Request) {
	fields, tx, ok := s.newebPayPostData(w, r, "1.1")
	if !ok {
		return
	}
	amount, amountErr := strconv.ParseInt(fields.Get("Amt"), 10, 64)
	closeType, cancel := fields.Get("CloseType"), fields.Get("Cancel") == "1"
	if amountErr != nil || amount <= 0 || (closeType != "1" && closeType != "2") || (fields.Get("Cancel") != "" && !cancel) {
		s.newebPayOperationResponse(w, fields.Get("RespondType"), "MEM40013", "invalid close request", tx)
		return
	}
	status, message := "SUCCESS", "close operation completed"
	switch {
	case closeType == "1" && !cancel && tx.TradeStatus == "1" && tx.CapturedMinor == 0 && amount <= tx.AmountMinor:
		tx.CapturedMinor = amount
	case closeType == "1" && cancel && tx.CapturedMinor == amount && tx.RefundedMinor == 0:
		tx.CapturedMinor = 0
	case closeType == "2" && !cancel && tx.CapturedMinor-tx.RefundedMinor >= amount:
		tx.RefundedMinor += amount
	case closeType == "2" && cancel && tx.RefundedMinor >= amount:
		tx.RefundedMinor -= amount
	default:
		status, message = "TRA10035", "transaction state does not allow this operation"
	}
	if status == "SUCCESS" {
		if _, err := s.db.Exec(r.Context(), `UPDATE payment_simulator_newebpay_transactions SET captured_amount_minor=$2,refunded_amount_minor=$3 WHERE id=$1`, tx.ID, tx.CapturedMinor, tx.RefundedMinor); err != nil {
			http.Error(w, "storage failed", http.StatusInternalServerError)
			return
		}
	}
	s.newebPayOperationResponse(w, fields.Get("RespondType"), status, message, tx)
}

func (s *Server) newebPayPostData(w http.ResponseWriter, r *http.Request, version string) (url.Values, newebPayTransaction, bool) {
	if err := r.ParseForm(); err != nil || r.Form.Get("MerchantID_") != s.newebPayMerchantID {
		s.newebPayOperationResponse(w, "JSON", "TRA10009", "invalid merchant", newebPayTransaction{})
		return nil, newebPayTransaction{}, false
	}
	plain, err := newebpay.DecryptTradeInfo(r.Form.Get("PostData_"), s.newebPayHashKey, s.newebPayHashIV)
	if err != nil {
		s.newebPayOperationResponse(w, "JSON", "TRA10008", "invalid encrypted request", newebPayTransaction{})
		return nil, newebPayTransaction{}, false
	}
	fields, err := url.ParseQuery(plain)
	timestamp, timestampErr := strconv.ParseInt(fields.Get("TimeStamp"), 10, 64)
	respondType := fields.Get("RespondType")
	if err != nil || timestampErr != nil || fields.Get("Version") != version || (respondType != "JSON" && respondType != "String") || absDuration(s.now().Sub(time.Unix(timestamp, 0))) > 120*time.Second {
		s.newebPayOperationResponse(w, fields.Get("RespondType"), "MEM40014", "invalid request timestamp or version", newebPayTransaction{})
		return nil, newebPayTransaction{}, false
	}
	indexType := fields.Get("IndexType")
	var tx newebPayTransaction
	query := `SELECT id::text,merchant_order_no,trade_no,amount_minor,trade_status,captured_amount_minor,refunded_amount_minor FROM payment_simulator_newebpay_transactions WHERE run_id=$1 AND merchant_id=$2 AND merchant_order_no=$3`
	lookup := fields.Get("MerchantOrderNo")
	if indexType == "2" {
		query = `SELECT id::text,merchant_order_no,trade_no,amount_minor,trade_status,captured_amount_minor,refunded_amount_minor FROM payment_simulator_newebpay_transactions WHERE run_id=$1 AND merchant_id=$2 AND trade_no=$3`
		lookup = fields.Get("TradeNo")
	} else if indexType != "1" {
		s.newebPayOperationResponse(w, fields.Get("RespondType"), "TRA10032", "invalid index type", newebPayTransaction{})
		return nil, newebPayTransaction{}, false
	}
	err = s.db.QueryRow(r.Context(), query, s.runID, s.newebPayMerchantID, lookup).Scan(&tx.ID, &tx.MerchantOrderNo, &tx.TradeNo, &tx.AmountMinor, &tx.TradeStatus, &tx.CapturedMinor, &tx.RefundedMinor)
	if err != nil {
		s.newebPayOperationResponse(w, fields.Get("RespondType"), "TRA10021", "transaction not found", newebPayTransaction{})
		return nil, newebPayTransaction{}, false
	}
	return fields, tx, true
}

func (s *Server) newebPayOperationResponse(w http.ResponseWriter, respondType, status, message string, tx newebPayTransaction) {
	checkCode := ""
	if tx.MerchantOrderNo != "" && tx.TradeNo != "" && tx.AmountMinor > 0 {
		checkCode = newebpay.ResponseCheckCode(map[string]string{"Amt": strconv.FormatInt(tx.AmountMinor, 10), "MerchantID": s.newebPayMerchantID, "MerchantOrderNo": tx.MerchantOrderNo, "TradeNo": tx.TradeNo}, s.newebPayHashKey, s.newebPayHashIV)
	}
	result := map[string]any{"MerchantID": s.newebPayMerchantID, "Amt": tx.AmountMinor, "TradeNo": tx.TradeNo, "MerchantOrderNo": tx.MerchantOrderNo, "CheckCode": checkCode}
	if strings.EqualFold(respondType, "String") {
		values := url.Values{"Status": {status}, "Message": {message}, "MerchantID": {s.newebPayMerchantID}, "Amt": {strconv.FormatInt(tx.AmountMinor, 10)}, "TradeNo": {tx.TradeNo}, "MerchantOrderNo": {tx.MerchantOrderNo}, "CheckCode": {checkCode}}
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		_, _ = io.WriteString(w, values.Encode())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"Status": status, "Message": message, "Result": result})
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func (s *Server) sendNewebPayCallback(r *http.Request, tx newebPayTransaction) error {
	if strings.TrimSpace(tx.NotifyURL) == "" {
		return nil
	}
	status := "SUCCESS"
	if tx.TradeStatus != "1" {
		status = "MPG03009"
	}
	inner := url.Values{"Status": {status}, "MerchantID": {s.newebPayMerchantID}, "Amt": {strconv.FormatInt(tx.AmountMinor, 10)}, "TradeNo": {tx.TradeNo}, "MerchantOrderNo": {tx.MerchantOrderNo}, "PaymentType": {"CREDIT"}, "RespondCode": {status}}
	encrypted, err := newebpay.EncryptTradeInfo(inner.Encode(), s.newebPayHashKey, s.newebPayHashIV)
	if err != nil {
		return err
	}
	body := url.Values{"Status": {status}, "MerchantID": {s.newebPayMerchantID}, "Version": {"2.3"}, "TradeInfo": {encrypted}, "TradeSha": {newebpay.TradeSHA(encrypted, s.newebPayHashKey, s.newebPayHashIV)}}.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, tx.NotifyURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.http.Do(req)
	callbackStatus := "failed"
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			callbackStatus = "succeeded"
		}
	}
	_, _ = s.db.Exec(r.Context(), `UPDATE payment_simulator_newebpay_transactions SET callback_attempts=callback_attempts+1,callback_status=$2 WHERE id=$1`, tx.ID, callbackStatus)
	if err != nil {
		return err
	}
	if callbackStatus != "succeeded" {
		return fmt.Errorf("callback HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *Server) newebPayByToken(r *http.Request, token string) (newebPayTransaction, bool) {
	if len(token) != 64 {
		return newebPayTransaction{}, false
	}
	digest := sha256.Sum256([]byte(token))
	var tx newebPayTransaction
	err := s.db.QueryRow(r.Context(), `SELECT id::text,merchant_order_no,trade_no,amount_minor,scenario,trade_status,captured_amount_minor,refunded_amount_minor,notify_url,return_url,callback_status,callback_attempts,created_at,updated_at FROM payment_simulator_newebpay_transactions WHERE public_token_sha256=$1`, hex.EncodeToString(digest[:])).Scan(&tx.ID, &tx.MerchantOrderNo, &tx.TradeNo, &tx.AmountMinor, &tx.Scenario, &tx.TradeStatus, &tx.CapturedMinor, &tx.RefundedMinor, &tx.NotifyURL, &tx.ReturnURL, &tx.CallbackStatus, &tx.CallbackAttempts, &tx.CreatedAt, &tx.UpdatedAt)
	return tx, err == nil
}

func (s *Server) newebPayAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(got) != len(s.adminToken) || subtle.ConstantTimeCompare([]byte(got), []byte(s.adminToken)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) newebPayListTransactions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT id::text,merchant_order_no,trade_no,amount_minor,scenario,trade_status,captured_amount_minor,refunded_amount_minor,callback_status,callback_attempts,created_at,updated_at FROM payment_simulator_newebpay_transactions WHERE run_id=$1 ORDER BY created_at DESC LIMIT 100`, s.runID)
	if err != nil {
		http.Error(w, "storage failed", 500)
		return
	}
	defer rows.Close()
	out := []newebPayTransaction{}
	for rows.Next() {
		var tx newebPayTransaction
		if rows.Scan(&tx.ID, &tx.MerchantOrderNo, &tx.TradeNo, &tx.AmountMinor, &tx.Scenario, &tx.TradeStatus, &tx.CapturedMinor, &tx.RefundedMinor, &tx.CallbackStatus, &tx.CallbackAttempts, &tx.CreatedAt, &tx.UpdatedAt) == nil {
			out = append(out, tx)
		}
	}
	writeJSON(w, 200, map[string]any{"transactions": out})
}
func (s *Server) newebPaySetScenario(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scenario string `json:"scenario"`
	}
	if !decodeJSON(w, r, &in) || !validScenario(normalizeScenario(in.Scenario)) {
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE payment_simulator_newebpay_transactions SET scenario=$2 WHERE id=$1`, r.PathValue("id"), normalizeScenario(in.Scenario))
	if err != nil || tag.RowsAffected() != 1 {
		http.Error(w, "not found", 404)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "updated"})
}
func (s *Server) newebPayRetryCallback(w http.ResponseWriter, r *http.Request) {
	var tx newebPayTransaction
	err := s.db.QueryRow(r.Context(), `SELECT id::text,merchant_order_no,trade_no,amount_minor,scenario,trade_status,notify_url FROM payment_simulator_newebpay_transactions WHERE id=$1`, r.PathValue("id")).Scan(&tx.ID, &tx.MerchantOrderNo, &tx.TradeNo, &tx.AmountMinor, &tx.Scenario, &tx.TradeStatus, &tx.NotifyURL)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	if err = s.sendNewebPayCallback(r, tx); err != nil {
		http.Error(w, "callback failed", 502)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "sent"})
}

var newebPayRedirectPage = template.Must(template.New("redirect").Parse(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url={{.URL}}"><title>NewebPay Simulator</title><a href="{{.URL}}">Continue to test payment</a>`))
var newebPayPaymentPage = template.Must(template.New("payment").Parse(`<!doctype html><html lang="zh-TW"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>NewebPay Simulator</title><style>body{font:16px system-ui;max-width:560px;margin:5rem auto;padding:2rem;background:#f5f7fb}main{background:white;padding:2rem;border-radius:16px}button{margin:.4rem;padding:.75rem 1rem}.warn{color:#b42318;font-weight:700}</style><main><p class="warn">TEST PAYMENT — NO REAL CHARGE</p><h1>NewebPay Simulator</h1><p>Order {{.Order}} · NT${{.Amount}}</p><form method="post" action="{{.Action}}"><button name="scenario" value="success">模擬成功</button><button name="scenario" value="declined">模擬拒絕</button><button name="scenario" value="requires_action">需要操作</button><button name="scenario" value="unknown">結果不明</button><button name="scenario" value="temporary_error">暫時錯誤</button></form></main></html>`))
var newebPayResultPage = template.Must(template.New("result").Parse(`<!doctype html><meta charset="utf-8"><title>Test payment result</title><h1>NewebPay Simulator</h1><p>TEST ONLY — result: {{.Scenario}}</p><p><a href="{{.ReturnURL}}">Return to RTK Cloud billing</a></p>`))

const newebPayAdminHTML = `<!doctype html><html lang="zh-TW"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>NewebPay Simulator Admin</title><style>body{font:14px system-ui;margin:2rem;background:#f5f7fb;color:#172033}header,section{background:white;padding:1.25rem;margin-bottom:1rem;border-radius:12px}input,button,select{padding:.55rem;margin:.2rem}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:.6rem;border-bottom:1px solid #ddd}.warn{color:#b42318;font-weight:700}</style><header><p class="warn">NON-PRODUCTION TEST SYSTEM</p><h1>NewebPay Simulator Admin</h1><label>Admin token <input id="token" type="password" autocomplete="off"></label><button id="load">Load transactions</button><span id="status" role="status"></span></header><section><table><thead><tr><th>Order</th><th>Trade</th><th>Amount</th><th>State</th><th>Captured / refunded</th><th>Callback</th><th>Scenario / actions</th></tr></thead><tbody id="rows"></tbody></table></section><script>
const token=()=>document.querySelector('#token').value;const status=document.querySelector('#status');
async function api(path,options={}){const response=await fetch(path,{...options,headers:{Authorization:'Bearer '+token(),'Content-Type':'application/json',...(options.headers||{})}});if(!response.ok)throw new Error('HTTP '+response.status);return response.json()}
async function load(){try{status.textContent='Loading…';const data=await api('/internal/admin/newebpay/transactions');document.querySelector('#rows').replaceChildren(...data.transactions.map(row=>{const tr=document.createElement('tr');tr.innerHTML='<td></td><td></td><td></td><td></td><td></td><td></td><td><select><option>success</option><option>declined</option><option>requires_action</option><option>unknown</option><option>temporary_error</option></select><button>Set</button><button>Retry callback</button></td>';const cells=tr.children;cells[0].textContent=row.merchant_order_no;cells[1].textContent=row.trade_no;cells[2].textContent='NT$'+row.amount_minor;cells[3].textContent=row.trade_status;cells[4].textContent='NT$'+row.captured_amount_minor+' / NT$'+row.refunded_amount_minor;cells[5].textContent=row.callback_status+' ('+row.callback_attempts+')';const select=cells[6].querySelector('select');select.value=row.scenario;const buttons=cells[6].querySelectorAll('button');buttons[0].onclick=async()=>{await api('/internal/admin/newebpay/transactions/'+encodeURIComponent(row.id)+'/scenario',{method:'PUT',body:JSON.stringify({scenario:select.value})});load()};buttons[1].onclick=async()=>{await api('/internal/admin/newebpay/transactions/'+encodeURIComponent(row.id)+'/callback',{method:'POST'});load()};return tr}));status.textContent=data.transactions.length+' transactions'}catch(error){status.textContent=error.message}}
document.querySelector('#load').onclick=load;
</script></html>`
