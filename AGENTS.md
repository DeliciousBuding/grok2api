最后更新：2026-07-06 01:55

# AGENTS

## Scope

This repository is a public, production-hardened fork of `grok2api`.

## Rules

- Do not commit real SSO tokens, API keys, cookies, hostnames, account dumps, or deployment paths.
- Keep examples generic and safe for public GitHub.
- Prefer test-first changes for request routing, account selection, retry behavior, config loading, and persistence.
- Storage backend changes must preserve the `account.Repository` contract and cover startup-only config behavior.
- Run `go test ./...` before claiming a code change is complete.
- Treat `config.defaults.toml`, `README.md`, and `API.md` as public documentation surfaces.

## Architecture Notes

- `internal/account` owns account records, quotas, in-memory selection, and feedback.
- `internal/api` owns OpenAI/Anthropic-compatible HTTP surfaces and admin APIs.
- `internal/grok` owns upstream protocol details for `grok.com`, `console.x.ai`, and media flows.
- `internal/config` owns defaults, user config, and environment overrides.

## Public Safety

Operational notes may mention generic Docker, reverse proxy, and metrics patterns, but must not reference private infrastructure or local machine paths.
