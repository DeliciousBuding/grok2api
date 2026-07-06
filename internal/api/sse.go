package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/DeliciousBuding/grok2api/internal/config"
	"github.com/DeliciousBuding/grok2api/internal/platform"
)

// sseWriter writes Server-Sent Events frames to an HTTP response.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: flusher}
}

// writeData emits "data: <payload>\n\n".
func (s *sseWriter) writeData(payload string) {
	fmt.Fprintf(s.w, "data: %s\n\n", payload)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// writeJSONData emits "data: <json>\n\n" with the JSON-encoded payload.
func (s *sseWriter) writeJSONData(v any) {
	b, _ := json.Marshal(v)
	s.writeData(string(b))
}

// writeEvent emits "event: <type>\ndata: <payload>\n\n".
func (s *sseWriter) writeEvent(eventType, payload string) {
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventType, payload)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// writeEventJSON emits "event: <type>\ndata: <json>\n\n".
func (s *sseWriter) writeEventJSON(eventType string, v any) {
	b, _ := json.Marshal(v)
	s.writeEvent(eventType, string(b))
}

// writeComment emits ": <comment>\n\n" (a comment/heartbeat line).
func (s *sseWriter) writeComment(comment string) {
	fmt.Fprintf(s.w, ": %s\n\n", comment)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// writeDone emits the terminal "data: [DONE]\n\n".
func (s *sseWriter) writeDone() {
	s.writeData("[DONE]")
}

// writeOpenAIError emits the OpenAI-style SSE error frame, then [DONE].
func (s *sseWriter) writeOpenAIError(message string, kind string, code string, param string) {
	errObj := map[string]any{"message": message, "type": kind, "code": code}
	if param != "" {
		errObj["param"] = param
	}
	s.writeEvent("error", "")
	// SSE "event:" line is followed by a data: line with the actual payload.
	// Some clients ignore the event line for OpenAI-style errors, so we
	// emit the error object under "data:" as well.
	s.writeJSONData(map[string]any{"error": errObj})
	s.writeDone()
}

// writeAnthropicError emits the Anthropic SSE error frame.
func (s *sseWriter) writeAnthropicError(message string, kind string, code string) {
	errObj := map[string]any{"type": "error", "error": map[string]any{
		"type": kind, "message": message, "code": code,
	}}
	s.writeEvent("error", "")
	s.writeJSONData(errObj)
	s.writeDone()
}

func (s *sseWriter) writeOpenAIAppError(err error) {
	var appErr *platform.AppError
	if errors.As(err, &appErr) {
		s.writeOpenAIError(appErr.Message, string(appErr.Kind), appErr.Code, appErr.Param)
		return
	}
	s.writeOpenAIError(err.Error(), string(platform.ErrServer), "stream_error", "")
}

type idleLineScanner struct {
	scanner *bufio.Scanner
	closer  io.Closer
	idle    time.Duration
	results chan lineScanResult
	doneCh  chan struct{}
	once    sync.Once
	done    bool
}

type lineScanResult struct {
	line string
	ok   bool
	err  error
}

func newIdleLineScanner(scanner *bufio.Scanner, closer io.Closer, idle time.Duration) *idleLineScanner {
	r := &idleLineScanner{scanner: scanner, closer: closer, idle: idle, doneCh: make(chan struct{})}
	if idle > 0 {
		r.results = make(chan lineScanResult, 1)
		go r.scan()
	}
	return r
}

func (r *idleLineScanner) scan() {
	for r.scanner.Scan() {
		if !r.send(lineScanResult{line: r.scanner.Text(), ok: true}) {
			return
		}
	}
	_ = r.send(lineScanResult{err: r.scanner.Err()})
}

func (r *idleLineScanner) send(res lineScanResult) bool {
	select {
	case r.results <- res:
		return true
	case <-r.doneCh:
		return false
	}
}

func (r *idleLineScanner) Next(ctx context.Context) (string, bool, error) {
	if r.done {
		return "", false, nil
	}
	if r.idle <= 0 {
		if r.scanner.Scan() {
			return r.scanner.Text(), true, nil
		}
		r.done = true
		return "", false, r.scanner.Err()
	}
	timer := time.NewTimer(r.idle)
	defer timer.Stop()
	select {
	case res := <-r.results:
		if !res.ok {
			r.done = true
		}
		return res.line, res.ok, res.err
	case <-ctx.Done():
		r.Close()
		r.done = true
		return "", false, ctx.Err()
	case <-timer.C:
		r.Close()
		r.done = true
		return "", false, platform.StreamIdleTimeout(durationSecondsCeil(r.idle))
	}
}

func (r *idleLineScanner) Close() {
	r.once.Do(func() {
		close(r.doneCh)
		if r.closer != nil {
			_ = r.closer.Close()
		}
	})
}

func streamIdleTimeoutDuration() time.Duration {
	n := config.Global().GetInt("timeout.stream_idle_sec", 60)
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

func durationSecondsCeil(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}
