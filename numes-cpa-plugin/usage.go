package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type keyUsage struct {
	Label           string   `json:"label"`
	Name            string   `json:"name"`
	BillingCurrency string   `json:"billing_currency"`
	Disabled        bool     `json:"disabled"`
	Limit           *float64 `json:"limit"`
	LimitRemaining  *float64 `json:"limit_remaining"`
	Usage           float64  `json:"usage"`
	CreatedAt       string   `json:"created_at"`
}

// fetchKeyUsage reads per-key spend from GET /key. Free models report 0, so
// this only proves the key is alive.
func fetchKeyUsage(sa *storedAuth, callbackID string) (*keyUsage, error) {
	resp, err := getJSON(gatewayBase+"/key", map[string]string{
		"Authorization": "Bearer " + sa.Auth.AccessToken,
	}, callbackID)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(resp.Body), 200))
	}
	var wrap struct {
		Data *keyUsage `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &wrap); err != nil {
		return nil, err
	}
	if wrap.Data == nil {
		return nil, fmt.Errorf("unexpected /key response")
	}
	return wrap.Data, nil
}

// probeKey verifies a key by listing models. Numes returns 401 for a bad key.
func probeKey(key, callbackID string) (bool, string) {
	resp, err := getJSON(gatewayBase+"/models", map[string]string{"Authorization": "Bearer " + key}, callbackID)
	if err != nil {
		return false, err.Error()
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Sprintf("HTTP %d %s", resp.StatusCode, truncate(string(resp.Body), 160))
	}
	return true, "ok"
}

type usageBlock struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// tokensFromBody extracts usage from a non-stream chat completion response.
func tokensFromBody(body []byte) usageBlock {
	var wrap struct {
		Usage *usageBlock `json:"usage"`
	}
	if json.Unmarshal(body, &wrap) != nil || wrap.Usage == nil {
		return usageBlock{}
	}
	return *wrap.Usage
}

// tokensFromStream scans SSE frames and returns the last usage block seen.
// Numes sends usage in the final chunk of a successful stream.
func tokensFromFrame(frame []byte) (usageBlock, bool) {
	line := strings.TrimSpace(string(frame))
	line = strings.TrimPrefix(line, "data:")
	line = strings.TrimSpace(line)
	if line == "" || line == "[DONE]" || !strings.Contains(line, `"usage"`) {
		return usageBlock{}, false
	}
	var chunk struct {
		Usage *usageBlock `json:"usage"`
	}
	if json.Unmarshal([]byte(line), &chunk) != nil || chunk.Usage == nil {
		return usageBlock{}, false
	}
	return *chunk.Usage, true
}

func tokensFromStream(frames [][]byte) usageBlock {
	var last usageBlock
	for _, f := range frames {
		if u, ok := tokensFromFrame(f); ok {
			last = u
		}
	}
	return last
}
