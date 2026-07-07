# grok2api

将 Grok (x.ai) 转换为 OpenAI / Anthropic 兼容的 API 网关。纯 Go 实现，单二进制部署，多架构 Docker 镜像开箱即用。

## 功能特性

- **OpenAI 兼容** — `/v1/chat/completions`、`/v1/images/generations`、`/v1/videos`、`/v1/responses`
- **Anthropic 兼容** — `/v1/messages`，支持流式和非流式
- **多账号池管理** — 支持 basic / super / heavy 三级账号池，自动配额跟踪
- **智能选号** — 配额感知策略（按剩余配额评分）和随机策略，自动故障转移
- **账号标签偏好** — 请求级 `grok2api_prefer_tags` 可优先路由到指定标签账号，匹配不到自动回退
- **浏览器指纹伪装** — TLS 指纹、HTTP/2 头序、Chrome 客户端提示，规避上游检测
- **WebSocket 图像生成** — 通过 `wss://grok.com/ws/imagine/listen` 实时流式生成图像，支持进度回调
- **纯 Go 生成反 bot 头** — `x-statsig-id` 等头由内置算法实时生成，无需浏览器或 JS 运行时
- **代理支持** — 直连 / 单代理，兼容 HTTP/HTTPS/SOCKS4/5
- **Cloudflare 绕过** — 手动 Cookie 注入，`cf_clearance` 自动提取
- **本地媒体缓存** — 图片和视频本地缓存，LRU 淘汰
- **可选账号存储** — 默认 JSONL，单机部署可切换 SQLite；分布式 PG+Redis 后端保留为 fail-fast 配置
- **管理后台** — 完整的 Token CRUD、配置热更新、批量操作
- **热重载配置** — 修改配置文件即时生效，无需重启
- **资源边界与可观测性** — 请求体限制、全局/单模型 admission、`/metrics`、`/ready`
- **安全审计事件** — 管理端变更会记录脱敏 `admin_audit` 事件，Token 仅以短哈希出现
- **韧性烟测工具** — `cmd/load-smoke` 和 `cmd/resilience-smoke` 覆盖负载、延迟、5xx 和 timeout 场景
- **公开 CI 门禁** — PR 自动运行 Go 测试、vet、构建、韧性烟测、actionlint 和 govulncheck
- **多实例部署** — 基于文件锁的 Leader 选举，支持多进程运行
- **多架构 Docker 镜像** — GHCR 自动构建 amd64 / arm64 / armv7

## 快速开始

