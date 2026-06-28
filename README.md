# grok2api

将 Grok (x.ai) 转换为 OpenAI / Anthropic 兼容的 API 网关。

## 功能特性

- **OpenAI 兼容** — `/v1/chat/completions`、`/v1/images/generations`、`/v1/videos`、`/v1/responses`
- **Anthropic 兼容** — `/v1/messages`，支持流式和非流式
- **多账号池管理** — 支持 basic / super / heavy 三级账号池，自动配额跟踪
- **智能选号** — 配额感知策略（按剩余配额评分）和随机策略，自动故障转移
- **浏览器指纹伪装** — TLS 指纹、HTTP/2 头序、Chrome 客户端提示，规避上游检测
- **代理支持** — 直连 / 单代理 / 代理池，支持 SOCKS4/5
- **Cloudflare 绕过** — 手动 Cookie / FlareSolverr 自动获取
- **本地媒体缓存** — 图片和视频本地缓存，LRU 淘汰
- **管理后台** — 完整的 Token CRUD、配置热更新、批量操作
- **热重载配置** — 修改配置文件即时生效，无需重启
- **多实例部署** — 基于文件锁的 Leader 选举，支持多进程运行

## 快速开始

### 1. 获取 SSO Token

SSO Token 是你的 Grok 账号凭证，用于调用上游 Grok API。

**方法一：浏览器 DevTools（推荐）**

