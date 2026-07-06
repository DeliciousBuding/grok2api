package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/DeliciousBuding/grok2api/internal/account"
	"github.com/DeliciousBuding/grok2api/internal/config"
	"github.com/DeliciousBuding/grok2api/internal/grok"
	"github.com/DeliciousBuding/grok2api/internal/metrics"
	"github.com/DeliciousBuding/grok2api/internal/model"
	"github.com/DeliciousBuding/grok2api/internal/platform"
	"github.com/DeliciousBuding/grok2api/internal/storage"
)

// fileIDRE matches a valid local media file ID (UUID-style hex with dashes).
var fileIDRE = regexp.MustCompile(`^[0-9a-fA-F\-]{16,36}$`)

// handleFileImage serves a cached image by file ID (public).
func (s *Server) handleFileImage(c *gin.Context) {
	id := strings.TrimSpace(c.Query("id"))
	if !fileIDRE.MatchString(id) {
		writeAppError(c, platform.ValidationError("Invalid file id", "id"))
		return
	}
	dir, err := storage.ImageFilesDir()
	if err != nil {
		writeAppError(c, platform.UpstreamError("image dir: "+err.Error(), 500, ""))
		return
	}
	for _, ext := range []string{".jpg", ".png"} {
		path := filepath.Join(dir, id+ext)
		if _, err := os.Stat(path); err == nil {
			mime := "image/jpeg"
			if ext == ".png" {
				mime = "image/png"
			}
			c.Header("Content-Type", mime)
			c.File(path)
			return
		}
	}
	writeAppError(c, platform.ValidationErrorCode("Image not found", "id", "file_not_found"))
}

// handleFileVideo serves a cached video by file ID (public).
func (s *Server) handleFileVideo(c *gin.Context) {
	id := strings.TrimSpace(c.Query("id"))
	if !fileIDRE.MatchString(id) {
		writeAppError(c, platform.ValidationError("Invalid file id", "id"))
		return
	}
	dir, err := storage.VideoFilesDir()
	if err != nil {
		writeAppError(c, platform.UpstreamError("video dir: "+err.Error(), 500, ""))
		return
	}
	path := filepath.Join(dir, id+".mp4")
	if _, err := os.Stat(path); err != nil {
		writeAppError(c, platform.ValidationErrorCode("Video not found", "id", "file_not_found"))
		return
	}
	c.Header("Content-Type", "video/mp4")
	c.Header("Content-Disposition", `inline; filename="`+id+`.mp4"`)
	c.File(path)
}

// --- Image generation (standalone) ---

const (
	defaultImageEditMaxFileBytes         = 30 << 20
	defaultFetchImageMaxBytes            = 50 << 20
	defaultFetchImageTimeout             = 30 * time.Second
	defaultFetchImageMaxIdleConnsPerHost = 100
)

func (s *Server) handleImageGenerations(c *gin.Context) {
	var req struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              int    `json:"n,omitempty"`
		Size           string `json:"size,omitempty"`
		ResponseFormat string `json:"response_format,omitempty"`
	}
	if err := readJSON(c, &req); err != nil {
		writeAppError(c, err)
		return
	}
	spec, ok := model.Resolve(req.Model)
	if !ok {
		writeAppError(c, platform.ValidationErrorCode("Model '"+req.Model+"' not found", "model", "model_not_found"))
		return
	}
	if !spec.IsImage() {
		writeAppError(c, platform.ValidationErrorCode("Model '"+req.Model+"' is not an image model", "model", "invalid_model"))
		return
	}
	releaseAdmission, ok := s.acquireModelAdmission(c, req.Model)
	if !ok {
		return
	}
	defer releaseAdmission()

	n := req.N
	if n <= 0 {
		n = 1
	}
	responseFormat := req.ResponseFormat
	if responseFormat == "" {
		responseFormat = "url"
	}

	// WS-based models (grok-imagine-image, grok-imagine-image-pro).
	if grok.IsWSImageModel(spec.ModelName) {
		s.handleWSImageGenerations(c, spec, req.Prompt, n, req.Size, responseFormat)
		return
	}

	// Lite model: chat-based generation with concurrent fan-out.
	maxN := 4
	if n > maxN {
		n = maxN
	}

	prompt := "Drawing: " + req.Prompt
	imageURLs := s.captureLiteImageBatch(c.Request, spec, prompt, n)

	images := make([]generatedImage, 0, len(imageURLs))
	for i := 0; i < n && i < len(imageURLs); i++ {
		images = append(images, generatedImage{url: imageURLs[i]})
	}
	out, err := renderGeneratedImages(c.Request.Context(), responseFormat, images, s.metricsRegistry())
	if err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"created": time.Now().Unix(),
		"data":    out,
	})
}

