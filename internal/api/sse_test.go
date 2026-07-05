package api

import (
	"bufio"
	"errors"
	"io"
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
