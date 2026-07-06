package grok

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadUpstreamResponseBodyRejectsOversizedBody(t *testing.T) {
	loadGrokTestConfig(t, "[upstream]\nmax_response_bytes = 4\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("12345")),
	}
	_, err := readUpstreamResponseBody(resp)
	if err == nil {
		t.Fatal("expected oversized upstream response to fail")
	}
	if !strings.Contains(err.Error(), "upstream response exceeds") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestReadUpstreamResponseBodyUsesDefaultLimitWhenUnconfigured(t *testing.T) {
	loadGrokTestConfig(t, "[upstream]\nmax_response_bytes = 0\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", defaultUpstreamMaxResponseBytes+1))),
	}
	_, err := readUpstreamResponseBody(resp)
	if err == nil {
		t.Fatal("expected default upstream response limit to reject oversized body")
	}
}

func TestReadUpstreamErrorBodyUsesSmallBoundedSample(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", defaultUpstreamMaxErrorBytes+1))),
	}

	body := readUpstreamErrorBody(resp)
	if len(body) != defaultUpstreamMaxErrorBytes {
		t.Fatalf("expected error body sample size %d, got %d", defaultUpstreamMaxErrorBytes, len(body))
	}
}

func TestTransportReturnsContextCanceledWithoutUpstreamRequest(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)

	tr, err := NewTransport()
	if err != nil {
		t.Fatalf("new transport: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = tr.GetJSON(ctx, upstream.URL, "tok-test")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %T %[1]v", err)
	}
	if requests != 0 {
		t.Fatalf("canceled request should not reach upstream, got %d requests", requests)
	}
}

func TestTransportReturnsDeadlineExceededWithoutUpstreamRequest(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)

	tr, err := NewTransport()
	if err != nil {
		t.Fatalf("new transport: %v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err = tr.GetJSON(ctx, upstream.URL, "tok-test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %T %[1]v", err)
	}
	if requests != 0 {
		t.Fatalf("expired request should not reach upstream, got %d requests", requests)
	}
}
