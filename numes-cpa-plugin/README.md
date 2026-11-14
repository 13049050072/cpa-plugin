# numes CPA plugin

Numes (`https://numes.ai`) for CLIProxyAPI.

Auth: `sk-numes-v1-…` API key. Chat: `POST https://api.numes.ai/api/v1/chat/completions`.
OpenAI-compatible, so the CPA executor forwards chat-completions payloads unchanged.

## 免费模型

$0 输入和输出，不需要余额。默认只列这些（`NUMES_ALL_MODELS=1` 可放开全部 44 个）：

```
deepseek/deepseek-v4.1-flash            1M ctx, tools
moonshotai/kimi-k3.1-pro                1M ctx, tools
nvidia/nemotron-3-ultra-550b-a55b       1M ctx, tools
z-ai/glm-5.3                            256K ctx
z-ai/glm-5.3-flash                      1M ctx
uniboshi/kimi-k3-abliterated-v1         1M ctx
dealignai/deepseek-v4.1-flash-uncensored-fp8   1M ctx
dealignai/glm-5.3-uncensored-fp8        1M ctx
dealignai/glm-5.3-flash-uncensored-fp8  1M ctx
```

付费模型（Claude / GPT-6 / Grok …）需要充值；$0 余额调用会返回 402，插件会转成可读错误。

## 构建

```bash
make test
make build          # numes.so
NUMES_TEST_KEY=sk-numes-v1-... make test   # 跑真实调用
```

`.so` 放到 CPA `plugins/`（部署机 Debian 上重新编译，macOS 产物不能直接用）。

config:

```yaml
plugins:
  configs:
    numes:
      enabled: true
```

## 管理面板

CPA 管理端 → 插件资源 → **Numes**:

| 列 | 来源 |
|---|---|
| 今日请求 / 累计请求 | 插件本地计数 |
| 今日 Token / 累计 Token | 响应里的 `usage.total_tokens`(流式取最后一帧) |
| 错误 | 上游非 2xx |
| 状态 | `GET /api/v1/key` 探活 |

计数存在 `/tmp/numes-cpa-stats.json`,按 auth 的 UID 分桶,CPA 重启不清零,UTC 零点翻篇。

面板 API：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/plugins/numes/accounts` | 本地 key 列表 |
| POST | `/plugins/numes/usage` | 本地计数 + key 探活 |
| POST | `/plugins/numes/import` | 导入 `sk-numes-v1-…` |

为什么不用官网的 activity 接口：那个 BFF 只认网页会话 Cookie + `x-numes-csrf`，
而登录页有 Cloudflare Turnstile，纯 HTTP 拿不到会话。所以统计只认插件自己看到的流量。

## 环境变量

| 变量 | 作用 |
|---|---|
| `NUMES_HTTP_PROXY` | 出口代理（默认直连，`api.numes.ai` 国内可直连） |
| `NUMES_ALL_MODELS=1` | 列出付费模型，不只是免费档 |

## 注意

- 免费模型是第三方节点白送容量，会抖：延迟 2s→18s，偶尔整批超时。客户端要带超时和重试。
- `max_tokens` 太小会返回 `content: null`（推理 token 计入上限），建议 ≥300。
- 流式前缀按客户端协议切换：`/v1/chat/completions` 发裸 JSON（CPA 自己加 `data: `），
  `/v1/messages` 等翻译路径发 `data: {json}`。发错会变成 `data: data:` 或整段丢失。
- 上游的 `[DONE]` 不转发，由 CPA 统一收尾。
