package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

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
	bump(sa.Account.UID)
	httpReq, err := http.NewRequest(http.MethodPost, gatewayBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := hostHTTPDoWithCallback(httpReq, req.HostCallbackID)
	if err != nil {
		bumpError(sa.Account.UID)
		return nil, fmt.Errorf("http_error: %w", err)
	}
	if resp.StatusCode >= 400 {
		bumpError(sa.Account.UID)
		return errorEnvelopeWithStatus("http_error", upstreamMessage(resp.StatusCode, resp.Body), resp.StatusCode), nil
	}
	addTokens(sa.Account.UID, tokensFromBody(resp.Body))
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
	bump(sa.Account.UID)
	prefix := streamPrefix(req)
	headers := http.Header{}
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")

	httpReq, err := http.NewRequest(http.MethodPost, gatewayBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	if req.StreamID == "" {
		stream, status, _, err := hostHTTPDoStreamWithCallback(httpReq, req.HostCallbackID)
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		if status >= 400 {
			bumpError(sa.Account.UID)
			payload, _ := io.ReadAll(newHostStreamReader(stream))
			return errorEnvelopeWithStatus("http_error", upstreamMessage(status, payload), status), nil
		}
		payload, _ := io.ReadAll(newHostStreamReader(stream))
		addTokens(sa.Account.UID, tokensFromStream(bytes.Split(payload, []byte("\n"))))
		return okEnvelope(streamResponse{Headers: headers, Chunks: streamChunksFromPayload(payload, prefix)})
	}

	ctx, cancel := context.WithCancel(bgCtx)
	httpReq = httpReq.WithContext(ctx)
	bgWG.Add(1)
	go pumpStream(httpReq, cancel, req.StreamID, req.HostCallbackID, sa.Account.UID, prefix)
	return okEnvelope(streamResponse{Headers: headers})
}

func pumpStream(httpReq *http.Request, cancel context.CancelFunc, streamID, callbackID, uid, prefix string) {
	defer bgWG.Done()
	defer cancel()
	defer streamClose(streamID)
	stream, status, _, err := hostHTTPDoStreamWithCallback(httpReq, callbackID)
	if err != nil {
		streamEmitError(streamID, err.Error())
		return
	}
	defer stream.Close()
	if status >= 400 {
		payload, _ := io.ReadAll(newHostStreamReader(stream))
		streamEmitError(streamID, upstreamMessage(status, payload))
		return
	}
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
		return "numes: insufficient credits — free models only (deepseek/deepseek-v4.1-flash, moonshotai/kimi-k3.1-pro, …)"
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
