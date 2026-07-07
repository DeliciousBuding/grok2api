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

	"github.com/DeliciousBuding/grok2api/internal/tlsclient"
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

func TestConfiguredUpstreamMaxResponseBytesClampsMisconfiguredLargeValue(t *testing.T) {
	loadGrokTestConfig(t, "[upstream]\nmax_response_bytes = 1073741824\n")

	if got := configuredUpstreamMaxResponseBytes(); got != maxUpstreamResponseBytes {
		t.Fatalf("expected upstream response byte limit to clamp to %d, got %d", maxUpstreamResponseBytes, got)
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

func TestTransportClosesIdleConnectionsWhenClientResets(t *testing.T) {
	first := &fakeUpstreamClient{body: `{}`}
	second := &fakeUpstreamClient{body: `{}`}
	created := []*fakeUpstreamClient{first, second}
	oldNewClient := newTLSHTTPClient
	newTLSHTTPClient = func(opts ...tlsclient.Option) upstreamHTTPClient {
		if len(created) == 0 {
			t.Fatal("unexpected extra client creation")
		}
		c := created[0]
		created = created[1:]
		return c
	}
	t.Cleanup(func() { newTLSHTTPClient = oldNewClient })

	tr, err := NewTransport()
	if err != nil {
		t.Fatalf("new transport: %v", err)
	}
	if _, err := tr.GetJSON(context.Background(), "https://example.test/one", "tok-test"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	tr.Reset()
	if _, err := tr.GetJSON(context.Background(), "https://example.test/two", "tok-test"); err != nil {
		t.Fatalf("second request: %v", err)
	}

	if first.closeIdleCalls != 1 {
		t.Fatalf("expected first client idle connections to close once, got %d", first.closeIdleCalls)
	}
	if second.closeIdleCalls != 0 {
		t.Fatalf("new active client should not be closed, got %d", second.closeIdleCalls)
	}
}

type fakeUpstreamClient struct {
	body           string
	closeIdleCalls int
}

func (c *fakeUpstreamClient) Do(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(c.body)),
	}, nil
}

func (c *fakeUpstreamClient) CloseIdleConnections() {
	c.closeIdleCalls++
}