// handleWSImageGenerations handles image generation via the WS imagine endpoint.
func (s *Server) handleWSImageGenerations(c *gin.Context, spec *model.Spec, prompt string, n int, size, responseFormat string) {
	if n <= 0 {
		n = 1
	}
	maxN := 10
	if n > maxN {
		n = maxN
	}

	aspectRatio := grok.ResolveAspectRatio(size)
	enableNSFW := config.Global().GetBool("features.enable_nsfw", true)
	enablePro := grok.IsProImageModel(spec.ModelName)

	apiToken, _ := c.Get("api_token")
	ssoToken, _ := apiToken.(string)

	// Reserve an account.
	lease, _ := reserveAccount(c.Request.Context(), s.Directory, spec, nil, nil)
	if lease == nil && ssoToken != "" {
		lease = &account.Lease{Token: ssoToken, ModeID: int(spec.ModeId)}
	}
	if lease == nil {
		writeAppError(c, platform.RateLimitError("No available accounts"))
		return
	}
	if s.Directory != nil {
		defer s.Directory.Release(lease)
	}

	stream := grok.NewImagineStream(lease.Token)
	s.metricsRegistry().IncAttempt("image_ws", spec.ModelName)
	events := stream.StreamImages(c.Request.Context(), prompt, aspectRatio, n, enableNSFW, enablePro)

	var images []generatedImage
	for ev := range events {
		switch ev.Type {
		case grok.ImagineEventImage:
			url := ""
			if ev.URL != "" {
				url = grok.ImageBaseURL + strings.TrimPrefix(ev.URL, "/")
			}
			images = append(images, generatedImage{url: url, blob: ev.Blob})
		case grok.ImagineEventError:
			s.writeWSImageGenerationFailure(c, spec.ModelName, lease, platform.UpstreamError(ev.Error, http.StatusBadGateway, ""))
			return
		}
	}
	if n < len(images) {
		images = images[:n]
	}
	out, err := renderGeneratedImages(c.Request.Context(), responseFormat, images, s.metricsRegistry())
	if err != nil {
		s.metricsRegistry().IncUpstreamStatus("image_ws", spec.ModelName, http.StatusBadGateway)
		writeAppError(c, err)
		return
	}
	s.metricsRegistry().IncUpstreamStatus("image_ws", spec.ModelName, http.StatusOK)
	c.JSON(http.StatusOK, gin.H{"created": time.Now().Unix(), "data": out})
}

func (s *Server) writeWSImageGenerationFailure(c *gin.Context, modelName string, lease *account.Lease, err error) {
	status := http.StatusBadGateway
	var appErr *platform.AppError
	if asAppError(err, &appErr) && appErr.Status > 0 {
		status = appErr.Status
	}
	s.metricsRegistry().IncUpstreamStatus("image_ws", modelName, status)
	writeAppError(c, err)
	if lease != nil {
		s.feedbackError(lease.Token, err, lease.ModeID)
	}
}

