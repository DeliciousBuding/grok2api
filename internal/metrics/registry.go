package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Gauge is a sampled point rendered with the counters owned by Registry.
type Gauge struct {
	Name   string
	Help   string
	Labels map[string]string
	Value  float64
}

// Registry stores process-local counters. It deliberately renders Prometheus
// text directly to keep the gateway dependency-light.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*counter
}

type counter struct {
	name   string
	labels map[string]string
	value  uint64
}

var counterHelp = map[string]string{
	"grok2api_attempts_total":           "Upstream attempt count.",
	"grok2api_retries_total":            "Retry count by surface, model, and reason.",
	"grok2api_upstream_responses_total": "Observed upstream response statuses.",
	"grok2api_account_feedback_total":   "Account feedback events by kind.",
	"grok2api_empty_outputs_total":      "Responses that completed with an empty output.",
}

var counterOrder = []string{
	"grok2api_attempts_total",
	"grok2api_retries_total",
	"grok2api_upstream_responses_total",
	"grok2api_account_feedback_total",
	"grok2api_empty_outputs_total",
}

func NewRegistry() *Registry {
	return &Registry{counters: map[string]*counter{}}
}

func (r *Registry) IncAttempt(surface, model string) {
	r.add("grok2api_attempts_total", map[string]string{"surface": surface, "model": model}, 1)
}

func (r *Registry) IncRetry(surface, model, reason string) {
	r.add("grok2api_retries_total", map[string]string{"surface": surface, "model": model, "reason": reason}, 1)
}

func (r *Registry) IncUpstreamStatus(surface, model string, status int) {
	r.add("grok2api_upstream_responses_total", map[string]string{
		"surface": surface,
		"model":   model,
		"status":  strconv.Itoa(status),
	}, 1)
}

func (r *Registry) IncFeedback(kind string) {
	r.add("grok2api_account_feedback_total", map[string]string{"kind": kind}, 1)
}

func (r *Registry) IncEmptyOutput(surface, model string) {
	r.add("grok2api_empty_outputs_total", map[string]string{"surface": surface, "model": model}, 1)
}

func (r *Registry) add(name string, labels map[string]string, delta uint64) {
	if r == nil {
		return
	}
	key := name + "\xff" + labelKey(labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.counters[key]
	if c == nil {
		c = &counter{name: name, labels: copyLabels(labels)}
		r.counters[key] = c
	}
	c.value += delta
}

func (r *Registry) RenderText(gauges []Gauge) string {
	var b strings.Builder
	counters := r.snapshotCounters()
	for _, name := range counterOrder {
		if help := counterHelp[name]; help != "" {
			writeHelpType(&b, name, help, "counter")
		}
		for _, c := range counters {
			if c.name == name {
				writeSample(&b, c.name, c.labels, float64(c.value))
			}
		}
	}

	sort.Slice(gauges, func(i, j int) bool {
		if gauges[i].Name == gauges[j].Name {
			return labelKey(gauges[i].Labels) < labelKey(gauges[j].Labels)
		}
		return gauges[i].Name < gauges[j].Name
	})
	seenGaugeTypes := map[string]bool{}
	for _, g := range gauges {
		if !seenGaugeTypes[g.Name] {
			writeHelpType(&b, g.Name, g.Help, "gauge")
			seenGaugeTypes[g.Name] = true
		}
		writeSample(&b, g.Name, g.Labels, g.Value)
	}
	return b.String()
}

func (r *Registry) UpstreamHealth() string {
	if r == nil {
		return "unknown"
	}
	var ok, bad uint64
	for _, c := range r.snapshotCounters() {
		if c.name != "grok2api_upstream_responses_total" {
			continue
		}
		status, _ := strconv.Atoi(c.labels["status"])
		if status == 429 || status >= 500 {
			bad += c.value
			continue
		}
		if status >= 200 && status < 500 {
			ok += c.value
		}
	}
	if ok == 0 && bad == 0 {
		return "unknown"
	}
	if bad > ok {
		return "degraded"
	}
	return "ok"
}

func (r *Registry) snapshotCounters() []*counter {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*counter, 0, len(r.counters))
	for _, c := range r.counters {
		out = append(out, &counter{name: c.name, labels: copyLabels(c.labels), value: c.value})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name == out[j].name {
			return labelKey(out[i].labels) < labelKey(out[j].labels)
		}
		return out[i].name < out[j].name
	})
	return out
}

func writeHelpType(b *strings.Builder, name, help, typ string) {
	if help != "" {
		fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	}
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
}

func writeSample(b *strings.Builder, name string, labels map[string]string, value float64) {
	fmt.Fprintf(b, "%s%s %g\n", name, formatLabels(labels), value)
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, escapeLabel(labels[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, "\xff")
}

func copyLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	cp := make(map[string]string, len(labels))
	for k, v := range labels {
		cp[k] = v
	}
	return cp
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
