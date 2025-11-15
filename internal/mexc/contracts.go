package mexc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const contractDetailURL = "https://contract.mexc.com/api/v1/contract/detail"

// ContractMeta mirrors the metadata required to size positions.
type ContractMeta struct {
	Symbol       string
	BaseCoin     string
	ContractSize float64
	MinVolume    float64
	VolumeStep   float64
}

// FetchContractMetadata retrieves metadata for all contracts.
func FetchContractMetadata(ctx context.Context) (map[string]ContractMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, contractDetailURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("contract detail http %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			Symbol       string      `json:"symbol"`
			BaseCoin     string      `json:"baseCoin"`
			ContractSize interface{} `json:"contractSize"`
			MinVol       interface{} `json:"minVol"`
			VolScale     interface{} `json:"volScale"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	meta := make(map[string]ContractMeta, len(body.Data))
	for _, entry := range body.Data {
		if entry.Symbol == "" {
			continue
		}
		contractSize := parseFloat(entry.ContractSize, 1)
		minVol := parseFloat(entry.MinVol, 0)
		volScale := parseFloat(entry.VolScale, 0)
		step := 1.0
		if volScale > 0 {
			step = pow10(-int(volScale))
		}
		meta[entry.Symbol] = ContractMeta{
			Symbol:       entry.Symbol,
			BaseCoin:     entry.BaseCoin,
			ContractSize: contractSize,
			MinVolume:    minVol,
			VolumeStep:   step,
		}
	}
	return meta, nil
}

func parseFloat(v interface{}, def float64) float64 {
	switch t := v.(type) {
	case string:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f
		}
	case float64:
		return t
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	}
	return def
}

func pow10(exp int) float64 {
	if exp == 0 {
		return 1
	}
	result := 1.0
	if exp > 0 {
		for i := 0; i < exp; i++ {
			result *= 10
		}
		return result
	}
	for i := 0; i < -exp; i++ {
		result /= 10
	}
	return result
}
