最后更新：2026-07-06 08:49

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
- Request-duration histograms expose route-pattern latency without token or path-parameter leakage.
- Streaming chat and console paths enforce a configurable upstream idle timeout.
- `cmd/load-smoke` provides a dependency-free load gate with error-rate and p95 thresholds.
- Admin token listing now has bounded pagination, query validation, and pagination metadata.
- Admin pool replacement rejects invalid pool names and malformed pool payloads instead of silently ignoring them.
- Admin batch endpoints now reject invalid `concurrency` and `enabled` query values before starting work.
- Admin cache-management endpoints now reject invalid cache types, malformed JSON, and oversized cache-list pagination.
- Admin asset listing now has bounded account pagination, query validation, and bounded upstream-list concurrency.
- Destructive asset operations now use specific missing-field errors, and clear-token requires `confirm: true`.
- Mutating admin endpoints now emit sanitized `admin_audit` events with operation, outcome, counts, safe resource metadata, and non-reversible token identifiers.

## Remaining Risks

- External dashboards and alert rules are still deployment-specific.
- Chaos/failure simulation beyond load smoke is still future work.
- Audit log forwarding, retention, and tamper-evidence controls are still deployment-specific.
- Multi-arch GHCR publication still needs a live GitHub Actions run after merge.