1. 打开 [grok.com](https://grok.com) 并登录你的账号
2. 按 `F12` 打开浏览器开发者工具
3. 切换到 **Application**（应用程序）标签页
4. 左侧找到 **Cookies** → `https://grok.com`
5. 找到名为 `sso` 的 Cookie，复制它的 **Value** 值
6. 这个值就是你的 SSO Token（通常是一串很长的字符）

**方法二：Network 面板抓包**

1. 打开 [grok.com](https://grok.com) 并登录
2. 按 `F12` → **Network**（网络）标签页
3. 在 Grok 页面随便发一条消息
4. 找到 `conversations/new` 请求 → **Headers** → **Cookie**
5. 从 Cookie 字符串中提取 `sso=` 后面的值

> **注意**：每个 SSO Token 对应一个 Grok 账号。Token 过期后需要重新获取。免费账号（basic pool）和付费账号（super/heavy pool）的配额不同。

### 2. 编译运行

```bash
# 编译
go build -o grok2api .

# 运行（默认监听 0.0.0.0:8000）
./grok2api
```

或直接运行：

```bash
go run .
```

### 3. 添加账号

通过管理 API 将 SSO Token 添加到账号池：

```bash
# 添加到 basic 池（免费账号）
curl -X POST http://localhost:8000/admin/api/tokens/add \
  -H "Content-Type: application/json" \
  -d '{"tokens": ["你的sso-token"], "pool": "basic"}'

# 添加到 super 池（付费账号）
curl -X POST http://localhost:8000/admin/api/tokens/add \
  -H "Content-Type: application/json" \
  -d '{"tokens": ["你的sso-token"], "pool": "super"}'
```

> 默认管理密码是 `grok2api`（配置项 `app.app_key`）。如果配置了 `app.api_key`，管理 API 也需要在 Header 中带上 `Authorization: Bearer grok2api`。

### 4. 调用 API

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.20-0309",
    "messages": [{"role": "user", "content": "你好！"}],
    "stream": true
  }'
```

也可以直接用 SSO Token 当 Bearer 调用（无需先加入账号池）：

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer 你的sso-token" \
  -d '{
    "model": "grok-4.20-0309",
    "messages": [{"role": "user", "content": "你好！"}],
    "stream": true
  }'
```

> **鉴权规则**：默认 `api_key` 为空，完全开放。配置 `api_key` 后，请求必须携带匹配的 API Key 或任意 SSO Token。

### 对接 OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8000/v1", api_key="any")

response = client.chat.completions.create(
    model="grok-4.20-0309",
    messages=[{"role": "user", "content": "你好！"}],
)
print(response.choices[0].message.content)
```

### 对接 Anthropic SDK

```python
import anthropic

client = anthropic.Anthropic(base_url="http://localhost:8000", api_key="any")

message = client.messages.create(
    model="grok-4.20-0309",
    max_tokens=4096,
    messages=[{"role": "user", "content": "你好！"}],
)
print(message.content[0].text)
```

## 配置反爬绕过（重要）

Grok 有 Cloudflare + 自研反爬机制。要正常调用，需要从浏览器抓取以下值：

1. 打开 [grok.com](https://grok.com)（已登录），F12 → **Network**
2. 在 Grok 页面随便发一条消息
3. 找到 `conversations/new` 请求 → **Headers**
4. 复制以下值到 `data/config.toml`：

```toml
[proxy.clearance]
mode = "manual"
# 从 Request Headers → Cookie 中复制
cf_cookies = "cf_clearance=完整的cf_clearance值"
# 从 Request Headers → Cookie 中复制 grok_device_id 的值
device_id = "e6189f18-6160-4033-9141-3c30817469c3"
# 从 Request Headers 中复制 x-statsig-id 的值
statsig_id = "Ww5mfysMHqr1EkqEL6FM5Ad..."
```

> 也可以把浏览器 Cookie 头里所有值都放到 `cf_cookies` 字段，程序会自动提取需要的部分。`device_id` 和 `statsig_id` 不配也行，程序会自动生成随机值。

## 配置说明

配置文件采用 TOML 格式，加载优先级：

1. `config.defaults.toml`（内置默认值）
2. `data/config.toml`（用户自定义，覆盖默认值）
3. `GROK_*` 环境变量（最高优先级）

### 主要配置项

```toml
[app]
app_key = "grok2api"           # 管理后台密码
app_url = ""                    # 应用访问地址（图片/视频链接需要）
api_key = ""                    # API 密钥（留空不鉴权，逗号分隔多个）

[features]
stream = true                   # 默认流式响应
thinking = true                 # 输出思考过程
temporary = true                # 临时对话（不保存历史）
auto_chat_mode_fallback = true  # AUTO 模型自动降级到 fast/expert

[proxy.egress]
mode = "direct"                 # direct | single_proxy | proxy_pool
proxy_url = ""                  # 单代理地址

[proxy.clearance]
mode = "none"                   # none | manual | flaresolverr

[account.refresh]
enabled = true                  # true=配额模式；false=随机模式
```

### 环境变量

| 变量 | 说明 | 默认值 |
|---|---|---|
| `SERVER_HOST` | 监听地址 | `0.0.0.0` |
| `SERVER_PORT` | 监听端口 | `8000` |
| `LOG_LEVEL` | 日志级别 | `INFO` |
| `LOG_FILE_ENABLED` | 启用文件日志 | `true` |
| `DATA_DIR` | 数据目录 | `./data` |
| `ACCOUNT_LOCAL_PATH` | 账号存储路径 | `./data/accounts.jsonl` |
| `PROXY_HTTP` | 代理地址（覆盖配置文件） | _(空)_ |

## 账号池与模型

### 账号池

| 池 | 说明 | 配额周期 |
|---|---|---|
| basic | 免费账号 | 24 小时 |
| super | 付费账号 | 2 小时 |
| heavy | 高级账号 | 2 小时 |

### 可用模型

**grok.com 聊天**：`grok-4.20-0309`、`grok-4.20-0309-reasoning`、`grok-4.20-heavy`、`grok-4.20-multi-agent-0309` 等 16 个模型

**Console**：`grok-4.3-console`、`grok-4.3-high`、`grok-4.20-multi-agent-xhigh` 等 13 个模型（通过 console.x.ai，免费额度）

**媒体**：`grok-imagine-image`、`grok-imagine-image-pro`、`grok-imagine-image-edit`、`grok-imagine-video`

完整模型列表见 [API.md](API.md)。

## 管理 API

管理端点使用 `app.app_key` 认证，支持 `Authorization: Bearer` 或 `?app_key=` 参数。

```bash
# 查看系统状态
curl http://localhost:8000/admin/api/status \
  -H "Authorization: Bearer grok2api"

# 查看所有 Token
curl http://localhost:8000/admin/api/tokens \
  -H "Authorization: Bearer grok2api"

# 更新配置
curl -X POST http://localhost:8000/admin/api/config \
  -H "Authorization: Bearer grok2api" \
  -H "Content-Type: application/json" \
  -d '{"key": "features.thinking", "value": "false"}'
```

完整管理 API 文档见 [API.md](API.md)。

## 部署

### Docker

```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o grok2api .

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/grok2api .
COPY config.defaults.toml .
EXPOSE 8000
CMD ["./grok2api"]
```

```bash
docker build -t grok2api .
docker run -p 8000:8000 -v ./data:/app/data grok2api
```

### 多实例

支持多进程部署，Leader 进程负责配额刷新，Follower 进程只做增量同步。基于 `flock` 文件锁自动选举。


## 常见问题

**Q: 如何获取 SSO Token？**
A: 登录 [grok.com](https://grok.com)，按 F12 打开开发者工具 → Application → Cookies → 找到 `sso` 字段复制其值。详见上方「获取 SSO Token」章节。

**Q: 为什么调用返回 403 "Request rejected by anti-bot rules"？**
A: 这是 Grok 的反爬机制。需要配置以下内容：
1. `proxy.clearance.cf_cookies` — 浏览器的 `cf_clearance` Cookie
2. `proxy.clearance.device_id` — 浏览器的 `grok_device_id` Cookie（可选，不配自动生成）
3. `proxy.clearance.statsig_id` — 浏览器的 `x-statsig-id` header 值（可选，不配自动生成随机值）

从浏览器 F12 → Network → 找到 grok.com 请求 → 复制 Cookie 和 Request Headers 中的对应值。

**Q: 如何获取 cf_clearance？**
A: 登录 grok.com，F12 → Network → 刷新页面 → 找到任意 grok.com 请求 → Request Headers → Cookie → 复制 `cf_clearance=...` 的值。有效期通常几小时到一天。

**Q: 如何使用代理？**
A: 在 `config.toml` 中设置 `[proxy.egress]`，支持 `single_proxy` 和 `proxy_pool` 模式，兼容 HTTP/HTTPS/SOCKS4/SOCKS5 协议。

**Q: 支持图片输入吗？**
A: 支持。在 messages 的 content 中使用 `image_url` 类型，支持 URL 和 base64 data URI。

**Q: 多实例怎么部署？**
A: 直接启动多个进程，自动通过文件锁选举 Leader。Leader 负责配额刷新，所有进程都处理 API 请求。

## 致谢

https://github.com/chenyme/grok2api

## 许可

MIT License
