最后更新：2026-07-06 08:19

# Project Overview

## Goal

`grok2api` is a public fork of a Go-based Grok API gateway. The target state is an enterprise-grade service with safer account-pool scheduling, bounded resource usage, clear observability, and public-safe documentation.

## Current Stack

- Language: Go 1.26.
- HTTP framework: Gin.
- Storage: JSONL account repository with in-memory directory.
- Upstream protocols: `grok.com` chat, `console.x.ai`, assets, image/video flows, WebSocket image generation.
- Deployment: single static binary, hardened Docker image, GHCR workflow, and sample Compose file.

## Entry Points

- `main.go`: config load, repository bootstrap, background loops, HTTP server lifecycle.
- `internal/api/server.go`: route tree, health/meta/admin/API endpoints.
- `internal/account/directory.go`: runtime account selection and feedback.
- `internal/grok/transport.go`: upstream HTTP transport and proxy handling.

## Baseline Verification

Before changes, `go test ./...` passed with only `internal/grok/statsig` tests. The fork adds regression tests for account concurrency, retry clamping, config path reload, body-size admission, admission control, readiness, public-safe metrics, parser boundaries, timeout classes, stream idle timeouts, request-duration histograms, retry budgets, admin API validation, and admin batch/cache guardrails. Release validation also covers Docker build, Compose config expansion, workflow linting, image metadata, and a dependency-free load smoke command.

## S.U.P.E.R Snapshot

- Single Purpose: medium. Main modules are recognizable, but API handlers still carry orchestration, parsing, retry, and response shaping together.
- Unidirectional: medium. API depends on account and grok; background refresh feeds repository and directory. Config reload is globally mutable.
- Ports over Implementation: medium. Upstream transport and repository have interfaces, and admission/metrics now have small replaceable packages. Handler orchestration still needs narrower ports.
- Environment-Agnostic: medium. Config/env abstractions exist; public fork now avoids private deployment assumptions.
- Replaceable: medium. JSONL repository and TLS transport can be replaced with moderate work; scheduler policy is still embedded in `Directory`.