// handleImageEdits serves the multipart image-edit endpoint.
func (s *Server) handleImageEdits(c *gin.Context) {
	if err := c.Request.ParseMultipartForm(50 << 20); err != nil {
		writeAppError(c, platform.ValidationError("Invalid multipart form: "+err.Error(), "body"))
		return
	}
	modelName := strings.TrimSpace(c.Request.FormValue("model"))
	prompt := strings.TrimSpace(c.Request.FormValue("prompt"))
	if modelName == "" || prompt == "" {
		writeAppError(c, platform.ValidationError("Missing model or prompt", "body"))
		return
	}
	spec, ok := model.Resolve(modelName)
	if !ok {
		writeAppError(c, platform.ValidationErrorCode("Model '"+modelName+"' not found", "model", "model_not_found"))
		return
	}
	if !spec.IsImageEdit() {
		writeAppError(c, platform.ValidationErrorCode("Model '"+modelName+"' is not an image-edit model", "model", "invalid_model"))
		return
	}
	releaseAdmission, ok := s.acquireModelAdmission(c, modelName)
	if !ok {
		return
	}
	defer releaseAdmission()

	responseFormat := strings.TrimSpace(c.Request.FormValue("response_format"))
	if responseFormat == "" {
		responseFormat = "url"
	}
	files := c.Request.MultipartForm.File["image[]"]
	if len(files) == 0 {
		writeAppError(c, platform.ValidationError("No images provided", "image[]"))
		return
	}
	contentBlocks := []map[string]any{{"type": "text", "text": prompt}}
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			continue
		}
		raw, err := readImageEditFileBytes(f)
		f.Close()
		if err != nil {
			writeAppError(c, err)
			return
		}
		if len(raw) == 0 {
			continue
		}
		mime := fh.Header.Get("Content-Type")
		if mime == "" || !strings.HasPrefix(mime, "image/") {
			continue
		}
		b64 := base64.StdEncoding.EncodeToString(raw)
		dataURI := "data:" + mime + ";base64," + b64
		contentBlocks = append(contentBlocks, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": dataURI},
		})
	}
	messages := []map[string]any{{"role": "user", "content": contentBlocks}}
	chatReq := &chatCompletionRequest{Model: modelName, Messages: messages}
	streamOff := false
	chatReq.Stream = &streamOff
	imageURLs := s.captureImageURLs(c.Request, chatReq, spec)
	images := make([]generatedImage, 0, len(imageURLs))
	for _, url := range imageURLs {
		images = append(images, generatedImage{url: url})
	}
	out, err := renderGeneratedImages(c.Request.Context(), responseFormat, images, s.metricsRegistry())
	if err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"created": time.Now().Unix(), "data": out})
}

type generatedImage struct {
	url  string
	blob string
}

func renderGeneratedImages(ctx context.Context, responseFormat string, images []generatedImage, regs ...*metrics.Registry) ([]map[string]any, error) {
	var reg *metrics.Registry
	if len(regs) > 0 {
		reg = regs[0]
	}
	if len(images) == 0 {
		return nil, platform.UpstreamError("no generated images returned", http.StatusBadGateway, "")
	}
	out := make([]map[string]any, 0, len(images))
	for _, img := range images {
		if responseFormat == "b64_json" {
			if img.blob != "" {
				out = append(out, map[string]any{"b64_json": img.blob})
				continue
			}
			if img.url == "" {
				continue
			}
			b64, err := fetchImageBase64(ctx, img.url)
			if err != nil {
				reg.IncAssetFetch(imageFetchMetricKind(err))
				return nil, platform.UpstreamError("image fetch failed: "+err.Error(), http.StatusBadGateway, "")
			}
			reg.IncAssetFetch("success")
			out = append(out, map[string]any{"b64_json": b64})
			continue
		}
		if img.url == "" {
			continue
		}
		out = append(out, map[string]any{"url": img.url})
	}
	if len(out) == 0 {
		return nil, platform.UpstreamError("no generated images returned", http.StatusBadGateway, "")
	}
	return out, nil
}

