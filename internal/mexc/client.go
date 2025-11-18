package mexc

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config describes how to talk to the unofficial MEXC web API.
type Config struct {
	AuthToken string
	BaseURL   string
	Timeout   time.Duration
}

// Client implements the minimal subset of the web API required by the bot.
type Client struct {
	cfg        Config
	httpClient *http.Client
}

// Side constants derived from the official web client.
const (
	SideOpenLong   = 1
	SideCloseShort = 2
	SideOpenShort  = 3
	SideCloseLong  = 4
)

// NewClient constructs a Client.
func NewClient(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// MarketOrder describes the market order options coded to match the web client.
type MarketOrder struct {
	Symbol       string
	Side         int
	Volume       float64
	Price        float64
	OpenType     int
	Leverage     int
	ReduceOnly   bool
	PositionMode *int
	ExternalOID  string
}

// SubmitOrderResponse mirrors the API response envelope.
type SubmitOrderResponse struct {
	Success bool            `json:"success"`
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

// SubmitMarketOrder submits a market order (type=5).
func (c *Client) SubmitMarketOrder(ctx context.Context, order MarketOrder) (*SubmitOrderResponse, error) {
	return c.submitOrder(ctx, order, 5)
}

// SubmitLimitOrder submits a limit order (type=1).
func (c *Client) SubmitLimitOrder(ctx context.Context, order MarketOrder) (*SubmitOrderResponse, error) {
	return c.submitOrder(ctx, order, 1)
}

func (c *Client) submitOrder(ctx context.Context, order MarketOrder, orderType int) (*SubmitOrderResponse, error) {
	if order.Symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	payload := struct {
		Symbol       string  `json:"symbol"`
		Price        float64 `json:"price"`
		Volume       float64 `json:"vol"`
		Side         int     `json:"side"`
		Type         int     `json:"type"`
		OpenType     int     `json:"openType"`
		ReduceOnly   bool    `json:"reduceOnly"`
		Leverage     int     `json:"leverage,omitempty"`
		PositionMode *int    `json:"positionMode,omitempty"`
		ExternalOID  string  `json:"externalOid,omitempty"`
	}{
		Symbol:       order.Symbol,
		Price:        order.Price,
		Volume:       order.Volume,
		Side:         order.Side,
		Type:         orderType,
		OpenType:     order.OpenType,
		ReduceOnly:   order.ReduceOnly,
		Leverage:     order.Leverage,
		PositionMode: order.PositionMode,
		ExternalOID:  order.ExternalOID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	respBytes, err := c.request(ctx, http.MethodPost, "/private/order/submit", nil, body, true)
	if err != nil {
		return nil, err
	}
	var resp SubmitOrderResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, err
	}
	if !resp.Success {
		return &resp, &OrderError{Msg: resp.Msg}
	}
	return &resp, nil
}

// GetAccountBalance returns the available balance for the given currency.
func (c *Client) GetAccountBalance(ctx context.Context, currency string) (float64, error) {
	if currency == "" {
		currency = "USDT"
	}
	path := fmt.Sprintf("/private/account/asset/%s", strings.ToUpper(currency))
	respBytes, err := c.request(ctx, http.MethodGet, path, nil, nil, true)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Success bool            `json:"success"`
		Msg     string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return 0, err
	}
	if !resp.Success {
		return 0, fmt.Errorf("account asset failed: %s", resp.Msg)
	}
	value := extractBalanceValue(resp.Data)
	if value == 0 {
		return 0, fmt.Errorf("account asset response missing balance")
	}
	return value, nil
}

func extractBalanceValue(data json.RawMessage) float64 {
	if len(data) == 0 {
		return 0
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err == nil {
		return balanceFromMap(obj)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(data, &arr); err == nil {
		for _, item := range arr {
			if v := balanceFromMap(item); v != 0 {
				return v
			}
		}
	}
	return 0
}

func balanceFromMap(m map[string]interface{}) float64 {
	keys := []string{
		"availableMargin",
		"availableBalance",
		"available",
		"marginBalance",
		"equity",
		"balance",
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if val := parseMaybeFloat(v); val != 0 {
				return val
			}
		}
	}
	return 0
}

// TickerPrice fetches the latest futures price from the public endpoint.
func (c *Client) TickerPrice(ctx context.Context, symbol string) (float64, error) {
	if symbol == "" {
		return 0, fmt.Errorf("symbol is required")
	}
	values := url.Values{"symbol": []string{symbol}}
	respBytes, err := c.request(ctx, http.MethodGet, "/contract/ticker", values, nil, false)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			LastPrice interface{} `json:"lastPrice"`
			Close     interface{} `json:"close"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return 0, err
	}
	if !resp.Success {
		return 0, fmt.Errorf("ticker request failed: %s", resp.Msg)
	}
	price := parseMaybeFloat(resp.Data.LastPrice)
	if price == 0 {
		price = parseMaybeFloat(resp.Data.Close)
	}
	if price == 0 {
		return 0, fmt.Errorf("ticker response missing price")
	}
	return price, nil
}

func (c *Client) request(ctx context.Context, method, path string, params url.Values, body []byte, includeAuth bool) ([]byte, error) {
	base := strings.TrimRight(c.cfg.BaseURL, "/")
	fullURL := base + path
	if len(params) > 0 {
		fullURL += "?" + params.Encode()
	}

	var payload []byte
	if len(body) > 0 {
		payload = body
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	headers := buildHeaders(includeAuth, payload, c.cfg.AuthToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, &StatusError{Code: resp.StatusCode, Body: string(bodyBytes)}
	}

	return io.ReadAll(resp.Body)
}

// StatusError описывает HTTP-ответ MEXC с ошибкой.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Code, e.Body)
}

func buildHeaders(includeAuth bool, body []byte, token string) map[string]string {
	headers := map[string]string{}
	for k, v := range defaultHeaders {
		headers[k] = v
	}
	if len(body) > 0 {
		headers["content-type"] = "application/json"
		headers["content-length"] = fmt.Sprintf("%d", len(body))
	}
	if includeAuth {
		headers["authorization"] = token
		if len(body) > 0 {
			ts := fmt.Sprintf("%d", time.Now().UnixMilli())
			headers["x-mxc-nonce"] = ts
			headers["x-mxc-sign"] = signPayload(token, ts, body)
		}
	}
	return headers
}

// OrderError описывает ошибку MEXC при работе с ордерами.
type OrderError struct {
	Msg string
}

func (e *OrderError) Error() string {
	return e.Msg
}

func signPayload(token, timestamp string, body []byte) string {
	first := md5.Sum([]byte(token + timestamp))
	firstHex := hex.EncodeToString(first[:])[7:]
	payload := append([]byte(timestamp), body...)
	payload = append(payload, []byte(firstHex)...)
	second := md5.Sum(payload)
	return hex.EncodeToString(second[:])
}

func parseMaybeFloat(value interface{}) float64 {
	switch v := value.(type) {
	case string:
		if v == "" {
			return 0
		}
		f, _ := strconv.ParseFloat(v, 64)
		return f
	case float64:
		return v
	case json.Number:
		f, _ := v.Float64()
		return f
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	default:
		return 0
	}
}

var defaultHeaders = map[string]string{
	"accept":             "*/*",
	"accept-language":    "en-US,en;q=0.9,ru;q=0.8,it;q=0.7,la;q=0.6,vi;q=0.5,lb;q=0.4",
	"cache-control":      "no-cache",
	"content-type":       "application/json",
	"dnt":                "1",
	"language":           "English",
	"origin":             "https://www.mexc.com",
	"pragma":             "no-cache",
	"priority":           "u=1, i",
	"referer":            "https://www.mexc.com/",
	"sec-ch-ua":          `"Chromium";v="136", "Google Chrome";v="136", "Not.A/Brand";v="99"`,
	"sec-ch-ua-mobile":   "?0",
	"sec-ch-ua-platform": `"macOS"`,
	"sec-fetch-dest":     "empty",
	"sec-fetch-mode":     "cors",
	"sec-fetch-site":     "same-site",
	"user-agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
	"x-language": "en-US",
}
