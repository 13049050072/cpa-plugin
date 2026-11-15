package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type authLoginPollWire struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type authRefreshWire struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func is50xKey(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), keyPrefix)
}

func handleStartLogin(_ []byte) ([]byte, error) {
	now := time.Now()
	state := fmt.Sprintf("fx-%d", now.UnixNano())
	loginStates.Store(state, &loginCtx{expires: now.Add(loginTTL)})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       siteURL + "/dashboard/keys",
		State:     state,
		ExpiresAt: now.Add(loginTTL).UTC(),
		Metadata: map[string]any{
			"logo":   pluginLogoURL,
			"prompt": "在 https://ai.50x.energy/dashboard 的 Keys 页建一把 key，把 sk-50x-… 填进来。",
		},
	})
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req authLoginPollWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired")
	}
	if req.Metadata != nil {
		if k, _ := req.Metadata["key"].(string); is50xKey(k) {
			loginStates.Delete(state)
			sa := storedFromKey(strings.TrimSpace(k), "", "")
			return okEnvelope(pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusSuccess,
				Message: "ok",
				Auth:    toAuthDataForLoginPoll(sa),
			})
		}
	}
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: "pending — paste sk-50x-… in the 50x panel Import",
	})
}

func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req authRefreshWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	resp, err := getJSON(gatewayBase+"/models", map[string]string{
		"Authorization": "Bearer " + sa.Auth.AccessToken,
	}, req.HostCallbackID)
	if err != nil {
		return nil, fmt.Errorf("refresh probe: %w", err)
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("refresh: key rejected (HTTP %d)", resp.StatusCode)
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             toAuthDataForRefresh(sa),
		NextRefreshAfter: time.Now().Add(24 * time.Hour).UTC(),
	})
}