func readImageEditFileBytes(r io.Reader) ([]byte, error) {
	limit := config.Global().GetInt("asset.max_inline_image_bytes", defaultImageEditMaxFileBytes)
	if limit <= 0 {
		limit = defaultImageEditMaxFileBytes
	}
	raw, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, platform.ValidationError("Invalid image file: "+err.Error(), "image[]")
	}
	if len(raw) > limit {
		return nil, platform.ValidationErrorCode(
			fmt.Sprintf("image file exceeds %d bytes", limit),
			"image[]",
			"image_file_too_large",
		)
	}
	return raw, nil
}

// captureImageURLs runs the non-streaming chat path and extracts any image URLs.
func (s *Server) captureImageURLs(r *http.Request, req *chatCompletionRequest, spec *model.Spec) []string {
	if err := r.Context().Err(); err != nil {
		return nil
	}
	cw := &captureWriter{}

	lease, _ := reserveAccount(r.Context(), s.Directory, spec, nil, req.preferTags())
	if lease == nil {
		return nil
	}
	if s.Directory != nil {
		defer s.Directory.Release(lease)
	}

	emitThink := resolveEmitThink(req.ReasoningEffort)
	message, fileInputs, perr := extractMessages(req.Messages)
	if perr != nil {
		return nil
	}
	temp := 0.8
	if req.Temperature != nil {
		temp = *req.Temperature
	}
	topP := 0.95
	if req.TopP != nil {
		topP = *req.TopP
	}
	s.metricsRegistry().IncAttempt("image", req.Model)
	err := s.runGrokChatOnce(cw, r, lease, spec, message, fileInputs, temp, topP, emitThink, false, req.Model)
	if err != nil {
		if shouldRecordUpstreamStatus(err) {
			s.metricsRegistry().IncUpstreamStatus("image", req.Model, metricStatusCode(err))
		}
		return nil
	}
	s.metricsRegistry().IncUpstreamStatus("image", req.Model, http.StatusOK)

	var obj map[string]any
	if err := json.Unmarshal(cw.body, &obj); err != nil {
		return nil
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return nil
	}
	text, _ := msg["content"].(string)
	return extractImageURLsFromMarkdown(text)
}

// captureLiteImageBatch runs N concurrent chat-based image generation
// requests and returns all collected image URLs.
func (s *Server) captureLiteImageBatch(r *http.Request, spec *model.Spec, prompt string, n int) []string {
	if n <= 0 {
		n = 1
	}
	ctx := r.Context()
	if err := ctx.Err(); err != nil {
		return nil
	}
	results := make([]string, n)
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			break
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if err := ctx.Err(); err != nil {
				return
			}
			msgs := []map[string]any{{"role": "user", "content": prompt}}
			chatReq := &chatCompletionRequest{
				Model:    spec.ModelName,
				Messages: msgs,
			}
			urls := s.captureImageURLs(r, chatReq, spec)
			if len(urls) > 0 {
				results[idx] = urls[0]
			}
		}(i)
	}
	wg.Wait()

	// Collect non-empty results preserving order.
	out := make([]string, 0, n)
	for _, u := range results {
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

// extractImageURLsFromMarkdown returns URLs found in markdown image syntax.
var imageMDRE = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)

func extractImageURLsFromMarkdown(text string) []string {
	matches := imageMDRE.FindAllStringSubmatch(text, -1)
	out := []string{}
	for _, m := range matches {
		if len(m) > 1 {
			out = append(out, m[1])
		}
	}
	return out
}

var fetchImageLimiter = newDynamicConcurrencyLimiter()
var fetchImageTransport http.RoundTripper = newFetchImageTransport()

func newFetchImageTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = defaultFetchImageMaxIdleConnsPerHost
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		tr.MaxIdleConns = tr.MaxIdleConnsPerHost
	}
	return tr
}

