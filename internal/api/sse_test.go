package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/DeliciousBuding/grok2api/internal/platform"
)

func TestIdleLineScannerReturnsStreamIdleTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	scanner := bufio.NewScanner(pr)
	reader := newIdleLineScanner(scanner, pr, 10*time.Millisecond)

	_, ok, err := reader.Next(t.Context())

	if ok {
		t.Fatal("idle scanner should not return a line")
	}
	var appErr *platform.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected app error, got %T %v", err, err)
	}
	if appErr.Code != "stream_idle_timeout" || appErr.Status != 504 {
		t.Fatalf("expected stream idle timeout, got code=%s status=%d", appErr.Code, appErr.Status)
	}
}

func TestIdleLineScannerHonorsCanceledContextWhenIdleDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	scanner := bufio.NewScanner(strings.NewReader("data: still-buffered\n"))
	reader := newIdleLineScanner(scanner, io.NopCloser(strings.NewReader("")), 0)

	line, ok, err := reader.Next(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got line=%q ok=%v err=%v", line, ok, err)
	}
	if ok {
		t.Fatal("canceled scanner should not return buffered lines")
	}
}
