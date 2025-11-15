package okx

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
}

// Token описывает запрос цены.
type Token struct {
	Name       string
	ChainIndex string
	Address    string
}

// New создаёт клиент OKX.
func New(baseURL, accessKey, secretKey, passphrase string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		baseURL:    baseURL,
		accessKey:  accessKey,
		secretKey:  secretKey,
		passphrase: passphrase,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// FetchPrices возвращает цены (в USDT) для указанных токенов.
func (c *Client) FetchPrices(ctx context.Context, tokens []Token) (map[string]float64, error) {
	if len(tokens) == 0 {
		return map[string]float64{}, nil
	}
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
		return nil, fmt.Errorf("okx http %d", resp.StatusCode)
	}

	var decoded struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			ChainIndex string `json:"chainIndex"`
			Address    string `json:"tokenContractAddress"`
			Price      string `json:"price"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	if decoded.Code != "0" {
		return nil, fmt.Errorf("okx error %s: %s", decoded.Code, decoded.Msg)
	}

	result := make(map[string]float64, len(decoded.Data))
	for _, item := range decoded.Data {
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
