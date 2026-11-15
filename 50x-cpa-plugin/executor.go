package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type executorRequestWire struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// 50x sheds load per key: a second in-flight request on the same credential
// gets 503 {"code":"gateway_overloaded"} and CPA also cools that credential
// down for a minute, so requests are queued per key instead of racing. Errors
// are returned before a stream opens, which keeps the 503 retryable by CPA.
var keySlots sync.Map // uid -> chan struct{} (capacity 1)

// The 50x edge itself sheds with 502/503 under load. CPA does not retry a
// failed plugin Execute on another credential, so retry here, with backoff,
// while the request has not produced a byte yet.
const execAttempts = 3

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryBackoff(attempt int) {
	d := time.Duration(800<<uint(attempt)) * time.Millisecond
	select {
	case <-time.After(d):
	case <-bgCtx.Done():
	}
}

func lockKey(uid string) (release func(), ok bool) {
	v, _ := keySlots.LoadOrStore(uid, make(chan struct{}, 1))
	ch := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	case <-bgCtx.Done():
		return nil, false
	}
}

func newChatRequest(body []byte, token string) (*http.Request, error) {
	httpReq, err := http.NewRequest(http.MethodPost, gatewayBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	return httpReq, nil
}

func handleExecExecute(raw []byte) ([]byte, error) {
	var req executorRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	body = rewriteModel(body, req.Model, false)
	uid := sa.Account.UID
	release, ok := lockKey(uid)
	if !ok {
		return nil, fmt.Errorf("shutting down")
	}
	defer release()
	bump(uid)
	var resp *hostHTTPResponse
	for attempt := 0; attempt < execAttempts; attempt++ {
		if attempt > 0 {
			retryBackoff(attempt - 1)
		}
		httpReq, errReq := newChatRequest(body, sa.Auth.AccessToken)
		if errReq != nil {
			return nil, errReq
		}
		resp, err = hostHTTPDoWithCallback(httpReq, req.HostCallbackID)
		if err != nil {
			if attempt < execAttempts-1 {
				continue
			}
			bumpError(uid)
			return nil, fmt.Errorf("http_error: %w", err)
		}
		if retryableStatus(resp.StatusCode) && attempt < execAttempts-1 {
			continue
		}
		break
	}
	if resp.StatusCode >= 400 {
		bumpError(uid)
		return errorEnvelopeWithStatus("http_error", upstreamMessage(resp.StatusCode, resp.Body), resp.StatusCode), nil
	}
	addTokens(uid, tokensFromBody(resp.Body))
	return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body})
}

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	body = rewriteModel(body, req.Model, true)
	uid := sa.Account.UID
	release, ok := lockKey(uid)
	if !ok {
		return nil, fmt.Errorf("shutting down")
	}
	bump(uid)
	prefix := streamPrefix(req)
	headers := http.Header{}
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")

	// Open upstream before handing a stream back: a 503 must surface as a
	// failed Execute so the request can be retried on another credential.
	stream, status, err := openUpstreamStream(body, sa.Auth.AccessToken, req.HostCallbackID)
	if err != nil {
		release()
		bumpError(uid)
		return nil, err
	}
	if status >= 400 {
		defer stream.Close()
		release()
		bumpError(uid)
		payload, _ := io.ReadAll(newHostStreamReader(stream))
		return errorEnvelopeWithStatus("http_error", upstreamMessage(status, payload), status), nil
	}

	if req.StreamID == "" {
		defer stream.Close()
		defer release()
		payload, _ := io.ReadAll(newHostStreamReader(stream))
		addTokens(uid, tokensFromStream(bytes.Split(payload, []byte("\n"))))
		return okEnvelope(streamResponse{Headers: headers, Chunks: streamChunksFromPayload(payload, prefix)})
	}

	// The key stays locked until the stream is drained: the upstream call is
	// still in flight for the whole answer.
	bgWG.Add(1)
	go pumpStream(stream, release, req.StreamID, uid, prefix)
	return okEnvelope(streamResponse{Headers: headers})
}

func openUpstreamStream(body []byte, token, callbackID string) (*hostHTTPStream, int, error) {
	var stream *hostHTTPStream
	var status int
	for attempt := 0; attempt < execAttempts; attempt++ {
		if attempt > 0 {
			retryBackoff(attempt - 1)
		}
		httpReq, err := newChatRequest(body, token)
		if err != nil {
			return nil, 0, err
		}
		// 50x's DRF layer only negotiates */* or application/json; an explicit
		// text/event-stream Accept is rejected with 406 before the stream starts.
		httpReq.Header.Set("Accept", "*/*")
		var errDo error
		stream, status, _, errDo = hostHTTPDoStreamWithCallback(httpReq, callbackID)
		if errDo != nil {
			if attempt < execAttempts-1 {
				continue
			}
			return nil, 0, errDo
		}
		if retryableStatus(status) && attempt < execAttempts-1 {
			stream.Close()
			continue
		}
		break
	}
	return stream, status, nil
}

func pumpStream(stream *hostHTTPStream, release func(), streamID, uid, prefix string) {
	defer bgWG.Done()
	defer release()
	defer streamClose(streamID)
	defer stream.Close()
	sc := bufio.NewScanner(newHostStreamReader(stream))
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	var last usageBlock
	for sc.Scan() {
		line := sc.Bytes()
		if u, ok := tokensFromFrame(line); ok {
			last = u
		}
		frame, ok := normalizeFrame(line, prefix)
		if !ok {
			continue
		}
		if err := streamEmit(streamID, frame); err != nil {
			return
		}
	}
	addTokens(uid, last)
}

func upstreamMessage(status int, body []byte) string {
	if status == http.StatusPaymentRequired {
		return "50x: insufficient credits — top up at https://ai.50x.energy/pricing"
	}
	return fmt.Sprintf("upstream %d: %s", status, truncate(string(body), 300))
}

func rewriteModel(body []byte, model string, stream bool) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if strings.TrimSpace(model) != "" {
		m["model"] = model
	}
	if stream {
		m["stream"] = true
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// normalizeFrame turns one upstream SSE line into the payload the host expects.
// "[DONE]" and blank lines are dropped: the host emits its own terminator.
func normalizeFrame(line []byte, prefix string) ([]byte, bool) {
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) {
		return nil, false
	}
	data := bytes.TrimSpace(t[len("data:"):])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !json.Valid(data) {
		return nil, false
	}
	return append([]byte(prefix), data...), true
}

func streamChunksFromPayload(payload []byte, prefix string) []pluginapi.ExecutorStreamChunk {
	var out []pluginapi.ExecutorStreamChunk
	for _, line := range bytes.Split(payload, []byte("\n")) {
		if frame, ok := normalizeFrame(line, prefix); ok {
			out = append(out, pluginapi.ExecutorStreamChunk{Payload: frame})
		}
	}
	return out
}

// streamPrefix reports whether chunks need the "data: " prefix. The OpenAI
// handlers wrap payloads themselves; the translation paths (claude, responses,
// gemini) expect upstream-style SSE lines.
func streamPrefix(req executorRequestWire) string {
	path, _ := req.Metadata["request_path"].(string)
	switch path {
	case "/v1/chat/completions", "/v1/completions":
		return ""
	case "":
		if req.Headers.Get("Anthropic-Version") == "" {
			return ""
		}
	}
	return "data: "
}
