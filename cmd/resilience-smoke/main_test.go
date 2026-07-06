package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestScenarioDefaultsMixedInjectsDeterministicFaults(t *testing.T) {
	cfg, err := scenarioDefaults("mixed", 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ErrorEvery = 2
	cfg.DelayEvery = 3
	cfg.Delay = time.Millisecond

	server := httptest.NewServer(newScenarioHandler(cfg))
	defer server.Close()

	statuses := []int{}
	for i := 0; i < 6; i++ {
		resp, err := http.Get(server.URL + "/health")
		if err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, resp.StatusCode)
		_ = resp.Body.Close()
	}

	want := []int{200, 503, 200, 503, 200, 503}
	for i := range want {
		if statuses[i] != want[i] {
			t.Fatalf("request %d: expected status %d, got sequence %v", i+1, want[i], statuses)
		}
	}
}

func TestScenarioDefaultsRejectsUnknownScenario(t *testing.T) {
	if _, err := scenarioDefaults("unknown", time.Second); err == nil {
		t.Fatal("unknown scenario should be rejected")
	}
}

func TestVerdictFailsWhenErrorRateExceedsThreshold(t *testing.T) {
	s := summary{total: 10, success: 8, failed: 2, errorRate: 0.20, p95: 100}

	verdict, reasons := evaluateVerdict(s, verdictConfig{MaxErrorRate: 0.10, MaxP95Ms: 500})

	if verdict != "FAIL" {
		t.Fatalf("expected FAIL verdict, got %s reasons=%v", verdict, reasons)
	}
	if len(reasons) == 0 {
		t.Fatal("expected failure reasons")
	}
}

func TestVerdictPassesWithinThresholds(t *testing.T) {
	s := summary{total: 10, success: 10, failed: 0, errorRate: 0, p95: 120}

	verdict, reasons := evaluateVerdict(s, verdictConfig{MaxErrorRate: 0.01, MaxP95Ms: 500})

	if verdict != "PASS" {
		t.Fatalf("expected PASS verdict, got %s reasons=%v", verdict, reasons)
	}
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

func TestRunSmokeCancelsInFlightRequestsAtDuration(t *testing.T) {
	canceled := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		once.Do(func() { close(canceled) })
	}))
	t.Cleanup(server.Close)

	start := time.Now()
	summary := runSmoke(runConfig{
		Method:      http.MethodGet,
		Target:      server.URL,
		Concurrency: 1,
		Duration:    40 * time.Millisecond,
		Timeout:     time.Second,
	})
	elapsed := time.Since(start)

	if elapsed > 300*time.Millisecond {
		t.Fatalf("expected run to stop near duration, elapsed %s", elapsed)
	}
	select {
	case <-canceled:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected in-flight request to be canceled by run duration")
	}
	if summary.total == 0 {
		t.Fatal("expected canceled request to be recorded")
	}
}