type imageFetchStatusError struct {
	status int
}

func (e imageFetchStatusError) Error() string {
	return fmt.Sprintf("image fetch returned %d", e.status)
}

type imageFetchTooLargeError struct {
	limit int
}

func (e imageFetchTooLargeError) Error() string {
	return fmt.Sprintf("image fetch exceeds %d bytes", e.limit)
}

type dynamicConcurrencyLimiter struct {
	mu       sync.Mutex
	inflight int
	changed  chan struct{}
}

func newDynamicConcurrencyLimiter() *dynamicConcurrencyLimiter {
	return &dynamicConcurrencyLimiter{changed: make(chan struct{})}
}

func (l *dynamicConcurrencyLimiter) acquire(ctx context.Context, limit int) (func(), error) {
	if limit <= 0 {
		return func() {}, nil
	}
	for {
		l.mu.Lock()
		if l.inflight < limit {
			l.inflight++
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(l.release)
			}, nil
		}
		changed := l.changed
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (l *dynamicConcurrencyLimiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight > 0 {
		l.inflight--
	}
	close(l.changed)
	l.changed = make(chan struct{})
}

// fetchImageBase64 downloads the image bytes and returns the base64 encoding.
func fetchImageBase64(ctx context.Context, url string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	limit := config.Global().GetInt("asset.max_fetch_image_concurrency", 0)
	release, err := fetchImageLimiter.acquire(ctx, limit)
	if err != nil {
		return "", err
	}
	defer release()

	client := fetchImageHTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", imageFetchStatusError{status: resp.StatusCode}
	}
	body, err := readFetchedImageBytes(resp.Body)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(body), nil
}

func fetchImageHTTPClient() *http.Client {
	return &http.Client{
		Transport: fetchImageTransport,
		Timeout:   fetchImageTimeout(),
	}
}

func fetchImageTimeout() time.Duration {
	timeoutS := config.Global().GetInt("asset.fetch_image_timeout_sec", int(defaultFetchImageTimeout/time.Second))
	if timeoutS <= 0 {
		return defaultFetchImageTimeout
	}
	return time.Duration(timeoutS) * time.Second
}

func readFetchedImageBytes(r io.Reader) ([]byte, error) {
	limit := config.Global().GetInt("asset.max_fetch_image_bytes", defaultFetchImageMaxBytes)
	if limit <= 0 {
		limit = defaultFetchImageMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, imageFetchTooLargeError{limit: limit}
	}
	return body, nil
}

