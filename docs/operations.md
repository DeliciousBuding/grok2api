最后更新：2026-07-07 00:00

# Operations Runbook

This runbook covers public, single-node operation of `grok2api` with the published GHCR image. Keep real SSO tokens, cookies, account dumps, and private host details outside this repository.

## Release Image

The Docker workflow publishes `ghcr.io/deliciousbuding/grok2api` from the public repository.

- Pull requests build the image without pushing it.
- Pushes to `main` and `v*` tags publish multi-arch images for `linux/amd64`, `linux/arm64`, and `linux/arm/v7`.
- Authentication uses the repository `GITHUB_TOKEN` with `contents: read` and `packages: write`; no custom package PAT is required.
- Published tags include `latest` for the default branch, the Git ref, the short commit SHA, and the version read from `VERSION`.

## CI Quality Gate

The public CI workflow uses least-privilege `contents: read` permissions and does not require secrets. It runs on pull requests, relevant pushes to `main`, and manual dispatch.

Gate coverage:

```bash
go mod verify
go vet ./...
go test -count=1 ./...
go build -trimpath -ldflags="-s -w" -o /tmp/grok2api .
go run ./cmd/resilience-smoke -scenario mixed -duration 1s -concurrency 2 -timeout 500ms -max-error-rate 0.25 -max-p95-ms 1000
go run github.com/rhysd/actionlint/cmd/actionlint@latest -color=false .github/workflows/*.yml
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Keep release workflows separate from this gate: CI verifies source quality and supply-chain posture; Docker and release workflows publish artifacts only on their own triggers.

## First Deploy

```bash
mkdir -p grok2api
cd grok2api
curl -fsSLO https://raw.githubusercontent.com/DeliciousBuding/grok2api/main/deploy/compose.example.yml
cp compose.example.yml compose.yml
docker compose pull
docker compose up -d
```

The sample Compose file creates named volumes for `/app/data` and `/app/logs`. Put runtime configuration in `/app/data/config.toml` through the mounted data volume, not in the image.

## Account Storage

The default account backend is JSONL (`account.storage.backend = "text"`), stored under the data volume. It is simple to inspect and back up, but every mutation rewrites the account file.

For single-node deployments with larger account pools or frequent admin mutations, use SQLite:

```toml
[account.storage]
backend = "sqlite"

[account.sqlite]
path = "/app/data/accounts.sqlite3"
```

SQLite uses a local database with WAL mode, `synchronous=NORMAL`, and a busy timeout. Treat it as a single-active-process backend; do not point multiple running gateway instances at the same SQLite account database. Back up the database together with its `-wal` and `-shm` files if they exist.

`pg+redis`, `postgres+redis`, and `postgresql+redis` are reserved distributed backend names for future multi-instance account-pool coordination. They currently fail at startup with a clear error instead of silently falling back to a local backend.

Storage backend settings, `server.*_timeout_sec` settings, and `server.max_header_bytes` are startup-only. Do not expect `/admin/api/config` changes to move a running process between JSONL, SQLite, a future distributed backend, or a different HTTP server timeout/header-size profile.

For container-only overrides, set `ACCOUNT_STORAGE_BACKEND=sqlite` and optionally `ACCOUNT_SQLITE_PATH=/app/data/accounts.sqlite3`.

## Health Checks

```bash
docker compose ps
curl -fsS http://127.0.0.1:8000/health
curl -fsS http://127.0.0.1:8000/ready
curl -fsS http://127.0.0.1:8000/metrics
```

Expected behavior:

- `/health` returns HTTP 200 when the process is serving.
- `/ready` reports account-pool readiness and observed upstream degradation.
- `/metrics` exposes aggregate counters, gauges, and request-duration histograms without token values.

## Audit Events

Mutating admin requests write one `admin_audit` log event with operation, outcome, method/path/status, counts, pool or media type where relevant, and short non-reversible token identifiers. Audit events are intended for incident review and change tracing; they intentionally omit raw SSO tokens, cookies, Authorization headers, request bodies, local paths, cache file names, tags, and raw asset IDs.

```bash
docker compose logs grok2api | grep admin_audit
```

## Capacity Controls

Tune these before exposing the service to untrusted clients:

```toml
[server]
max_body_bytes = 10485760
read_header_timeout_sec = 30
read_timeout_sec = 0
write_timeout_sec = 0
idle_timeout_sec = 120
shutdown_timeout_sec = 15
max_header_bytes = 1048576

[admission]
global_max_inflight = 64
per_model_max_inflight = 16

[account.selection]
max_inflight = 8

[retry]
max_retries = 1

[timeout]
stream_idle_sec = 60

[asset]
max_download_bytes = 31457280
max_inline_image_bytes = 31457280
max_fetch_image_bytes = 52428800
fetch_image_timeout_sec = 30
max_fetch_image_concurrency = 0

