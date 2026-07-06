package main

import "testing"

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
