最后更新：2026-07-06 03:10

# Module Inventory

| Module | Responsibility | S.U.P.E.R Notes | Risk |
|---|---|---|---|
| `main.go` | Process bootstrap, background loops, HTTP server lifecycle | Too much startup orchestration in one file; acceptable for single binary but needs smaller helpers as lifecycle grows. | Medium |
| `internal/account` | Persistent record model, quota windows, selection, feedback, refresh | Core scheduler is compact; policy constants now have focused tests, but load behavior still needs stress coverage. | High |
| `internal/api` | HTTP routes, auth, request parsing, OpenAI/Anthropic compatibility | Handlers are large and mix parsing, retries, upstream calls, and response formatting. | High |
| `internal/grok` | Reverse-engineered Grok protocol, headers, assets, usage, WebSocket image generation | Most upstream-specific fragility lives here; parser boundary tests exist, but protocol drift remains high-risk. | High |
| `internal/config` | TOML defaults, user config, env overrides | Simple and useful; path reload semantics needed tests and was improved. | Medium |
| `internal/storage` | Local media cache | SQLite-backed cache with cleanup logic; needs quota and corruption tests before expansion. | Medium |
| `internal/tlsclient` | fhttp/tls-client adapter | Correctly hides implementation details behind stdlib-like transport. | Medium |
| `internal/metrics` | Process-local counters, histograms, and Prometheus text rendering | Small replaceable registry with stable labels and request-duration histogram support. | Medium |
| `internal/logger` | Logging setup | Minimal. Request metrics now live in `internal/metrics`; structured logs remain future work. | Low |

## Priority Hotspots

1. Account selection must respect configured per-account concurrency in all strategies.
2. Retry settings must be bounded to avoid retry amplification.
3. HTTP ingress and upstream calls need bounded body, admission, retry, and timeout behavior before high-concurrency use.
4. Metrics/readiness must expose aggregate health without leaking tokens.
5. Config reload must be safe and predictable.
