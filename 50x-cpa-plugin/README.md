# 50x CPA plugin

50X AI (`https://ai.50x.energy`) for CLIProxyAPI.

Auth: `sk-50x-…` API key. Chat: `POST https://api.ai.50x.energy/v1/chat/completions`.
OpenAI-compatible, so the CPA executor forwards chat-completions payloads unchanged.

## 模型

`deepseek-ai/DeepSeek-V4.1-Flash-1017`（1,048,576 context，vLLM）。
`qwen3.8-27b-bf16` 在价格表里有，但上游一直 503，暂不注册。

## auth 文件

```json
{
  "type": "50x",
  "provider": "50x",
  "auth": { "accessToken": "sk-50x-…" },
  "account": { "uid": "…", "email": "…" }
}
```

`accessToken` 也可以直接写成 `key` / `api_key`。面板「导入」粘贴 `sk-50x-…` 即可。

## 环境变量

- `FIFTYX_HTTP_PROXY` — 可选，插件出站代理（默认直连）

## 构建

```bash
make build     # -> 50x.so
make test
```
