package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestPercentileClampsAndSorts(t *testing.T) {
	values := []float64{300, 100, 200}

	if got := percentile(values, 0.95); got != 300 {
		t.Fatalf("expected p95 to clamp to highest sample, got %v", got)
	}
	if got := percentile(values, 0.50); got != 200 {
		t.Fatalf("expected p50 to sort samples, got %v", got)
	}
}

func TestStatusOKClassifiesOnly2xx(t *testing.T) {
	if !statusOK(204) {
		t.Fatal("204 should be successful")
	}
	if statusOK(429) {
		t.Fatal("429 should not be successful")
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