> **Docker 用户**：可直接 `docker pull ghcr.io/deliciousbuding/grok2api:latest` 一键启动，无需编译。完整 Docker 指引见文末[部署](#部署)章节；下方流程适用于源码运行。

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

Grok 有 Cloudflare + 自研反爬机制。要正常调用，最关键的是从浏览器抓取 `cf_clearance` Cookie：

1. 打开 [grok.com](https://grok.com)（已登录），F12 → **Network**
2. 在 Grok 页面随便发一条消息
3. 找到 `conversations/new` 请求 → **Headers** → **Cookie**
4. 复制 Cookie 字符串到 `data/config.toml`：

```toml
[proxy.clearance]
# 直接粘贴整段 Cookie 头，程序会自动提取 cf_clearance 等字段
cf_cookies = "cf_clearance=...; sso=...; grok_device_id=...; ..."
# 抓取 Cookie 时浏览器使用的 User-Agent（必须一致，否则 cf_clearance 失效）
user_agent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 ..."
```

> **提示**
> - `cf_clearance` 有效期通常几小时到一天，过期后需重新抓取，否则会返回 403。
> - 把整段 Cookie 头都放进 `cf_cookies` 即可，程序会自动提取所需部分。
> - `x-statsig-id` 等反 bot 头由程序用纯 Go 实时生成，启动时会自动生成匹配的 fresh seed/HEX，**无需手动配置**；如需应急覆盖，可抓取 `statsig_seed` / `statsig_hex` 并填入配置，详见下方[抓取 statsig 指纹](#抓取-statsig-指纹)。

### 抓取 statsig 指纹

程序内置纯 Go 生成 `x-statsig-id`，默认会在启动时生成 fresh seed 并计算匹配 HEX，大多数场景无需额外配置。但如果 Grok 调整反 bot 逻辑导致频繁拦截，可以使用仓库提供的浏览器脚本抓取**真实**的 `statsig_seed` 和 `statsig_hex` 作为临时覆盖。

**步骤：**

1. 打开 [grok.com](https://grok.com) 并登录
2. 按 `F12` → **Console**（控制台）
3. 复制 `capture_statsig_pair.js` 的**全部内容**，粘贴到控制台并按 Enter 执行
4. 页面顶部出现绿色提示条后，按 **F5** 刷新页面
5. 在 Grok 页面**发送一条消息**
6. 绿色提示条会显示 `SEED=...` 和 `HEX=...`，例如：

```
✅ DONE

SEED=<48-byte-base64-seed>

HEX =<hex-fingerprint>

── config.toml ──
[proxy.clearance]
statsig_seed = "<48-byte-base64-seed>"
statsig_hex  = "<hex-fingerprint>"
```

7. 将 `SEED` 和 `HEX` 填入 `data/config.toml`：

```toml
[proxy.clearance]
statsig_seed = "<48-byte-base64-seed>"
statsig_hex  = "<hex-fingerprint>"
```

`statsig_seed` 和 `statsig_hex` 必须同时配置或同时留空。`statsig_seed` 必须是可解码为 48 字节的 base64 字符串；`statsig_hex` 只允许十六进制字符且最多 512 字符。无效值会在启动或 `/admin/api/config` 更新时被拒绝，错误信息不会回显原始指纹值。

`proxy.clearance` 下用于生成请求头或 Cookie 的字符串会在启动和 `/admin/api/config` 更新时统一校验。`user_agent` 最多 512 字符；`cf_cookies` 最多 8192 字符；`cf_clearance` 最多 4096 字符；`device_id`、`x_anonuserid`、`x_challenge`、`x_signature`、`x_userid` 和 `statsig_id` 最多 1024 字符。以上字段都不能包含 CR/LF 换行，避免 header 注入和请求头异常放大；校验错误只返回字段名和规则，不回显原始 Cookie 或指纹值。

## 配置说明

配置文件采用 TOML 格式，加载优先级：

1. `config.defaults.toml`（内置默认值）
2. `data/config.toml`（用户自定义，覆盖默认值）
3. `GROK_*` 环境变量（最高优先级）

### 主要配置项

```toml
[app]
app_key = "grok2api"           # 管理后台密码
api_key = ""                    # API 密钥（留空不鉴权，逗号分隔多个）

[logging]
file_level = "INFO"             # 文件日志级别
max_files = 7                   # 日志文件最大保留数

[server]
max_body_bytes = 0              # 请求体最大字节数；0 = 非 multipart 写请求使用内置 10MiB 默认上限；正数有效上限 256MiB
read_header_timeout_sec = 30    # HTTP 请求头读取超时（秒），0 = 禁用
read_timeout_sec = 0            # HTTP 请求体读取超时（秒），0 = 禁用
write_timeout_sec = 0           # HTTP 响应写入超时（秒），长流式响应通常保持 0
idle_timeout_sec = 120          # Keep-alive 空闲连接超时（秒），0 = 禁用
shutdown_timeout_sec = 15       # SIGINT/SIGTERM graceful shutdown 等待时间（秒）
max_header_bytes = 1048576      # HTTP 请求头总大小上限（字节），<=0 使用 Go 默认值

[admission]
global_max_inflight = 0         # 全局写请求并发上限，0 = 不限制，正数有效上限 10000
per_model_max_inflight = 0      # 单模型并发上限，0 = 不限制，正数有效上限 10000

[features]
stream = true                   # 默认流式响应
thinking = true                 # 输出思考过程
temporary = true                # 临时对话（不保存历史）
memory = false                  # 会话记忆
auto_chat_mode_fallback = true  # AUTO 模型自动降级到 fast/expert
custom_instruction = ""         # 全局附加指令（系统提示）

[cache.local]
image_max_mb = 0                # 图片缓存上限（MB），0 = 不限制，正数有效上限 1048576
video_max_mb = 0                # 视频缓存上限（MB），0 = 不限制，正数有效上限 1048576

[proxy.egress]
proxy_url = ""                  # 出站代理（留空直连），HTTP/HTTPS/SOCKS4/5

[proxy.clearance]
cf_cookies = ""                 # 手动模式：浏览器 Cookie 串（含 cf_clearance），最多 8192 字符，不能包含换行
user_agent = "..."              # 需与抓取 Cookie 时的 UA 一致，最多 512 字符，不能包含换行
statsig_seed = ""               # 可选应急覆盖：真实 statsig 种子；留空则启动时自动生成 fresh seed
statsig_hex  = ""               # 可选应急覆盖：真实 statsig HEX 指纹；必须与 statsig_seed 成对配置

[retry]
max_retries = 1                 # 换账号重试最大次数（0 = 不重试，运行时钳制到 0..5）
on_codes = "429,401,503"        # 触发重试的 HTTP 状态码
reset_session_status_codes = [403]  # 触发重建代理 Session 的状态码

[account.refresh]
enabled = true                  # true=配额模式；false=随机模式
basic_interval_sec = 86400      # basic 池刷新间隔（秒）
super_interval_sec = 7200       # super 池刷新间隔（秒）
heavy_interval_sec = 7200       # heavy 池刷新间隔（秒）

[account.selection]
max_inflight = 8                # 单号并发上限，quota/random 选号策略均生效，正数有效上限 256

[account.storage]
backend = "text"                # text/jsonl/local 或 sqlite；pg+redis 当前会 fail-fast

[account.local]
path = ""                       # text/jsonl/local 路径，留空使用 data/accounts.jsonl

[account.sqlite]
path = ""                       # sqlite 路径，留空使用 data/accounts.sqlite3

[account.postgresql]
dsn = ""                        # 预留：未来 pg+redis 后端

[account.redis]
addr = ""                       # 预留：未来 pg+redis 后端

[timeout]
chat_sec = 300                  # 聊天上游超时（秒，正数有效上限 3600）
console_sec = 300               # Console 上游超时（秒，正数有效上限 3600）
image_sec = 300                 # 图像上游超时（秒，正数有效上限 3600）
video_sec = 600                 # 视频上游超时（秒，正数有效上限 3600）
stream_idle_sec = 60            # 流式响应上游单次静默超时（秒），0 = 禁用
admin_sec = 60                  # 管理操作超时（秒，正数有效上限 3600）

[asset]
upload_timeout = 60             # 资源上传超时（秒，正数有效上限 3600）
list_timeout = 60               # 资源列表超时（秒，正数有效上限 3600）
delete_timeout = 60             # 资源删除超时（秒，正数有效上限 3600）
max_download_bytes = 31457280   # 远程资源下载上限（字节，<=0 使用安全默认值，正数有效上限 268435456）
max_inline_image_bytes = 31457280  # 图像编辑源图上限（字节，<=0 使用安全默认值，正数有效上限 268435456）
max_fetch_image_bytes = 52428800   # b64_json 图片抓取上限（字节，<=0 使用安全默认值，正数有效上限 268435456）
fetch_image_timeout_sec = 30       # b64_json 图片抓取超时（秒，<=0 使用安全默认值）
max_fetch_image_concurrency = 0    # b64_json 图片抓取并发上限，0 = 不限制，正数有效上限 256

[upstream]
max_response_bytes = 16777216   # 非流式上游响应上限（字节，<=0 使用安全默认值，正数有效上限 67108864）

[nsfw]
timeout = 60                    # NSFW/TOS/生日设置超时（秒，正数有效上限 3600）
```

> `config.defaults.toml` 内置全部默认值，`data/config.toml` 只需覆盖你想修改的项即可。

### 环境变量

| 变量 | 说明 | 默认值 |
|---|---|---|
| `SERVER_HOST` | 监听地址 | `0.0.0.0` |
| `SERVER_PORT` | 监听端口 | `8000` |
| `LOG_LEVEL` | 日志级别 | `INFO` |
| `LOG_FILE_ENABLED` | 启用文件日志 | `true` |
| `DATA_DIR` | 数据目录 | `./data` |
| `ACCOUNT_STORAGE_BACKEND` | 账号存储后端：`text` 或 `sqlite` | `text` |
| `ACCOUNT_LOCAL_PATH` | JSONL 账号存储路径 | `./data/accounts.jsonl` |
| `ACCOUNT_SQLITE_PATH` | SQLite 账号数据库路径 | `./data/accounts.sqlite3` |
| `PROXY_HTTP` | 代理地址（覆盖配置文件） | _(空)_ |
| `GROK_SECTION_KEY` | 配置覆盖（映射到 `section.key`） | _(空)_ |

> `GROK_*` 环境变量可用于覆盖任意配置项。例如 `GROK_FEATURES_STREAM=false` 等同于 `features.stream = false`。
> `account.storage.*`、`account.local.*`、`account.sqlite.*`、`account.postgresql.*`、`account.redis.*`、`server.*_timeout_sec`、`server.max_header_bytes` 属于启动期配置，不能通过管理 API 热更新；修改后需要重启进程或容器。

### 账号存储后端

默认后端是 `text`，使用 `data/accounts.jsonl`，兼容早期部署和人工备份流程。

单机部署可切换到 SQLite，减少 JSONL 全量重写带来的落盘放大，并使用 WAL / busy timeout 提升本地并发写入的稳定性。SQLite 面向单个活跃进程，不要让多实例同时共享同一个账号数据库文件：

```toml
[account.storage]
backend = "sqlite"

[account.sqlite]
path = "/app/data/accounts.sqlite3"
```

`pg+redis` / `postgres+redis` / `postgresql+redis` 是预留的分布式账号后端名称，用于未来多实例共享账号池。当前版本会在启动时 fail-fast，不会静默回退到 JSONL 或 SQLite。

## 账号池与模型

### 账号池

| 池 | 说明 | 配额周期 |
|---|---|---|
| basic | 免费账号 | 24 小时 |
| super | 付费账号 | 2 小时 |
| heavy | 高级账号 | 2 小时 |

### 可用模型

**grok.com 聊天**：`grok-4.20-0309`、`grok-4.20-0309-reasoning`、`grok-4.20-heavy`、`grok-4.20-multi-agent-0309` 等 16 个模型

**Console**：`grok-4.3-console`、`grok-4.3-high`、`grok-4.20-multi-agent-xhigh`、`grok-4.20-0309-non-reasoning-console`、`grok-build-console` 等 13 个模型（通过 console.x.ai，免费额度）

**媒体**：`grok-imagine-image-lite`、`grok-imagine-image`、`grok-imagine-image-pro`（WebSocket 实时生成）、`grok-imagine-image-edit`、`grok-imagine-video`

完整模型列表见 [API.md](API.md)。

## 管理 API

管理端点使用 `app.app_key` 认证，支持 `Authorization: Bearer` 或 `?app_key=` 参数。

所有管理端写操作会输出 `admin_audit` 日志事件。事件包含操作名、成功/失败、HTTP method/path/status、数量和安全资源标识；不会记录原始 SSO Token、Cookie、Authorization 头、请求体、缓存文件名或本地路径。

```bash
# 查看系统状态
curl http://localhost:8000/admin/api/status \
  -H "Authorization: Bearer grok2api"

# 查看所有 Token
curl http://localhost:8000/admin/api/tokens \
  -H "Authorization: Bearer grok2api"

# 分页查看 Token（默认 page_size=50，最大 1000）
curl "http://localhost:8000/admin/api/tokens?page=1&page_size=50" \
  -H "Authorization: Bearer grok2api"

# 分页查看本地缓存（type=image|video，page_size 最大 1000）
curl "http://localhost:8000/admin/api/cache/list?type=image&page=1&page_size=100" \
  -H "Authorization: Bearer grok2api"

# 分页查看账号资产（按账号分页，concurrency 最大 80）
curl "http://localhost:8000/admin/api/assets?page=1&page_size=50&concurrency=20" \
  -H "Authorization: Bearer grok2api"

# 更新配置
curl -X POST http://localhost:8000/admin/api/config \
  -H "Authorization: Bearer grok2api" \
  -H "Content-Type: application/json" \
  -d '{"key": "features.thinking", "value": "false"}'
```

完整管理 API 文档见 [API.md](API.md)。

## 部署

### Docker（推荐）

镜像已通过 GitHub Actions 自动构建并发布到 GHCR，支持 `linux/amd64`、`linux/arm64`、`linux/arm/v7` 多架构。直接拉取即可使用，无需本地编译。

```bash
# 拉取最新镜像
docker pull ghcr.io/deliciousbuding/grok2api:latest

# 运行容器（挂载 data 目录以持久化配置与账号数据）
docker run -d \
  --name grok2api \
  -p 8000:8000 \
  -v $(pwd)/data:/app/data \
  ghcr.io/deliciousbuding/grok2api:latest
```

启动后访问 `http://localhost:8000`，管理后台密码默认为 `grok2api`。

> **可选环境变量**：`SERVER_PORT`（覆盖监听端口）、`PROXY_HTTP`（覆盖出站代理）、`TZ`（时区，如 `Asia/Shanghai`）。
>
> **指定版本**：`docker pull ghcr.io/deliciousbuding/grok2api:v1.0.3`，或用 commit 短哈希 `ghcr.io/deliciousbuding/grok2api:<sha>` 锁定具体构建。
>
> **发布流程**：`.github/workflows/ci.yml` 负责 PR 质量门禁；`.github/workflows/build_docker.yml` 使用仓库 `GITHUB_TOKEN` 发布 GHCR 镜像，不需要自定义 PAT。

#### Docker Compose

仓库提供了生产风格的单节点示例：[deploy/compose.example.yml](deploy/compose.example.yml)。它包含命名卷、healthcheck、资源限制、日志滚动、只读根文件系统和基础安全限制。

```bash
cp deploy/compose.example.yml compose.yml
docker compose up -d
```

公开运维流程见 [docs/operations.md](docs/operations.md)，包括健康检查、容量控制、`cmd/load-smoke` 负载冒烟、升级、备份和回滚。

#### 本地韧性烟测

`cmd/resilience-smoke` 默认启动本地合成目标，不需要账号或外部服务，可在 CI 或上线前验证错误率和延迟阈值：

```bash
go run ./cmd/resilience-smoke \
  -scenario mixed \
  -duration 10s \
  -concurrency 8 \
  -max-error-rate 0.20 \
  -max-p95-ms 2000
```

支持 `steady`、`latency`、`errors`、`timeouts`、`mixed` 场景；也可用 `-base-url http://127.0.0.1:8000` 指向本地或 staging 网关做被动烟测。

#### 本地自行构建

如需修改源码后自建镜像，可使用仓库自带的 `Dockerfile`：

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
A: 这是 Grok 的反爬机制，通常是 `cf_clearance` 缺失或过期。解决方法：

1. 按上方「配置反爬绕过」重新抓取浏览器 Cookie，填入 `proxy.clearance.cf_cookies`
2. 确保 `proxy.clearance.user_agent` 与抓取时浏览器的 UA 完全一致
3. 若使用代理，确认代理出口 IP 与浏览器 IP 一致（cf_clearance 绑定 IP）

> `x-statsig-id` 等反 bot 头由程序自动生成，无需手动处理。启动时会生成 fresh seed/HEX；如遇频繁拦截，可手动抓取真实值作为覆盖，详见[抓取 statsig 指纹](#抓取-statsig-指纹)。

**Q: 如何获取 cf_clearance？**
A: 登录 grok.com，F12 → Network → 刷新页面 → 找到任意 grok.com 请求 → Request Headers → Cookie → 复制 `cf_clearance=...` 的值。有效期通常几小时到一天。最简单的做法是把整段 Cookie 头都粘进 `cf_cookies`。

**Q: 如何使用代理？**
A: 在 `config.toml` 的 `[proxy.egress]` 中设置 `proxy_url`，兼容 HTTP/HTTPS/SOCKS4/SOCKS5 协议；或用环境变量 `PROXY_HTTP` 覆盖。示例：`proxy_url = "socks5://127.0.0.1:1080"`。

**Q: 支持图片输入吗？**
A: 支持。在 messages 的 content 中使用 `image_url` 类型，支持 URL 和 base64 data URI。

**Q: 多实例怎么部署？**
A: 直接启动多个进程，自动通过文件锁选举 Leader。Leader 负责配额刷新，所有进程都处理 API 请求。Docker 多实例同理，分别 `docker run` 即可（注意挂载各自的 `data` 目录或共享存储）。

## 致谢

本项目在以下开源项目的基础上发展而来，特此致谢：

- [chenyme/grok2api](https://github.com/chenyme/grok2api) — 原始 Python 实现，为本项目的协议兼容与账号管理提供了重要参考。

同时感谢 [LINUX DO 社区](https://linux.do) —— 本项目在此发布，感谢社区用户的反馈与帮助。

> 本项目为独立重写的 Go 实现，与上述项目无附属关系，旨在提供更轻量、高性能的部署体验。

## 许可

MIT License
