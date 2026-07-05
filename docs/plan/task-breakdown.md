最后更新：2026-07-06 03:18

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
