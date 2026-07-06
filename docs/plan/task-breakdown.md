最后更新：2026-07-06 08:19

# Task Breakdown

## Phase 1: Safety Baseline

- [x] Apply configured per-account concurrency to quota and random selection.
- [x] Clamp retry counts to prevent retry storms.
- [x] Add request body-size admission.
- [x] Add token-free basic metrics endpoint.
- [x] Add tests for the above.

## Phase 2: Admission Control

- [x] Add global in-flight limiter.
- [x] Add per-model in-flight limiter.
- [x] Return structured 429 when admission is exhausted.
- [x] Add tests for limiter fairness and release-on-error.

## Phase 3: Observability

- [x] Replace basic metrics string output with a metrics package and stable labels.
- [x] Add counters for attempts, retries, upstream statuses, account feedback kinds, and empty-output events.
- [x] Add readiness endpoint that distinguishes process-up, account-pool-ready, and upstream-degraded states.

## Phase 4: Upstream Robustness

- [x] Add parser tests for empty successful responses and malformed SSE frames.
- [x] Add configurable timeout classes for chat, console, image, video, and admin operations.
- [x] Add retry budget tests across streaming and non-streaming paths.

## Phase 5: Release Hardening

- [x] Add public Docker image workflow for `ghcr.io/deliciousbuding/grok2api`.
- [x] Add sample compose file with resource limits and health checks.
- [x] Add public operations runbook.
- [x] Run `go test ./...` and local Docker build.

## Phase 6: Runtime Resilience

- [x] Add Prometheus request-duration histograms with stable, low-cardinality labels.
- [x] Add configurable upstream stream idle timeout for long-lived SSE paths.
- [x] Add dependency-free load smoke command with error-rate and p95 thresholds.
- [x] Run full verification and update PR/GitHub tracking.

## Phase 7: Admin API Validation

- [x] Add bounded pagination, filters, and metadata to `GET /admin/api/tokens`.
- [x] Reject invalid admin list query values with structured 400 responses.
- [x] Reject invalid pool replacement names and malformed pool payloads.
- [x] Add focused admin API regression tests and public API documentation.

## Phase 8: Admin Batch/Cache Guardrails

- [x] Reject invalid batch `concurrency` and `enabled` query values before starting work.
- [x] Reject empty batch cache-clear token lists before service-availability checks.
- [x] Reject invalid cache `type` / `cache_type` values across cache list and mutation endpoints.
- [x] Add bounded cache-list pagination metadata and negative-case regression tests.