[upstream]
max_response_bytes = 16777216
```

Use lower limits for small account pools. A good starting point is to keep `global_max_inflight` below `account_count * account.selection.max_inflight`.

`server.max_body_bytes` applies to both requests with `Content-Length` and streamed/chunked JSON bodies. Oversized bodies return HTTP 413 with `request_body_too_large`, which should be counted separately from malformed JSON in client dashboards.

`server.read_header_timeout_sec`, `server.read_timeout_sec`, `server.write_timeout_sec`, and `server.idle_timeout_sec` tune the public HTTP server's connection-level deadlines. Keep `server.write_timeout_sec = 0` for long streaming deployments unless an outer proxy enforces stream-safe write deadlines. `server.shutdown_timeout_sec` bounds graceful shutdown after SIGINT/SIGTERM. `server.max_header_bytes` bounds aggregate request-header memory before routing or authentication.

`asset.max_download_bytes` caps remote image/file downloads before they are re-uploaded to the upstream service. Values less than or equal to zero use the built-in 30MiB safety default.

`asset.max_inline_image_bytes` caps each multipart source image submitted to the image-edit endpoint. Oversized files return `image_file_too_large` instead of being truncated.

`asset.max_fetch_image_bytes` caps image bytes downloaded for `response_format=b64_json`. These downloads inherit the client request context, so client cancellation stops the outbound fetch. Non-2xx image responses and oversized images fail instead of returning encoded error pages or truncated images. When a client explicitly asks for `b64_json`, fetch failures return `upstream_error` instead of silently falling back to URL output.

`asset.fetch_image_timeout_sec` caps each remote image download for `response_format=b64_json`. Values less than or equal to zero use the built-in 30 second safety default.

`asset.max_fetch_image_concurrency` caps simultaneous remote image downloads for `response_format=b64_json`. Set it when many image results can be converted in parallel and the process needs a predictable outbound-download ceiling; queued fetches still honor client cancellation. `0` preserves the historical unlimited behavior.

Remote image downloads for `response_format=b64_json` reuse a shared HTTP transport for connection pooling while still applying the current per-request timeout and cancellation context to each fetch. The transport keeps an explicit per-host idle pool so repeated image downloads to the same upstream host are not limited by Go's small implicit default.

Remote image downloads only accept absolute HTTP(S) URLs and reject direct localhost, private, link-local, multicast, or unspecified IP destinations before dialing. Redirect targets go through the same validation so generated-image fetches cannot be bounced into local service endpoints. DNS-resolved private targets should still be blocked with deployment egress policy if that threat matters for the environment.

`/metrics` also reports `grok2api_asset_fetch_total{kind="..."}` for `response_format=b64_json` remote image downloads. Kinds are intentionally low-cardinality and URL-free: `success`, `status`, `too_large`, `blocked`, `timeout`, `canceled`, and `request_error`. Use this counter to separate upstream image URL failures from gateway admission, account-pool, and model-response errors.

Chat-routed lite image generation also checks the client request context before starting fan-out workers, so already-canceled requests do not reserve accounts or start upstream image attempts.

WebSocket image generation inherits the client request context as well. Client cancellation stops pending websocket dials and closes active imagine streams instead of waiting for the upstream read deadline.

`upstream.max_response_bytes` caps non-streaming JSON and gRPC-web response bodies read into memory. SSE streaming responses are governed by request timeouts and stream idle timeouts instead.

Non-2xx upstream responses use a small bounded body sample for diagnostics rather than the full success-response cap. This keeps error handling predictable when an upstream service returns a large HTML or JSON error body.

Request-path config reload checks are throttled to avoid filesystem stat and TOML parsing amplification under load. `POST /admin/api/config` still persists changes and forces an immediate reload; if an externally edited config file is temporarily invalid, the previous in-memory snapshot remains in use until a valid reload succeeds.

Admin batch endpoints use fixed worker pools bounded by the `concurrency` query parameter. This bounds goroutine growth for large token lists; tune `concurrency` for upstream pressure, not for request body size.

Admin token imports start quota refresh work through a small bounded background task gate. If the gate is saturated, the import still records the tokens and logs a count-only skip event rather than spawning unbounded detached refresh goroutines.

Successful request feedback also uses the same small bounded background task gate before starting asynchronous quota refresh. If the gate is saturated, the request feedback still updates the in-memory account state and skips the detached refresh instead of spawning unbounded goroutines.

Unauthorized failure feedback persists account-failure state with a bounded 5 second repository deadline, so a slow account backend cannot hold the request path indefinitely.

`GET /admin/api/storage` reports the active account storage backend (`jsonl` or `sqlite`) so operators can verify the startup configuration before importing or replacing large account pools.

Use account tags for soft workload routing. Add tags through the admin token APIs, then send `grok2api_prefer_tags` on `/v1/chat/completions` or `/v1/responses` requests. The selector prefers accounts that contain all requested tags, but falls back to the normal candidate set when none are available; use separate API keys, admission limits, or deployments when strict tenant isolation is required.

Quota refresh requests deduplicate repeated tokens in one batch and coalesce concurrent refreshes for the same token inside one process. This reduces success-feedback and admin-refresh fan-out under high concurrency; it is not a distributed lock across multiple running gateway instances.

Batch quota refresh stops scheduling remaining tokens after its context is canceled. This keeps admin-triggered and feedback-triggered refresh work from continuing to call upstream services after the parent request, timeout, or background deadline has expired.

When any mode-level quota refresh observes an upstream invalid-credentials response, the refresh cancels the sibling mode requests and returns the credential error promptly. This prevents one expired account from waiting on unrelated per-mode request timeouts before it can be marked expired.

Scheduled quota refresh scans account pages in stable token order before applying mutations, so account pools larger than one backend page and accounts expiring during a refresh pass are not skipped.

Async video job status is in-memory and bounded to the most recent 1024 jobs. Poll clients should persist any returned `video_url` they need, because older job IDs are pruned once the registry reaches that limit.

## Load Smoke Test

The repository includes a dependency-free load smoke command. The default target is `/health`, so it is safe to run before adding account credentials.

```bash
go run ./cmd/load-smoke \
  -base-url http://127.0.0.1:8000 \
  -path /health \
  -concurrency 16 \
  -duration 30s \
  -max-error-rate 0.01 \
  -max-p95-ms 500
