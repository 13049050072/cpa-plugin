package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// probeKey verifies a key by listing models. Numes returns 401/403 for a bad
// key; any other failure (network, 429, 5xx) is transient and proves nothing.
func probeKey(key, callbackID string) (alive, invalid bool, detail string) {
	resp, err := getJSON(gatewayBase+"/models", map[string]string{"Authorization": "Bearer " + key}, callbackID)
	if err != nil {
		return false, false, err.Error()
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return false, true, fmt.Sprintf("HTTP %d %s", resp.StatusCode, truncate(string(resp.Body), 160))
	}
	if resp.StatusCode >= 400 {
		return false, false, fmt.Sprintf("HTTP %d %s", resp.StatusCode, truncate(string(resp.Body), 160))
	}
	return true, false, "ok"
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
