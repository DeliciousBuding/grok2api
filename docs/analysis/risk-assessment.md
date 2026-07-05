最后更新：2026-07-06 03:18

# Risk Assessment

## Production Risks

- Retry amplification: unbounded or misconfigured retries can multiply upstream pressure during 429/503 incidents.
- Account hot-spotting: per-account inflight caps must apply to quota selection, not only random selection.
- Resource exhaustion: large request bodies, slow clients, and long streaming calls need explicit server-side boundaries.
- Observability gaps: aggregate pool status, in-flight count, and readiness must be visible without exposing tokens.
- Upstream volatility: Grok endpoints and anti-bot headers can change quickly; parser failures need safe errors.

## Current Mitigations Added In This Fork

- Configured `account.selection.max_inflight` now applies to quota selection and random selection.
- Invalid `max_inflight` values reset to a safe default.
- `retry.max_retries` is clamped to `0..5`.
- `server.max_body_bytes` adds ingress body-size admission.
- `/metrics` exposes token-free aggregate gauges.
- `config.SetPaths` now forces reload, making test and runtime path changes predictable.
- Global and per-model admission control now rejects excess in-flight work with structured 429 responses.
- Admission counters release on synchronous handler errors and asynchronous video job completion/failure.
- `/metrics` is backed by a small registry with stable labels for attempts, retries, upstream statuses, account feedback, and empty outputs.
- `/ready` distinguishes process-up, account-pool readiness, and observed upstream degradation without exposing secrets.
- Stream parser tests cover empty successful payloads and malformed SSE frames.
- Chat, console, image, video, and admin operations now use configurable timeout classes.
- Retry budget decisions are test-covered through a shared helper used by streaming and non-streaming paths.
- Docker image build now includes the default config file, runs as a non-root user, and exposes image-level healthcheck metadata.
- Release workflow publishes GHCR images with `GITHUB_TOKEN`, metadata tags, Buildx cache, provenance, and SBOM enabled.
- Public Compose and operations runbook cover health checks, resource limits, update, backup, and rollback procedures.

## Remaining Risks

- Metrics do not yet include latency histograms or external dashboards.
- Streaming handlers still lack per-chunk idle timeout enforcement.
- No load-test harness or chaos/failure simulation yet.
- Admin APIs need more validation and pagination tests.
- Multi-arch GHCR publication still needs a live GitHub Actions run after merge.
