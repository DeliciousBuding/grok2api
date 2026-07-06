package metrics

import (
	"strings"
	"testing"
)

func TestRegistryRendersCountersAndGaugesWithStableLabels(t *testing.T) {
	reg := NewRegistry()
	reg.IncAttempt("chat", "grok-4.20-fast")
	reg.IncRetry("chat", "grok-4.20-fast", "429")
	reg.IncUpstreamStatus("chat", "grok-4.20-fast", 429)
	reg.IncFeedback("rate_limited")
	reg.IncEmptyOutput("responses", "grok-4.20-fast")
	reg.IncAssetFetch("timeout")

	out := reg.RenderText([]Gauge{{
		Name:   "grok2api_accounts_total",
		Help:   "Accounts currently loaded in memory.",
		Labels: map[string]string{"pool": "all"},
		Value:  2,
	}})

	want := []string{
		`# TYPE grok2api_attempts_total counter`,
		`grok2api_attempts_total{model="grok-4.20-fast",surface="chat"} 1`,
		`grok2api_retries_total{model="grok-4.20-fast",reason="429",surface="chat"} 1`,
		`grok2api_upstream_responses_total{model="grok-4.20-fast",status="429",surface="chat"} 1`,
		`grok2api_account_feedback_total{kind="rate_limited"} 1`,
		`grok2api_empty_outputs_total{model="grok-4.20-fast",surface="responses"} 1`,
		`grok2api_asset_fetch_total{kind="timeout"} 1`,
		`grok2api_accounts_total{pool="all"} 2`,
	}
	for _, needle := range want {
		if !strings.Contains(out, needle) {
			t.Fatalf("expected metrics output to contain %q, got:\n%s", needle, out)
		}
	}
}

func TestRegistryReportsUpstreamHealth(t *testing.T) {
	reg := NewRegistry()
	if got := reg.UpstreamHealth(); got != "unknown" {
		t.Fatalf("empty upstream health should be unknown, got %q", got)
	}

	reg.IncUpstreamStatus("chat", "grok-4.20-fast", 503)
	if got := reg.UpstreamHealth(); got != "degraded" {
		t.Fatalf("error-only upstream health should be degraded, got %q", got)
	}

	reg.IncUpstreamStatus("chat", "grok-4.20-fast", 200)
	reg.IncUpstreamStatus("chat", "grok-4.20-fast", 200)
	if got := reg.UpstreamHealth(); got != "ok" {
		t.Fatalf("mostly successful upstream health should be ok, got %q", got)
	}
}

func TestRegistryRendersRequestDurationHistogram(t *testing.T) {
	reg := NewRegistry()
	reg.ObserveRequestDuration("POST", "/v1/chat/completions", 200, 0.42)
	reg.ObserveRequestDuration("POST", "/v1/chat/completions", 200, 3.2)

	out := reg.RenderText(nil)

	want := []string{
		`# TYPE grok2api_http_request_duration_seconds histogram`,
		`grok2api_http_request_duration_seconds_bucket{le="0.5",method="POST",path="/v1/chat/completions",status="200"} 1`,
		`grok2api_http_request_duration_seconds_bucket{le="5",method="POST",path="/v1/chat/completions",status="200"} 2`,
		`grok2api_http_request_duration_seconds_bucket{le="+Inf",method="POST",path="/v1/chat/completions",status="200"} 2`,
		`grok2api_http_request_duration_seconds_sum{method="POST",path="/v1/chat/completions",status="200"} 3.62`,
		`grok2api_http_request_duration_seconds_count{method="POST",path="/v1/chat/completions",status="200"} 2`,
	}
	for _, needle := range want {
		if !strings.Contains(out, needle) {
			t.Fatalf("expected metrics output to contain %q, got:\n%s", needle, out)
		}
	}
}
