package main

import "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

// 50x currently serves one model on its Vast AI cluster. qwen3.8-27b-bf16 is
// listed in billing prices but returns 503 (upstream ConnectTimeout), so it is
// not advertised until it actually answers.
var staticModels = []pluginapi.ModelInfo{
	mkModel("deepseek-ai/DeepSeek-V4.1-Flash-1017", 1048576),
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

// handleModelForAuth returns the fixed catalogue: unlike numes there is no
// per-key model discovery, and probing /v1/models on every CPA start only
// burns rate limit for nothing.
func handleModelForAuth(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: staticModels})
}