func imageFetchMetricKind(err error) string {
	if err == nil {
		return "success"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var statusErr imageFetchStatusError
	if errors.As(err, &statusErr) {
		return "status"
	}
	var tooLargeErr imageFetchTooLargeError
	if errors.As(err, &tooLargeErr) {
		return "too_large"
	}
	return "request_error"
}

// --- Video jobs (async) ---

type videoJob struct {
	mu          sync.RWMutex
	ID          string `json:"id"`
	Object      string `json:"object"`
	CreatedAt   int64  `json:"created_at"`
	Status      string `json:"status"`
	Model       string `json:"model"`
	Progress    int    `json:"progress"`
	Prompt      string `json:"prompt"`
	Seconds     int    `json:"seconds"`
	Size        string `json:"size"`
	Quality     string `json:"quality"`
	VideoURL    string `json:"video_url,omitempty"`
	CompletedAt *int64 `json:"completed_at,omitempty"`
	Error       *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	contentPath string
}

var (
	videoJobsMap   = map[string]*videoJob{}
	videoJobsMutex sync.Mutex
)

const maxVideoJobs = 1024

// handleVideoCreate queues an async video job.
func (s *Server) handleVideoCreate(c *gin.Context) {
	if err := c.Request.ParseMultipartForm(50 << 20); err != nil {
		writeAppError(c, platform.ValidationError("Invalid multipart form: "+err.Error(), "body"))
		return
	}
	modelName := strings.TrimSpace(c.Request.FormValue("model"))
	prompt := strings.TrimSpace(c.Request.FormValue("prompt"))
	if modelName == "" || prompt == "" {
		writeAppError(c, platform.ValidationError("Missing model or prompt", "body"))
		return
	}
	spec, ok := model.Resolve(modelName)
	if !ok {
		writeAppError(c, platform.ValidationErrorCode("Model '"+modelName+"' not found", "model", "model_not_found"))
		return
	}
	if !spec.IsVideo() {
		writeAppError(c, platform.ValidationErrorCode("Model '"+modelName+"' is not a video model", "model", "invalid_model"))
		return
	}
	seconds := 6
	if v := c.Request.FormValue("seconds"); v != "" {
		if n, err := parseIntStr(v); err == nil {
			if isValidVideoLength(n) {
				seconds = n
			}
		}
	}
	size := c.Request.FormValue("size")
	if size == "" {
		size = "720x1280"
	}
	releaseAdmission, ok := s.acquireModelAdmission(c, modelName)
	if !ok {
		return
	}

	job := &videoJob{
		ID:        "video_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24],
		Object:    "video",
		CreatedAt: time.Now().Unix(),
		Status:    "queued",
		Model:     modelName,
		Progress:  0,
		Prompt:    prompt,
		Seconds:   seconds,
		Size:      size,
		Quality:   "standard",
	}
	registerVideoJob(job)

	go s.runVideoJob(job, prompt, modelName, spec, releaseAdmission)
	c.JSON(http.StatusOK, job.toDict())
}

// handleVideoGet serves GET /v1/videos/:id and /v1/videos/:id/content.
func (s *Server) handleVideoGet(c *gin.Context) {
	id := c.Param("id")
	job := lookupVideoJob(id)
	if job == nil {
		writeAppError(c, platform.ValidationErrorCode("Video '"+id+"' not found", "video_id", "video_not_found"))
		return
	}
	// Check if requesting content
	if strings.HasSuffix(c.Request.URL.Path, "/content") {
		contentPath, ok := job.contentPathIfReady()
		if !ok {
			writeAppError(c, platform.NewAppError("Video content is not ready yet", platform.ErrUpstream, "video_not_ready", http.StatusConflict))
			return
		}
		c.Header("Content-Type", "video/mp4")
		c.Header("Content-Disposition", `inline; filename="`+id+`.mp4"`)
		c.File(contentPath)
		return
	}
	c.JSON(http.StatusOK, job.toDict())
}

// runVideoJob executes the video generation in the background.
func (s *Server) runVideoJob(job *videoJob, prompt, modelName string, spec *model.Spec, releaseAdmission func()) {
	if releaseAdmission != nil {
		defer releaseAdmission()
	}
	timeout := timeoutClassDuration("video", 600)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	job.markInProgress()
	preset := "normal"
	promptWithFlag := prompt + " --mode=normal"

	lease, _ := reserveAccount(ctx, s.Directory, spec, nil, nil)
	if lease == nil {
		job.fail("no available accounts")
		return
	}
	if s.Directory != nil {
		defer s.Directory.Release(lease)
	}

	payload := grok.BuildChatPayload(promptWithFlag, model.ModeId(lease.ModeID), nil, nil, nil, map[string]any{
		"enable_pro": false,
		"preset":     preset,
	})
	body, err := json.Marshal(payload)
	if err != nil {
		s.failVideoJob(job, "encode video payload: "+err.Error())
		return
	}
	s.metricsRegistry().IncAttempt("video", modelName)
	bodyReader, err := s.Transport.PostStream(ctx, grok.Chat, lease.Token, body,
		grok.WithTimeout(timeout),
		grok.WithReferer("https://grok.com/imagine"))
	if err != nil {
		if shouldRecordUpstreamStatus(err) {
			s.metricsRegistry().IncUpstreamStatus("video", modelName, metricStatusCode(err))
		}
		s.failVideoJob(job, "video upstream: "+err.Error())
		return
	}
	defer bodyReader.Close()

	adapter := grok.NewStreamAdapter()
	scanner := bufio.NewScanner(bodyReader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		kind, data := grok.ClassifyLine(line)
		if kind == "done" {
			break
		}
		if kind != "data" {
			continue
		}
		events, _ := adapter.Feed([]byte(data))
		for _, ev := range events {
			if ev.Kind == grok.EventImageProgress {
				if n, err := parseIntStr(ev.Content); err == nil {
					job.setProgress(n)
				}
			}
		}
	}

	if len(adapter.ImageURLs) > 0 {
		job.complete(adapter.ImageURLs[0][0])
		s.metricsRegistry().IncUpstreamStatus("video", modelName, http.StatusOK)
		return
	}
	s.metricsRegistry().IncUpstreamStatus("video", modelName, http.StatusBadGateway)
	s.failVideoJob(job, "no video URL in upstream response")
}

func (s *Server) failVideoJob(job *videoJob, message string) {
	job.fail(message)
}

func (j *videoJob) markInProgress() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = "in_progress"
	j.Progress = 1
}

