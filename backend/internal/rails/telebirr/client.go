package telebirr

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"superapp/backend/internal/money"
	"superapp/backend/internal/payments"
)

type Config struct {
	BaseURL       string // API gateway, e.g. https://developerportal.ethiotelebirr.et:38443/apiaccess/payment/gateway
	CheckoutURL   string // web checkout ("paygate") URL; query parameters are appended
	FabricAppID   string // X-APP-Key
	AppSecret     string
	MerchantAppID string // biz_content.appid
	MerchantCode  string // short code, biz_content.merch_code
	PrivateKey    *rsa.PrivateKey
	PublicKey     *rsa.PublicKey // telebirr's key, for notifications
	NotifyURL     string         // public HTTPS URL of POST /v1/webhooks/rails/telebirr
	RedirectURL   string         // where checkout returns the customer (optional)

	// Settings the public sources disagree on.
	Padding      Padding // default PSS
	TradeType    string  // default "Checkout"
	PayeeType    string  // default "5000"
	BusinessType string  // sent only if set
	OrderTimeout string  // default "120m"

	HTTP *http.Client
}

func (c *Config) defaults() {
	if c.Padding == "" {
		c.Padding = PaddingPSS
	}
	if c.TradeType == "" {
		c.TradeType = "Checkout"
	}
	if c.PayeeType == "" {
		c.PayeeType = "5000"
	}
	if c.OrderTimeout == "" {
		c.OrderTimeout = "120m"
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
}

func (c *Config) validate() error {
	var missing []string
	for name, v := range map[string]string{
		"base URL": c.BaseURL, "checkout URL": c.CheckoutURL, "fabric app id": c.FabricAppID,
		"app secret": c.AppSecret, "merchant app id": c.MerchantAppID, "merchant code": c.MerchantCode,
		"notify URL": c.NotifyURL,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if c.PrivateKey == nil {
		missing = append(missing, "merchant private key")
	}
	if c.PublicKey == nil {
		missing = append(missing, "telebirr public key")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("telebirr config missing: %s", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(c.BaseURL, "https://") || !strings.HasPrefix(c.NotifyURL, "https://") {
		return errors.New("telebirr base and notify URLs must be https")
	}
	if c.Padding != PaddingPSS && c.Padding != PaddingPKCS1 {
		return fmt.Errorf("telebirr padding must be %q or %q", PaddingPSS, PaddingPKCS1)
	}
	return nil
}

// Client implements payments.TopUpRail for telebirr.
type Client struct {
	cfg Config
	Now func() time.Time

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

var _ payments.TopUpRail = (*Client)(nil)

func New(cfg Config) (*Client, error) {
	cfg.defaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg}, nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// ---------------------------------------------------------------------------
// fabric token
// ---------------------------------------------------------------------------

func (c *Client) fabricToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.tokenExp) {
		return c.token, nil
	}
	body, _ := json.Marshal(map[string]string{"appSecret": c.cfg.AppSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/payment/v1/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-APP-Key", c.cfg.FabricAppID)
	var out struct {
		Token          string          `json:"token"`
		ExpirationDate json.RawMessage `json:"expirationDate"`
	}
	if err := c.send(req, &out); err != nil {
		return "", fmt.Errorf("telebirr token: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("telebirr token: empty token")
	}
	c.token = out.Token
	c.tokenExp = c.expiry(out.ExpirationDate).Add(-5 * time.Minute)
	return c.token, nil
}

// expiry reads expirationDate as epoch seconds or milliseconds (number or string). Anything else
// falls back to 50 minutes; tokens are documented as lasting an hour.
func (c *Client) expiry(raw json.RawMessage) time.Time {
	s := strings.Trim(string(raw), `"`)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	return c.now().Add(50 * time.Minute)
}

func (c *Client) dropToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// signed API calls
// ---------------------------------------------------------------------------

const nonceChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func nonce() (string, error) {
	b := make([]byte, 32)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(nonceChars))))
		if err != nil {
			return "", err
		}
		b[i] = nonceChars[n.Int64()]
	}
	return string(b), nil
}

type apiError struct {
	Status int
	Code   string
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telebirr: HTTP %d code=%s msg=%s", e.Status, e.Code, e.Msg)
}

// call sends a signed request and returns the response's biz_content as strings.
func (c *Client) call(ctx context.Context, path, method string, biz map[string]string) (map[string]string, error) {
	for attempt := 0; ; attempt++ {
		out, err := c.callOnce(ctx, path, method, biz)
		var ae *apiError
		if attempt == 0 && errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			c.dropToken() // the cached token was revoked or expired early
			continue
		}
		return out, err
	}
}

func (c *Client) callOnce(ctx context.Context, path, method string, biz map[string]string) (map[string]string, error) {
	token, err := c.fabricToken(ctx)
	if err != nil {
		return nil, err
	}
	n, err := nonce()
	if err != nil {
		return nil, err
	}
	top := map[string]string{
		"timestamp": strconv.FormatInt(c.now().Unix(), 10),
		"nonce_str": n,
		"method":    method,
		"version":   "1.0",
	}
	sig, err := sign(c.cfg.PrivateKey, c.cfg.Padding, canonical(top, biz))
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"sign_type": signType, "sign": sig, "biz_content": biz}
	for k, v := range top {
		payload[k] = v
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-APP-Key", c.cfg.FabricAppID)
	req.Header.Set("Authorization", token)

	var out struct {
		Code       json.RawMessage `json:"code"`
		Msg        string          `json:"msg"`
		ErrorCode  string          `json:"errorCode"`
		ErrorMsg   string          `json:"errorMsg"`
		BizContent map[string]any  `json:"biz_content"`
	}
	if err := c.send(req, &out); err != nil {
		return nil, err
	}
	if out.BizContent == nil {
		code := strings.Trim(string(out.Code), `"`)
		if out.ErrorCode != "" {
			code = out.ErrorCode
		}
		return nil, &apiError{Status: http.StatusOK, Code: code, Msg: out.Msg + out.ErrorMsg}
	}
	return stringify(out.BizContent), nil
}

func (c *Client) send(req *http.Request, out any) error {
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			ErrorCode string `json:"errorCode"`
			ErrorMsg  string `json:"errorMsg"`
		}
		_ = json.Unmarshal(raw, &e)
		return &apiError{Status: resp.StatusCode, Code: e.ErrorCode, Msg: e.ErrorMsg}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(out)
}

