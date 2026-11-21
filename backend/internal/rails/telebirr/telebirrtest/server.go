// Package telebirrtest is a stand-in for telebirr's merchant API, for automated tests only. It checks
// that our requests are signed correctly, keeps orders in memory, and produces notifications signed
// with its own key. No production code imports it.
package telebirrtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"superapp/backend/internal/rails/telebirr"
)

const (
	FabricAppID   = "fabric-app-test"
	AppSecret     = "secret-test"
	MerchantAppID = "1000000000001"
	MerchantCode  = "245445"
)

type Order struct {
	Amount         string
	Status         string // WAIT_PAY, PAY_SUCCESS, PAY_FAILED
	PaymentOrderID string
	PaidAmount     string
}

type Server struct {
	*httptest.Server
	t           testing.TB
	key         *rsa.PrivateKey // telebirr's key
	merchantPub *rsa.PublicKey

	mu         sync.Mutex
	orders     map[string]*Order
	TokenCalls atomic.Int32
	QueryCalls atomic.Int32
	// RejectToken makes the next authenticated call answer 401, as if the token had been revoked.
	RejectToken atomic.Bool
	// Down makes every call fail with 502.
	Down atomic.Bool
}

// New starts the server. merchantKey is the key our client signs with; the server verifies with it.
func New(t testing.TB, merchantKey *rsa.PrivateKey) *Server {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{t: t, key: key, merchantPub: &merchantKey.PublicKey, orders: map[string]*Order{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /payment/v1/token", s.token)
	mux.HandleFunc("POST /payment/v1/merchant/preOrder", s.signed("payment.preorder", s.preOrder))
	mux.HandleFunc("POST /payment/v1/merchant/queryOrder", s.signed("payment.queryorder", s.queryOrder))
	s.Server = httptest.NewTLSServer(mux)
	t.Cleanup(s.Close)
	return s
}

// Config returns a client config pointing at this server.
func (s *Server) Config(merchantKey *rsa.PrivateKey) telebirr.Config {
	return telebirr.Config{
		BaseURL: s.URL, CheckoutURL: s.URL + "/payment/web/paygate?",
		FabricAppID: FabricAppID, AppSecret: AppSecret, MerchantAppID: MerchantAppID, MerchantCode: MerchantCode,
		PrivateKey: merchantKey, PublicKey: &s.key.PublicKey,
		NotifyURL: "https://api.example.test/v1/webhooks/rails/telebirr",
		HTTP:      s.Client(),
	}
}

// PublicKeyPEM returns telebirr's public key as a PEM block.
func (s *Server) PublicKeyPEM() string {
	der, _ := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	return "-----BEGIN PUBLIC KEY-----\n" + base64.StdEncoding.EncodeToString(der) + "\n-----END PUBLIC KEY-----\n"
}

func (s *Server) Order(id string) *Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.orders[id]; ok {
		cp := *o
		return &cp
	}
	return nil
}

// Pay marks the order paid. paid overrides the amount the customer paid ("" = the order amount).
func (s *Server) Pay(id, paymentOrderID, paid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.orders[id]
	if o == nil {
		s.t.Fatalf("telebirrtest: no order %s", id)
	}
	o.Status, o.PaymentOrderID, o.PaidAmount = "PAY_SUCCESS", paymentOrderID, paid
	if paid == "" {
		o.PaidAmount = o.Amount
	}
}

func (s *Server) Fail(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[id].Status = "PAY_FAILED"
}

// Notification builds a signed payment notification for an order, as telebirr would POST it.
// With transIDQuirk the signature covers "trans_id" while the body says "transId".
func (s *Server) Notification(id string, transIDQuirk bool) []byte {
	s.mu.Lock()
	o := *s.orders[id]
	s.mu.Unlock()
	f := map[string]string{
		"notify_url": "https://api.example.test/v1/webhooks/rails/telebirr", "appid": MerchantAppID,
		"notify_time": strconv.FormatInt(time.Now().UnixMilli(), 10), "merch_code": MerchantCode,
		"merch_order_id": id, "payment_order_id": o.PaymentOrderID, "total_amount": o.PaidAmount,
		"trans_currency": "ETB", "trade_status": "Completed",
		"trans_end_time": strconv.FormatInt(time.Now().UnixMilli(), 10), "trans_id": "T" + o.PaymentOrderID,
	}
	sig, err := telebirr.SignFields(s.key, telebirr.PaddingPSS, f, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	body := map[string]any{"sign": sig, "sign_type": "SHA256WithRSA"}
	for k, v := range f {
		body[k] = v
	}
	if transIDQuirk {
		body["transId"] = body["trans_id"]
		delete(body, "trans_id")
	}
	b, _ := json.Marshal(body)
	return b
}

// ---------------------------------------------------------------------------

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AppSecret string `json:"appSecret"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if s.Down.Load() {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if r.Header.Get("X-APP-Key") != FabricAppID || body.AppSecret != AppSecret {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	n := s.TokenCalls.Add(1)
	writeJSON(w, map[string]any{
		"token":          fmt.Sprintf("Bearer token-%d", n),
		"effectiveDate":  strconv.FormatInt(time.Now().UnixMilli(), 10),
		"expirationDate": strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10),
	})
}

// signed checks headers, token and the request signature before calling the handler.
func (s *Server) signed(method string, h func(biz map[string]string) map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		want := fmt.Sprintf("Bearer token-%d", s.TokenCalls.Load())
		if s.RejectToken.CompareAndSwap(true, false) || r.Header.Get("Authorization") != want || r.Header.Get("X-APP-Key") != FabricAppID {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"errorCode": "401", "errorMsg": "invalid token"})
			return
		}
		var req struct {
			Timestamp string            `json:"timestamp"`
			Nonce     string            `json:"nonce_str"`
			Method    string            `json:"method"`
			Version   string            `json:"version"`
			SignType  string            `json:"sign_type"`
			Sign      string            `json:"sign"`
			Biz       map[string]string `json:"biz_content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != method || req.Version != "1.0" ||
			req.SignType != "SHA256WithRSA" || len(req.Nonce) != 32 {
			writeJSON(w, map[string]any{"code": "400", "msg": "bad request"})
			return
		}
		top := map[string]string{"timestamp": req.Timestamp, "nonce_str": req.Nonce, "method": req.Method, "version": req.Version}
		if err := telebirr.VerifyFields(s.merchantPub, telebirr.PaddingPSS, top, req.Biz, req.Sign); err != nil {
			writeJSON(w, map[string]any{"code": "403", "msg": "signature invalid"})
			return
		}
		if req.Biz["appid"] != MerchantAppID || req.Biz["merch_code"] != MerchantCode {
			writeJSON(w, map[string]any{"code": "403", "msg": "unknown merchant"})
			return
		}
		writeJSON(w, h(req.Biz))
	}
}

func (s *Server) preOrder(biz map[string]string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := biz["merch_order_id"]
	if biz["trans_currency"] != "ETB" || biz["total_amount"] == "" || biz["notify_url"] == "" {
		return map[string]any{"code": "400", "msg": "missing fields"}
	}
	s.orders[id] = &Order{Amount: biz["total_amount"], Status: "WAIT_PAY"}
	return map[string]any{"code": "0", "msg": "success", "result": "SUCCESS",
		"biz_content": map[string]any{"merch_order_id": id, "prepay_id": "prepay-" + id}}
}

func (s *Server) queryOrder(biz map[string]string) map[string]any {
	s.QueryCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.orders[biz["merch_order_id"]]
	if o == nil {
		return map[string]any{"code": "404", "msg": "order not found"}
	}
	out := map[string]any{"merch_order_id": biz["merch_order_id"], "order_status": o.Status, "trans_currency": "ETB",
		"total_amount": o.Amount}
	if o.Status == "PAY_SUCCESS" {
		out["payment_order_id"] = o.PaymentOrderID
		out["total_amount"] = o.PaidAmount
	}
	return map[string]any{"code": "0", "msg": "success", "biz_content": out}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
