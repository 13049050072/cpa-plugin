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
  "account": { "uid": "…", "email": "…", "password": "…", "session": "…" }
}
```

`accessToken` 也可以直接写成 `key` / `api_key`。面板「导入」粘贴 `sk-50x-…` 即可。

`account.password` / `account.session` 只用于面板显示余额：`GET /api/billing`
（控制面是 `Authorization: Token <session>`，API key 查不到账单）。session 失效时
自动用邮箱+密码重新登录。没有这两项时余额显示 `—`，其余功能不受影响。

## 503 / 并发

50x 网关按 key 限流：同一个 key 上第二个并发请求返回
`503 {"code":"gateway_overloaded"}`，高峰期边缘还会直接 502。

- 插件按 key 排队：同一个 key 同时只发一个请求；
- 429/502/503/504 在**开流之前**重试 3 次（0.8s、1.6s 退避）；
- 仍失败时返回明确错误 `upstream 503: …`，不再是
  `upstream stream closed before any chunk carried finish_reason`
  （那是 CPA 在流没有 finish_reason 就结束时的兜底报错）。

## 余额为 0 自动停用

每次拉余额（面板加载、点卡片刷新、10 分钟后台刷新）都会检查 `balance_usd`：

- `<= 0` → 把该账号 auth 文件写成 `"disabled": true`（附 `"auto_disabled": true` 标记），
  CPA 立即停止调度它，面板显示「停用」；
- 余额回到 0 以上 → 只恢复带 `auto_disabled` 标记的账号；手动停用的账号不受影响。

## 环境变量

- `FIFTYX_HTTP_PROXY` — 可选，插件出站代理（默认直连）

## 构建

```bash
make build     # -> 50x.so
make test
```