// stringify turns scalar JSON values into the strings telebirr signs. Nested objects are dropped.
func stringify(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case string:
			out[k] = x
		case json.Number:
			out[k] = x.String()
		case bool:
			out[k] = strconv.FormatBool(x)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// payments.TopUpRail
// ---------------------------------------------------------------------------

func (c *Client) CreateCheckout(ctx context.Context, orderID string, amt money.Money, title string) (payments.RailCheckout, error) {
	if amt.Currency != money.ETB {
		return payments.RailCheckout{}, errors.New("telebirr collects ETB only")
	}
	biz := map[string]string{
		"notify_url":            c.cfg.NotifyURL,
		"appid":                 c.cfg.MerchantAppID,
		"merch_code":            c.cfg.MerchantCode,
		"merch_order_id":        orderID,
		"trade_type":            c.cfg.TradeType,
		"title":                 title,
		"total_amount":          amt.Decimal(),
		"trans_currency":        "ETB",
		"timeout_express":       c.cfg.OrderTimeout,
		"payee_identifier":      c.cfg.MerchantCode,
		"payee_identifier_type": "04",
		"payee_type":            c.cfg.PayeeType,
	}
	if c.cfg.RedirectURL != "" {
		biz["redirect_url"] = c.cfg.RedirectURL
	}
	if c.cfg.BusinessType != "" {
		biz["business_type"] = c.cfg.BusinessType
	}
	resp, err := c.call(ctx, "/payment/v1/merchant/preOrder", "payment.preorder", biz)
	if err != nil {
		return payments.RailCheckout{}, err
	}
	prepay := resp["prepay_id"]
	if prepay == "" {
		return payments.RailCheckout{}, errors.New("telebirr preOrder: no prepay_id in response")
	}
	link, err := c.checkoutURL(prepay)
	if err != nil {
		return payments.RailCheckout{}, err
	}
	return payments.RailCheckout{OrderRef: prepay, CheckoutURL: link}, nil
}

// checkoutURL signs appid, merch_code, nonce_str, prepay_id and timestamp and appends them to the
// paygate URL. Values are URL-encoded: a raw base64 "+" in the signature would arrive as a space.
func (c *Client) checkoutURL(prepayID string) (string, error) {
	n, err := nonce()
	if err != nil {
		return "", err
	}
	p := map[string]string{
		"appid": c.cfg.MerchantAppID, "merch_code": c.cfg.MerchantCode, "nonce_str": n,
		"prepay_id": prepayID, "timestamp": strconv.FormatInt(c.now().Unix(), 10),
	}
	sig, err := sign(c.cfg.PrivateKey, c.cfg.Padding, canonical(p, nil))
	if err != nil {
		return "", err
	}
	q := url.QueryEscape
	return c.cfg.CheckoutURL + "appid=" + q(p["appid"]) + "&merch_code=" + q(p["merch_code"]) +
		"&nonce_str=" + n + "&prepay_id=" + q(prepayID) + "&timestamp=" + p["timestamp"] +
		"&sign=" + q(sig) + "&sign_type=" + signType + "&version=1.0&trade_type=" + q(c.cfg.TradeType), nil
}

// QueryOrder asks telebirr for the order's state. This, not the notification body, decides whether
// a wallet is credited.
func (c *Client) QueryOrder(ctx context.Context, orderID string) (payments.RailOrder, error) {
	resp, err := c.call(ctx, "/payment/v1/merchant/queryOrder", "payment.queryorder", map[string]string{
		"appid": c.cfg.MerchantAppID, "merch_code": c.cfg.MerchantCode, "merch_order_id": orderID,
	})
	if err != nil {
		return payments.RailOrder{}, err
	}
	if id := resp["merch_order_id"]; id != "" && id != orderID {
		return payments.RailOrder{}, fmt.Errorf("telebirr queryOrder: asked for %s, got %s", orderID, id)
	}
	ord := payments.RailOrder{Status: resp["order_status"], State: orderState(resp["order_status"], resp["trade_status"])}
	if ord.State != payments.RailSucceeded {
		return ord, nil
	}
	ord.PartnerRef = resp["payment_order_id"]
	if ord.PartnerRef == "" {
		ord.PartnerRef = resp["trans_id"]
	}
	if ord.PartnerRef == "" {
		return payments.RailOrder{}, errors.New("telebirr queryOrder: paid order without a payment reference")
	}
	cur := money.Currency(resp["trans_currency"])
	if cur == "" {
		cur = money.ETB
	}
	ord.Amount, err = money.ParseDecimal(resp["total_amount"], cur)
	if err != nil {
		return payments.RailOrder{}, fmt.Errorf("telebirr queryOrder: total_amount %q: %w", resp["total_amount"], err)
	}
	return ord, nil
}

// orderState maps telebirr's statuses. Only explicit success credits a wallet and only explicit
// failure fails a top-up; anything unrecognised stays pending and is asked about again later.
func orderState(orderStatus, tradeStatus string) payments.RailState {
	switch {
	case orderStatus == "PAY_SUCCESS" || (orderStatus == "" && (tradeStatus == "Completed" || tradeStatus == "PAY_SUCCESS")):
		return payments.RailSucceeded
	case orderStatus == "PAY_FAILED" || orderStatus == "PAY_CANCEL" || orderStatus == "ORDER_CLOSED":
		return payments.RailFailed
	}
	return payments.RailPending
}

// ---------------------------------------------------------------------------
// notifications
// ---------------------------------------------------------------------------

var ErrBadNotification = errors.New("telebirr notification rejected")

// VerifyNotification checks the signature on a payment notification with telebirr's public key and
// that it is addressed to this merchant, and returns the merch_order_id. The caller must still ask
// QueryOrder before crediting: the notification only says "look at this order now".
func (c *Client) VerifyNotification(body []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return "", ErrBadNotification
	}
	if inner, ok := raw["data"].(map[string]any); ok { // some deliveries wrap the fields in "data"
		raw = inner
	}
	f := stringify(raw)
	sig := f["sign"]
	if sig == "" {
		return "", ErrBadNotification
	}
	err := verify(c.cfg.PublicKey, c.cfg.Padding, canonical(f, nil), sig)
	if err != nil && f["transId"] != "" {
		// Reported quirk: telebirr signs "trans_id" but sends "transId".
		alt := make(map[string]string, len(f))
		for k, v := range f {
			alt[k] = v
		}
		alt["trans_id"] = alt["transId"]
		delete(alt, "transId")
		err = verify(c.cfg.PublicKey, c.cfg.Padding, canonical(alt, nil), sig)
	}
	if err != nil {
		return "", ErrBadNotification
	}
	if f["appid"] != c.cfg.MerchantAppID || f["merch_code"] != c.cfg.MerchantCode || f["merch_order_id"] == "" {
		return "", ErrBadNotification
	}
	return f["merch_order_id"], nil
}
