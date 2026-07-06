package grok

import (
	"io"
	"net/http"
	"strings"
	"testing"
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
