package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:embed panel.html
var panelHTML []byte

var (
	mgmtBase = "/v0/management"
	resBase  = "/v0/resource/plugins/" + providerName
)

type managementRequestWire struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List 50x keys (local only)."},
			{Method: http.MethodPost, Path: base + "/usage", Description: "Local counts plus dashboard requests/tokens."},
			{Method: http.MethodPost, Path: base + "/import", Description: "Import a sk-50x-… API key."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "50x", Description: "50x keys, balance and local request counts."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")
	if req.Method == http.MethodGet && strings.HasPrefix(path, resBase) {
		sub := strings.TrimPrefix(path, resBase)
		return okEnvelope(mgmtHTMLResponse(servePanel(sub)))
	}
	base := mgmtBase + "/plugins/" + providerName
	switch {
	case req.Method == http.MethodGet && path == base+"/accounts":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleAccounts()))
	case req.Method == http.MethodPost && path == base+"/usage":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleUsage(req)))
	case req.Method == http.MethodPost && path == base+"/import":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleImport(req.ManagementRequest)))
	default:
		return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
	}
}

func handleAccounts() map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	accounts := make([]map[string]any, 0, len(files))
	for _, f := range files {
		row := map[string]any{"auth_index": f.AuthIndex, "name": f.Name, "label": f.Label, "disabled": f.Disabled}
		raw, err := hostAuthGetJSON(f.AuthIndex)
		if err == nil {
			if sa, err := parseStored(raw); err == nil {
				row["uid"] = sa.Account.UID
				row["nickname"] = sa.Account.Nickname
				row["email"] = sa.Account.Email
				row["key_prefix"] = prefixKey(sa.Auth.AccessToken)
			}
		}
		accounts = append(accounts, row)
	}
	return map[string]any{"accounts": accounts, "site": siteURL, "count": len(accounts)}
}

type probeResult struct {
	alive   bool
	invalid bool
	detail  string
	at      time.Time
}

// probeCache keeps conclusive probe results for a few minutes: the panel probes
// every key on load, and re-probing all of them each time both stalls the page
// and punts transient network errors into the "dead key" badge.
var probeCache = struct {
	sync.Mutex
	m map[string]probeResult
}{m: map[string]probeResult{}}

const probeTTL = 5 * time.Minute

func cachedProbe(key string) probeResult {
	probeCache.Lock()
	r, ok := probeCache.m[key]
	probeCache.Unlock()
	if ok && time.Since(r.at) < probeTTL {
		return r
	}
	alive, invalid, detail := probeKey(key, "")
	r = probeResult{alive: alive, invalid: invalid, detail: detail, at: time.Now()}
	if alive || invalid {
		probeCache.Lock()
		probeCache.m[key] = r
		probeCache.Unlock()
	}
	return r
}

func handleUsage(req managementRequestWire) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
		Login     bool   `json:"login"`
		Refresh   bool   `json:"refresh"`
	}
	_ = json.Unmarshal(req.Body, &body)
	id := strings.TrimSpace(body.AuthIndex)
	if id == "" && req.Query != nil {
		id = strings.TrimSpace(req.Query.Get("auth_index"))
	}
	if id == "" {
		return map[string]any{"error": "missing auth_index"}
	}
	name, raw, err := hostAuthGet(id)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	sa, err := parseStored(raw)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	out := map[string]any{
		"auth_index": id,
		"name":       name,
		"local":      getStats(sa.Account.UID),
		"balance":    balanceFor(sa, body.Refresh),
	}
	if body.Login {
		pr := cachedProbe(sa.Auth.AccessToken)
		state := "unknown"
		if pr.alive {
			state = "ok"
		} else if pr.invalid {
			state = "invalid"
		}
		out["alive"] = pr.alive
		out["state"] = state
		out["probe"] = shortErr(pr.detail)
	}
	return out
}

func prefixKey(key string) string {
	if len(key) <= 16 {
		return key
	}
	return key[:12] + "…" + key[len(key)-4:]
}

func handleImport(req pluginapi.ManagementRequest) map[string]any {
	raw := bytesTrimSpace(req.Body)
	var wrap struct {
		JSON json.RawMessage `json:"json"`
		Raw  string          `json:"raw"`
		Key  string          `json:"key"`
	}
	_ = json.Unmarshal(raw, &wrap)

	sa, err := parseStored(raw)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	return saveImported(sa)
}

func saveImported(sa *storedAuth) map[string]any {
	fileJSON, err := buildAuthFileJSON(sa, false, "")
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	name := authFileNameFor(sa)
	if err := hostAuthSaveJSON(name, fileJSON); err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	return map[string]any{"success": true, "file": name, "uid": sa.Account.UID}
}

func servePanel(sub string) []byte {
	if sub != "" && sub != "/" && sub != "/panel" && sub != "/panel.html" {
		return []byte("<h1>404</h1>")
	}
	base, _ := json.Marshal(mgmtBase)
	return bytes.ReplaceAll(panelHTML, []byte("__NM_MANAGEMENT_BASE_PATH_JSON__"), base)
}

func mgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}

func shortErr(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