```

For authenticated API checks, pass headers and a request body file:

```bash
go run ./cmd/load-smoke \
  -base-url http://127.0.0.1:8000 \
  -method POST \
  -path /v1/chat/completions \
  -header "Authorization: Bearer <api-key>" \
  -body @chat-request.json \
  -concurrency 8 \
  -duration 30s
```

Do not store real API keys, SSO tokens, or cookie values in this repository.

## Resilience Smoke Test

`cmd/resilience-smoke` provides a local, dependency-free failure simulation gate. With no `-base-url`, it starts an embedded synthetic target and injects deterministic latency, 5xx responses, or timeouts. This keeps blast radius local while still verifying the smoke tooling, thresholds, and alert-worthy output shape.

```bash
go run ./cmd/resilience-smoke \
  -scenario mixed \
  -duration 10s \
  -concurrency 8 \
  -timeout 2s \
  -max-error-rate 0.20 \
  -max-p95-ms 2000
```

Supported scenarios:

| Scenario | Purpose |
|---|---|
| `steady` | Baseline with no injected faults. |
| `latency` | Adds deterministic latency to part of the request stream. |
| `errors` | Returns deterministic 503 responses. |
| `timeouts` | Exceeds the client timeout on deterministic requests. |
| `mixed` | Combines deterministic latency and 503 responses. |

To run a passive gate against a local or staging gateway, provide `-base-url` and the target path:

```bash
go run ./cmd/resilience-smoke \
  -base-url http://127.0.0.1:8000 \
  -path /ready \
  -scenario steady \
  -duration 10s \
  -max-error-rate 0.05
```

The command prints request counts, error rate, RPS, p50/p95/p99, status-code distribution, and `verdict=PASS` or `verdict=FAIL`. Do not use production credentials in command-line headers or body files.

## Updates

```bash
docker compose pull
docker compose up -d
docker compose ps
curl -fsS http://127.0.0.1:8000/ready
```

For controlled rollouts, pin the image tag in `compose.yml` to a version or short SHA instead of `latest`.

## Backup

Back up the data volume before upgrades or bulk token changes.

```bash
docker run --rm \
  -v grok2api-data:/data:ro \
  -v "$PWD:/backup" \
  alpine:3.21 \
  tar czf /backup/grok2api-data-backup.tgz -C /data .
```

The important runtime files are account storage, user config, and local media cache under `/app/data`. For SQLite account storage, include `accounts.sqlite3`, `accounts.sqlite3-wal`, and `accounts.sqlite3-shm` when present.

## Rollback

1. Pin the previous known-good image tag in `compose.yml`.
2. Run `docker compose pull && docker compose up -d`.
3. Check `/health`, `/ready`, and `/metrics`.
4. Restore the data-volume backup only if the runtime data itself was changed incorrectly.

## Troubleshooting

| Symptom | Check | Likely Action |
|---|---|---|
| Container restarts immediately | `docker compose logs --tail=100 grok2api` | Check `/app/data/config.toml` syntax and data volume permissions. |
| `/ready` is not ready | `curl -fsS http://127.0.0.1:8000/ready` | Add valid accounts or inspect upstream status metrics. |
| Many 429 responses | `/metrics` admission counters | Increase account pool capacity or lower client concurrency. |
| Upstream 403 responses | upstream status metrics and clearance config | Refresh browser cookies and verify proxy egress consistency. |
| Disk growth | `docker system df`, log volume size | Rotate logs, back up data, and remove old images deliberately. |