func (j *videoJob) setProgress(progress int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if progress > j.Progress {
		j.Progress = progress
	}
}

func (j *videoJob) complete(videoURL string) {
	now := time.Now().Unix()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = "completed"
	j.Progress = 100
	j.CompletedAt = &now
	j.VideoURL = videoURL
}

func (j *videoJob) fail(message string) {
	now := time.Now().Unix()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Status = "failed"
	j.CompletedAt = &now
	j.Error = &struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: "video_generation_failed", Message: message}
}

func (j *videoJob) toDict() map[string]any {
	j.mu.RLock()
	defer j.mu.RUnlock()
	m := map[string]any{
		"id": j.ID, "object": j.Object, "created_at": j.CreatedAt,
		"status": j.Status, "model": j.Model, "progress": j.Progress,
		"prompt": j.Prompt, "seconds": fmt.Sprintf("%d", j.Seconds),
		"size": j.Size, "quality": j.Quality,
	}
	if j.VideoURL != "" {
		m["video_url"] = j.VideoURL
	}
	if j.CompletedAt != nil {
		m["completed_at"] = *j.CompletedAt
	}
	if j.Error != nil {
		m["error"] = j.Error
	}
	return m
}

func (j *videoJob) contentPathIfReady() (string, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if j.Status != "completed" || j.contentPath == "" {
		return "", false
	}
	return j.contentPath, true
}

func (j *videoJob) createdAt() int64 {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.CreatedAt
}

func registerVideoJob(job *videoJob) {
	videoJobsMutex.Lock()
	defer videoJobsMutex.Unlock()
	videoJobsMap[job.ID] = job
	pruneVideoJobsLocked(maxVideoJobs)
}

func lookupVideoJob(id string) *videoJob {
	videoJobsMutex.Lock()
	defer videoJobsMutex.Unlock()
	return videoJobsMap[id]
}

func pruneVideoJobsLocked(limit int) {
	for len(videoJobsMap) > limit {
		oldestID := ""
		var oldestCreated int64
		for id, job := range videoJobsMap {
			created := job.createdAt()
			if oldestID == "" || created < oldestCreated || (created == oldestCreated && id < oldestID) {
				oldestID = id
				oldestCreated = created
			}
		}
		delete(videoJobsMap, oldestID)
	}
}

func isValidVideoLength(n int) bool {
	switch n {
	case 6, 10, 12, 16, 20:
		return true
	}
	return false
}

func parseIntStr(s string) (int, error) {
	n := 0
	neg := false
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	if i == len(s) {
		return 0, fmt.Errorf("empty")
	}
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid digit")
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}
