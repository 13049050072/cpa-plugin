package main

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type authModelRequestWire struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

var staticModels = []pluginapi.ModelInfo{
	mkModel("deepseek/deepseek-v4.1-flash", 1048576),
	mkModel("moonshotai/kimi-k3.1-pro", 1048576),
	mkModel("nvidia/nemotron-3-ultra-550b-a55b", 1048576),
	mkModel("z-ai/glm-5.3", 262144),
	mkModel("z-ai/glm-5.3-flash", 1048576),
	mkModel("uniboshi/kimi-k3-abliterated-v1", 1048576),
	mkModel("dealignai/deepseek-v4.1-flash-uncensored-fp8", 1048576),
	mkModel("dealignai/glm-5.3-uncensored-fp8", 1048576),
	mkModel("dealignai/glm-5.3-flash-uncensored-fp8", 1048576),
}

func mkModel(id string, ctx int64) pluginapi.ModelInfo {
	return pluginapi.ModelInfo{
		ID:                         id,
		Name:                       id,
		DisplayName:                id,
		OwnedBy:                    providerName,
		Type:                       "chat",
		Object:                     "model",
		SupportedGenerationMethods: []string{"chat"},
		ContextLength:              ctx,
		MaxCompletionTokens:        65536,
	}
}

func handleModelStatic(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: staticModels})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req authModelRequestWire
	_ = json.Unmarshal(raw, &req)
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: staticModels})
	}
	models := fetchModels(sa, req.HostCallbackID)
	if len(models) == 0 {
		models = staticModels
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

// fetchModels lists models for the key. Paid models are hidden unless
// NUMES_ALL_MODELS=1, because a $0 balance can only use the free tier.
func fetchModels(sa *storedAuth, callbackID string) []pluginapi.ModelInfo {
	resp, err := getJSON(gatewayBase+"/models", map[string]string{
		"Authorization": "Bearer " + sa.Auth.AccessToken,
	}, callbackID)
	if err != nil || resp.StatusCode >= 400 {
		return nil
	}
	var wrap struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
			Pricing       struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if json.Unmarshal(resp.Body, &wrap) != nil {
		return nil
	}
	includePaid := strings.TrimSpace(os.Getenv("NUMES_ALL_MODELS")) == "1"
	out := make([]pluginapi.ModelInfo, 0, len(wrap.Data))
	for _, m := range wrap.Data {
		if m.ID == "" {
			continue
		}
		if !includePaid && !isFreePrice(m.Pricing.Prompt, m.Pricing.Completion) {
			continue
		}
		out = append(out, mkModel(m.ID, m.ContextLength))
	}
	return out
}

func isFreePrice(prompt, completion string) bool {
	return strings.TrimSpace(prompt) == "0" && strings.TrimSpace(completion) == "0"
}
