package api

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DeliciousBuding/grok2api/internal/account"
)

func TestRequestSizeMiddlewareRejectsOversizedBody(t *testing.T) {
	t.Setenv("GROK_APP_API_KEY", "")
	loadTestConfig(t, "[server]\nmax_body_bytes = 8\n")

	r := NewServer(nil, nil, nil, nil, nil).Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"too":"large"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestMetricsEndpointDoesNotExposeTokens(t *testing.T) {
	loadTestConfig(t, "")

	r := NewServer(nil, nil, nil, nil, nil).Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sso") || strings.Contains(w.Body.String(), "tok-") {
		t.Fatalf("metrics should not expose token-like values: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "grok2api_build_info") {
		t.Fatalf("metrics should include build info, got %s", w.Body.String())
	}
}

func TestMetricsEndpointIncludesOperationalCounters(t *testing.T) {
	loadTestConfig(t, "")
	server := NewServer(nil, nil, nil, nil, nil)
	server.Metrics.IncAttempt("chat", "grok-4.20-fast")
	server.Metrics.IncRetry("chat", "grok-4.20-fast", "429")
	server.Metrics.IncUpstreamStatus("chat", "grok-4.20-fast", 429)
	server.Metrics.IncFeedback("rate_limited")
	server.Metrics.IncEmptyOutput("responses", "grok-4.20-fast")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	for _, needle := range []string{
		`grok2api_attempts_total{model="grok-4.20-fast",surface="chat"} 1`,
		`grok2api_retries_total{model="grok-4.20-fast",reason="429",surface="chat"} 1`,
		`grok2api_upstream_responses_total{model="grok-4.20-fast",status="429",surface="chat"} 1`,
		`grok2api_account_feedback_total{kind="rate_limited"} 1`,
		`grok2api_empty_outputs_total{model="grok-4.20-fast",surface="responses"} 1`,
	} {
		if !strings.Contains(w.Body.String(), needle) {
			t.Fatalf("expected metrics output to contain %q, got:\n%s", needle, w.Body.String())
		}
	}
}

func TestReadyEndpointReportsNotReadyWithoutAccountPool(t *testing.T) {
	loadTestConfig(t, "")
	server := NewServer(nil, nil, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"process":{"status":"ok"`) {
		t.Fatalf("expected process check in readiness body, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"account_pool":{"active":0`) ||
		!strings.Contains(w.Body.String(), `"status":"not_ready"`) {
		t.Fatalf("expected account pool not_ready check, got %s", w.Body.String())
	}
}

func TestReadyEndpointReportsReadyWithActiveAccount(t *testing.T) {
	loadTestConfig(t, "")
	repo := &snapshotRepo{items: []*account.Record{account.NewRecord("tok-ready")}}
	dir := account.NewDirectory(repo)
	if err := dir.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := NewServer(nil, dir, nil, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"ready"`) {
		t.Fatalf("expected ready status, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"account_pool":{"active":1`) {
		t.Fatalf("expected active account count, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"upstream":{"status":"unknown"`) {
		t.Fatalf("expected upstream unknown before observations, got %s", w.Body.String())
	}
}

func TestGlobalAdmissionRejectsBeforeBodyParsing(t *testing.T) {
	loadTestConfig(t, "[admission]\nglobal_max_inflight = 1\n")
	server := NewServer(nil, nil, nil, nil, nil)
	release, ok := server.Admission.TryAcquire("global", 1)
	if !ok {
		t.Fatal("pre-acquire should pass")
	}
	defer release()

	r := server.Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{`))
	req.Header.Set("Content-Type", "application/json")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admission_control_exhausted") {
		t.Fatalf("expected structured admission error, got %s", w.Body.String())
	}
}

func TestModelAdmissionRejectsBeforeAccountSelection(t *testing.T) {
	loadTestConfig(t, "[admission]\nper_model_max_inflight = 1\n")
	server := NewServer(nil, nil, nil, nil, nil)
	release, ok := server.Admission.TryAcquire("model:grok-4.20-fast", 1)
	if !ok {
		t.Fatal("pre-acquire should pass")
	}
	defer release()

	r := server.Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.20-fast","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model:grok-4.20-fast") {
		t.Fatalf("expected model admission scope in response, got %s", w.Body.String())
	}
}

func TestModelAdmissionReleasesOnValidationError(t *testing.T) {
	loadTestConfig(t, "[admission]\nper_model_max_inflight = 1\n")
	server := NewServer(nil, nil, nil, nil, nil)

	r := server.Router()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.20-fast","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected validation failure, got %d body=%s", w.Code, w.Body.String())
	}
	if got := server.Admission.Snapshot()["model:grok-4.20-fast"]; got != 0 {
		t.Fatalf("expected model admission to release on handler error, got %d", got)
	}
}

func TestModelAdmissionRejectsMediaEndpointsBeforeWork(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		path        string
		model       string
		contentType string
		body        string
		multipart   map[string]string
	}{
		{
			name:        "image generations",
			method:      http.MethodPost,
			path:        "/v1/images/generations",
			model:       "grok-imagine-image",
			contentType: "application/json",
			body:        `{"model":"grok-imagine-image","prompt":"a city","n":1}`,
		},
		{
			name:      "image edits",
			method:    http.MethodPost,
			path:      "/v1/images/edits",
			model:     "grok-imagine-image-edit",
			multipart: map[string]string{"model": "grok-imagine-image-edit", "prompt": "edit it"},
		},
		{
			name:      "video create",
			method:    http.MethodPost,
			path:      "/v1/videos",
			model:     "grok-imagine-video",
			multipart: map[string]string{"model": "grok-imagine-video", "prompt": "a city"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loadTestConfig(t, "[admission]\nper_model_max_inflight = 1\n")
			server := NewServer(nil, nil, nil, nil, nil)
			scope := "model:" + tc.model
			release, ok := server.Admission.TryAcquire(scope, 1)
			if !ok {
				t.Fatal("pre-acquire should pass")
			}
			defer release()

			var req *http.Request
			if tc.multipart != nil {
				body, contentType := makeMultipartBody(t, tc.multipart)
				req = httptest.NewRequest(tc.method, tc.path, body)
				req.Header.Set("Content-Type", contentType)
			} else {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()

			server.Router().ServeHTTP(w, req)

			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("expected 429, got %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), scope) {
				t.Fatalf("expected %s in response, got %s", scope, w.Body.String())
			}
		})
	}
}

func TestVideoAdmissionReleasesWhenBackgroundJobFails(t *testing.T) {
	loadTestConfig(t, "[admission]\nper_model_max_inflight = 1\n")
	server := NewServer(nil, nil, nil, nil, nil)

	body, contentType := makeMultipartBody(t, map[string]string{
		"model":  "grok-imagine-video",
		"prompt": "a city",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", body)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()

	server.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected queued video job, got %d body=%s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for {
		if got := server.Admission.Snapshot()["model:grok-imagine-video"]; got == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected video admission to release after failed background job, snapshot=%v", server.Admission.Snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func makeMultipartBody(t *testing.T, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatalf("write multipart field: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return body, writer.FormDataContentType()
}

type snapshotRepo struct {
	items []*account.Record
}

func (r *snapshotRepo) Initialize(ctx context.Context) error { return nil }
func (r *snapshotRepo) GetRevision(ctx context.Context) (int, error) {
	return 1, nil
}
func (r *snapshotRepo) RuntimeSnapshot(ctx context.Context) (*account.Snapshot, error) {
	return &account.Snapshot{Revision: 1, Items: r.items}, nil
}
func (r *snapshotRepo) ScanChanges(ctx context.Context, since int, limit int) (*account.ChangeSet, error) {
	return &account.ChangeSet{Revision: 1}, nil
}
func (r *snapshotRepo) UpsertAccounts(ctx context.Context, items []account.Upsert) (*account.MutationResult, error) {
	return &account.MutationResult{}, nil
}
func (r *snapshotRepo) PatchAccounts(ctx context.Context, patches []account.Patch) (*account.MutationResult, error) {
	return &account.MutationResult{}, nil
}
func (r *snapshotRepo) DeleteAccounts(ctx context.Context, tokens []string) (*account.MutationResult, error) {
	return &account.MutationResult{}, nil
}
func (r *snapshotRepo) GetAccounts(ctx context.Context, tokens []string) ([]*account.Record, error) {
	return nil, nil
}
func (r *snapshotRepo) ListAccounts(ctx context.Context, query account.ListQuery) (*account.Page, error) {
	return &account.Page{}, nil
}
func (r *snapshotRepo) ReplacePool(ctx context.Context, pool string, upserts []account.Upsert) (*account.MutationResult, error) {
	return &account.MutationResult{}, nil
}
func (r *snapshotRepo) ResetExpiredConsoleWindows(ctx context.Context) (int, error) {
	return 0, nil
}
func (r *snapshotRepo) RecoverConsoleExpiredAccounts(ctx context.Context) (int, error) {
	return 0, nil
}
func (r *snapshotRepo) Close(ctx context.Context) error { return nil }
