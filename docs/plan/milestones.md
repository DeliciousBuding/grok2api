最后更新：2026-07-06 09:11

# Milestones

| Milestone | Exit Criteria |
|---|---|
| Safety Baseline | Tests prove configured account concurrency, retry clamps, body limit, and metrics safety. |
| Admission Control | Global and per-model limiters protect upstream calls under concurrent load. |
| Observability | Metrics and readiness expose actionable aggregate health without token leakage. |
| Upstream Robustness | Timeout and parser boundaries protect streaming and media flows. |
| Release Hardening | Public Docker build, sample deployment, and runbook are ready. |
| Runtime Resilience | Latency histograms, stream idle timeout, and load smoke tooling are verified. |
| Admin API Validation | Token listing is paginated and invalid admin payloads fail with predictable errors. |
| Admin Batch/Cache Guardrails | Batch and cache-management endpoints reject invalid parameters before executing work. |
| Admin Asset Guardrails | Asset listing is bounded and destructive asset operations require precise validation. |
| Admin Audit Events | Mutating admin operations emit sanitized audit events without raw token or payload leakage. |
| Local Resilience Smoke | Local failure scenarios produce automated verdicts without touching production systems. |
| Public CI Quality Gate | PRs run source, workflow, resilience, and vulnerability gates with least privilege. |
