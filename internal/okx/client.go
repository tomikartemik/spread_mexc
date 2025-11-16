package okx

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const pricePath = "/api/v6/dex/market/price"

// Client работает с OKX Web3 DEX API.
type Client struct {
	baseURL    string
	accessKey  string
	secretKey  string
	passphrase string
	httpClient *http.Client
	batchDelay time.Duration
	maxRetries int
}

// Token описывает запрос цены.
type Token struct {
	Name       string
	ChainIndex string
	Address    string
}

// New создаёт клиент OKX.
func New(baseURL, accessKey, secretKey, passphrase string, timeout time.Duration, batchDelay time.Duration, maxRetries int) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if maxRetries < 1 {
		maxRetries = 1
	}
	return &Client{
		baseURL:    baseURL,
		accessKey:  accessKey,
		secretKey:  secretKey,
		passphrase: passphrase,
		httpClient: &http.Client{Timeout: timeout},
		batchDelay: batchDelay,
		maxRetries: maxRetries,
	}
}

const maxBatchSize = 50

// FetchPrices возвращает цены (в USDT) для указанных токенов.
func (c *Client) FetchPrices(ctx context.Context, tokens []Token) (map[string]float64, error) {
	if len(tokens) == 0 {
		return map[string]float64{}, nil
	}
	result := make(map[string]float64, len(tokens))
	for start := 0; start < len(tokens); start += maxBatchSize {
		end := start + maxBatchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		chunk := tokens[start:end]
		batchPrices, err := c.fetchBatchWithRetry(ctx, chunk)
		if err != nil {
			return nil, err
		}
		for k, v := range batchPrices {
			result[k] = v
		}
		if c.batchDelay > 0 && end < len(tokens) {
			time.Sleep(c.batchDelay)
		}
	}
	return result, nil
}

func (c *Client) fetchBatchWithRetry(ctx context.Context, tokens []Token) (map[string]float64, error) {
	var lastErr error
	backoff := c.batchDelay
	if backoff <= 0 {
		backoff = 200 * time.Millisecond
	}
	for attempt := 1; attempt <= c.maxRetries; attempt++ {
		prices, err := c.fetchBatchOnce(ctx, tokens)
		if err == nil {
			return prices, nil
		}
		lastErr = err
		var se *statusError
		if errors.As(err, &se) && se.Code == http.StatusTooManyRequests && attempt < c.maxRetries {
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		break
	}
	return nil, lastErr
}

func (c *Client) fetchBatchOnce(ctx context.Context, tokens []Token) (map[string]float64, error) {
	reqItems := make([]map[string]string, 0, len(tokens))
	for _, t := range tokens {
		if t.ChainIndex == "" || t.Address == "" {
			continue
		}
		reqItems = append(reqItems, map[string]string{
			"chainIndex":           t.ChainIndex,
			"tokenContractAddress": t.Address,
		})
	}
	if len(reqItems) == 0 {
		return map[string]float64{}, nil
	}
	body, err := json.Marshal(reqItems)
	if err != nil {
		return nil, err
	}

	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	sign := c.sign(ts, "POST", pricePath, string(body))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+pricePath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OK-ACCESS-KEY", c.accessKey)
	req.Header.Set("OK-ACCESS-PASSPHRASE", c.passphrase)
	req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
	req.Header.Set("OK-ACCESS-SIGN", sign)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, &statusError{Code: resp.StatusCode}
	}

	var decoded struct {
		Code string          `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	if decoded.Code != "0" {
		return nil, fmt.Errorf("okx error %s: %s", decoded.Code, decoded.Msg)
	}
	items, err := parsePriceItems(decoded.Data)
	if err != nil {
		return nil, err
	}
	result := make(map[string]float64, len(items))
	for _, item := range items {
		if item.Price == "" {
			continue
		}
		price, err := strconv.ParseFloat(item.Price, 64)
		if err != nil {
			continue
		}
		result[key(item.ChainIndex, item.Address)] = price
	}
	return result, nil
}

type statusError struct {
	Code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("okx http %d", e.Code)
}

type priceItem struct {
	ChainIndex string `json:"chainIndex"`
	Address    string `json:"tokenContractAddress"`
	Price      string `json:"price"`
}

func parsePriceItems(raw json.RawMessage) ([]priceItem, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return nil, nil
	}
	var arr []priceItem
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var wrapper struct {
		Result []priceItem `json:"result"`
		Data   []priceItem `json:"data"`
		Rows   []priceItem `json:"rows"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil {
		switch {
		case len(wrapper.Result) > 0:
			return wrapper.Result, nil
		case len(wrapper.Data) > 0:
			return wrapper.Data, nil
		case len(wrapper.Rows) > 0:
			return wrapper.Rows, nil
		}
	}
	return nil, fmt.Errorf("unsupported OKX price format: %s", trimmed)
}

func (c *Client) sign(timestamp, method, path, body string) string {
	payload := timestamp + method + path + body
	mac := hmac.New(sha256.New, []byte(c.secretKey))
	mac.Write([]byte(payload))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Key формирует ключ для мапы цен.
func key(chainIndex, address string) string {
	return chainIndex + "|" + address
}

// TokenKey helper.
func TokenKey(chainIndex, address string) string {
	return key(chainIndex, address)
}
