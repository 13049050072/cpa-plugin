package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseStored(t *testing.T) {
	sa, err := parseStored([]byte(`{"auth":{"accessToken":"sk-50x-abc"},"account":{"uid":"u1"}}`))
	if err != nil || sa.Auth.AccessToken != "sk-50x-abc" {
		t.Fatalf("nested %#v %v", sa, err)
	}
	sa, err = parseStored([]byte(`{"key":"sk-50x-flat"}`))
	if err != nil || sa.Auth.AccessToken != "sk-50x-flat" {
		t.Fatalf("key %#v %v", sa, err)
	}
	sa, err = parseStored([]byte(" sk-50x-raw "))
	if err != nil || sa.Auth.AccessToken != "sk-50x-raw" {
		t.Fatalf("raw %#v %v", sa, err)
	}
	if sa.Auth.Domain != gatewayBase {
		t.Fatalf("domain = %q", sa.Auth.Domain)
	}
}

func TestUIDFromKey(t *testing.T) {
	if got := uidFromKey("sk-50x-0123456789abcdef0123"); got != "456789abcdef0123" {
		t.Fatalf("uid = %q", got)
	}
}

func TestParseAuthSkipForeign(t *testing.T) {
	body, err := handleParseAuth(mustJSON(pluginapi.AuthParseRequest{
		FileName: "tokenharbor-1.json",
		RawJSON:  []byte(`{"type":"tokenharbor","auth":{"accessToken":"thk_live_x"}}`),
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(body, &env)
	var resp pluginapi.AuthParseResponse
	_ = json.Unmarshal(env.Result, &resp)
	if resp.Handled {
		t.Fatal("claimed tokenharbor file")
	}
}

func TestParseAuthClaim(t *testing.T) {
	body, err := handleParseAuth(mustJSON(pluginapi.AuthParseRequest{
		FileName: "50x-abc.json",
		RawJSON:  []byte(`{"key":"sk-50x-abcdef1234567890"}`),
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(body, &env)
	var resp pluginapi.AuthParseResponse
	_ = json.Unmarshal(env.Result, &resp)
	if !resp.Handled || resp.Auth.ID != "" || resp.Auth.FileName != "50x-abc.json" {
		t.Fatalf("%#v", resp)
	}
}

func TestStaticModelsNonEmpty(t *testing.T) {
	if len(staticModels) == 0 {
		t.Fatal("no static models")
	}
	for _, m := range staticModels {
		if m.ID == "" || m.ContextLength <= 0 {
			t.Fatalf("bad model %#v", m)
		}
	}
}

func TestRewriteModel(t *testing.T) {
	out := rewriteModel([]byte(`{"model":"x","stream":false}`), "deepseek/deepseek-v4.1-flash", true)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "deepseek/deepseek-v4.1-flash" || m["stream"] != true {
		t.Fatalf("%v", m)
	}
}

func TestLiveExecute(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("FIFTYX_TEST_KEY"))
	if key == "" {
		t.Skip("set FIFTYX_TEST_KEY to run live calls")
	}
	sa := storedFromKey(key, "", "")
	storage, _ := json.Marshal(sa)
	req := executorRequestWire{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:           "deepseek/deepseek-v4.1-flash",
		OriginalRequest: []byte(`{"model":"x","messages":[{"role":"user","content":"Say OK"}],"max_tokens":300}`),
		StorageJSON:     storage,
	}}
	raw, err := handleExecExecute(mustJSON(req))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("envelope error: %+v", env.Error)
	}
	var resp pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(resp.Payload) || !strings.Contains(string(resp.Payload), "choices") {
		t.Fatalf("unexpected payload: %.200s", resp.Payload)
	}
	st := getStats(sa.Account.UID)
	if st.Total == 0 {
		t.Fatal("request not counted")
	}
	if st.TokensTotal == 0 {
		t.Fatalf("tokens not counted: %+v", st)
	}
	t.Logf("counted: requests=%d tokens=%d errors=%d", st.Total, st.TokensTotal, st.Errors)
}

func TestLiveModels(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("FIFTYX_TEST_KEY"))
	if key == "" {
		t.Skip("set FIFTYX_TEST_KEY to run live calls")
	}
	alive, invalid, detail := probeKey(key, "")
	if !alive {
		t.Fatalf("probe failed invalid=%v %s", invalid, detail)
	}
}

func TestBumpStats(t *testing.T) {
	uid := "test-stats-" + time.Now().Format("150405.000000")
	bump(uid)
	bump(uid)
	bumpError(uid)
	s := getStats(uid)
	if s.Total != 2 || s.Today != 2 || s.Errors != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestAddTokens(t *testing.T) {
	uid := "test-tokens-" + time.Now().Format("150405.000000")
	bump(uid)
	addTokens(uid, usageBlock{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})
	addTokens(uid, usageBlock{PromptTokens: 3, CompletionTokens: 2})
	st := getStats(uid)
	if st.TokensTotal != 20 || st.TokensToday != 20 {
		t.Fatalf("%+v", st)
	}
}

func TestTokensFromBody(t *testing.T) {
	body := []byte(`{"choices":[],"usage":{"prompt_tokens":36,"completion_tokens":12,"total_tokens":48}}`)
	u := tokensFromBody(body)
	if u.TotalTokens != 48 || u.PromptTokens != 36 || u.CompletionTokens != 12 {
		t.Fatalf("%+v", u)
	}
	if got := tokensFromBody([]byte(`{"error":1}`)); got.TotalTokens != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestTokensFromFrame(t *testing.T) {
	frame := []byte(`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	u, ok := tokensFromFrame(frame)
	if !ok || u.TotalTokens != 10 {
		t.Fatalf("%+v %v", u, ok)
	}
	if _, ok := tokensFromFrame([]byte(`data: [DONE]`)); ok {
		t.Fatal("DONE parsed as usage")
	}
	if _, ok := tokensFromFrame([]byte(`data: {"choices":[{"delta":{}}]}`)); ok {
		t.Fatal("delta parsed as usage")
	}
}

func TestLiveStreamTokens(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("FIFTYX_TEST_KEY"))
	if key == "" {
		t.Skip("set FIFTYX_TEST_KEY to run live calls")
	}
	sa := storedFromKey(key, "", "")
	storage, _ := json.Marshal(sa)
	before := getStats(sa.Account.UID).TokensTotal
	req := executorRequestWire{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:           "deepseek/deepseek-v4.1-flash",
		OriginalRequest: []byte(`{"model":"x","messages":[{"role":"user","content":"Say OK"}],"max_tokens":300,"stream":true}`),
		StorageJSON:     storage,
	}}
	raw, err := handleExecStream(mustJSON(req))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("envelope error: %+v", env.Error)
	}
	var resp streamResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Chunks) == 0 || !strings.HasPrefix(strings.TrimSpace(string(resp.Chunks[0].Payload)), "{") {
		t.Fatalf("expected bare JSON chunk: %.160s", string(resp.Chunks[0].Payload))
	}
	after := getStats(sa.Account.UID).TokensTotal
	if after <= before {
		t.Fatalf("stream tokens not counted: before=%d after=%d", before, after)
	}
	t.Logf("stream tokens counted: +%d", after-before)
}

func TestStatsPersistence(t *testing.T) {
	old := statsPath
	statsPath = t.TempDir() + "/stats.json"
	defer func() { statsPath = old }()

	uid := "persist-" + time.Now().Format("150405.000000")
	bump(uid)
	addTokens(uid, usageBlock{TotalTokens: 42})
	saveStats()

	statsMu.Lock()
	statsData = map[string]*reqStats{}
	statsMu.Unlock()
	loadStats()

	st := getStats(uid)
	if st.Total != 1 || st.TokensTotal != 42 {
		t.Fatalf("%+v", st)
	}
}

func TestNormalizeFrame(t *testing.T) {
	line := []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`)
	bare, ok := normalizeFrame(line, "")
	if !ok || !json.Valid(bare) || bytes.HasPrefix(bare, []byte("data:")) {
		t.Fatalf("bare: %q %v", bare, ok)
	}
	pref, ok := normalizeFrame(line, "data: ")
	if !ok || !bytes.HasPrefix(pref, []byte("data: ")) || !json.Valid(bytes.TrimPrefix(pref, []byte("data: "))) {
		t.Fatalf("prefixed: %q %v", pref, ok)
	}
	for _, drop := range [][]byte{[]byte("data: [DONE]"), []byte(""), []byte(": keepalive"), []byte("data: not-json")} {
		if _, ok := normalizeFrame(drop, ""); ok {
			t.Fatalf("should drop %q", drop)
		}
	}
}

func TestProbeCacheHit(t *testing.T) {
	key := "sk-50x-cachetest"
	probeCache.Lock()
	probeCache.m[key] = probeResult{alive: true, detail: "ok", at: time.Now()}
	probeCache.Unlock()
	if pr := cachedProbe(key); !pr.alive || pr.detail != "ok" {
		t.Fatalf("cache miss: %+v", pr)
	}
}

// Shutdown must cancel in-flight work and let every goroutine exit, otherwise
// the host unloads the .so while goroutines still run (restart hangs / SIGSEGV).
// Keep this last: it cancels bgCtx for the whole test binary.
func TestShutdownStopsGoroutines(t *testing.T) {
	cliproxyPluginShutdown()
	select {
	case <-bgCtx.Done():
	default:
		t.Fatal("bgCtx not canceled")
	}
	done := make(chan struct{})
	go func() { bgWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background goroutines still running after shutdown")
	}
	if _, err := os.Stat(statsPath); err != nil {
		t.Fatalf("stats not flushed: %v", err)
	}
}

func TestLiveBalance(t *testing.T) {
	email := strings.TrimSpace(os.Getenv("FIFTYX_TEST_EMAIL"))
	password := strings.TrimSpace(os.Getenv("FIFTYX_TEST_PASSWORD"))
	if email == "" || password == "" {
		t.Skip("set FIFTYX_TEST_EMAIL/FIFTYX_TEST_PASSWORD to run live billing")
	}
	sa := &storedAuth{}
	sa.Account.Email = email
	sa.Account.Password = password
	snap := fetchBalance(sa)
	if snap.Err != "" {
		t.Fatalf("billing failed: %s", snap.Err)
	}
	if snap.BalanceUSD == "" {
		t.Fatalf("empty balance: %#v", snap)
	}
	t.Logf("balance=%s tokens_today=%d cost_today=%s cost_7d=%s req7d=%d", snap.BalanceUSD, snap.TokensToday, snap.CostToday, snap.Cost7d, snap.Requests7d)
}

func TestBalanceZero(t *testing.T) {
	cases := map[string]struct{ zero, ok bool }{
		"0":             {true, true},
		"0.000000001":   {false, true},
		"100.000000000": {false, true},
		"-1.5":          {true, true},
		"":              {false, false},
		"n/a":           {false, false},
	}
	for in, want := range cases {
		zero, ok := balanceZero(in)
		if zero != want.zero || ok != want.ok {
			t.Fatalf("balanceZero(%q) = %v,%v want %v,%v", in, zero, ok, want.zero, want.ok)
		}
	}
}

func TestApplyBalancePolicyTracksAutoDisabled(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{UID: "u-policy"}}
	// Fetch error or unparsable balance must not flip anything.
	applyBalancePolicy(sa, balanceSnap{Err: "boom"})
	applyBalancePolicy(sa, balanceSnap{BalanceUSD: ""})
	if len(autoDisabled.m) != 0 {
		t.Fatalf("policy touched state on error/empty: %#v", autoDisabled.m)
	}
}

func TestExecuteRetriesLoadShed(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"gateway_overloaded"}}`))
	}))
	defer srv.Close()
	old := gatewayBase
	gatewayBase = srv.URL
	defer func() { gatewayBase = old }()

	body, err := handleExecExecute(mustJSON(executorRequestWire{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:       "deepseek-ai/DeepSeek-V4.1-Flash-1017",
			Payload:     []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
			StorageJSON: []byte(`{"auth":{"accessToken":"sk-50x-test"},"account":{"uid":"u-shed"}}`),
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(body, &env)
	if env.OK || env.Error == nil || !strings.Contains(env.Error.Message, "gateway_overloaded") {
		t.Fatalf("env %s", body)
	}
	if env.Error.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", env.Error.HTTPStatus)
	}
	if hits != execAttempts {
		t.Fatalf("hits = %d, want %d", hits, execAttempts)
	}
}

func TestExecuteSerializesPerKey(t *testing.T) {
	var cur, peak int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&cur, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	old := gatewayBase
	gatewayBase = srv.URL
	defer func() { gatewayBase = old }()

	req := mustJSON(executorRequestWire{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:       "deepseek-ai/DeepSeek-V4.1-Flash-1017",
		Payload:     []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		StorageJSON: []byte(`{"auth":{"accessToken":"sk-50x-test"},"account":{"uid":"u-serialize"}}`),
	}})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = handleExecExecute(req)
		}()
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("peak concurrency per key = %d, want 1", peak)
	}
}

func TestExecuteRecoversOnRetry(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	old := gatewayBase
	gatewayBase = srv.URL
	defer func() { gatewayBase = old }()

	body, err := handleExecExecute(mustJSON(executorRequestWire{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:       "deepseek-ai/DeepSeek-V4.1-Flash-1017",
			Payload:     []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
			StorageJSON: []byte(`{"auth":{"accessToken":"sk-50x-test"},"account":{"uid":"u-retry-ok"}}`),
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(body, &env)
	if !env.OK || hits != 2 {
		t.Fatalf("ok=%v hits=%d body=%s", env.OK, hits, body)
	}
}
