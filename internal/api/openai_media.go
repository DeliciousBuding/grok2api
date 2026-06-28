package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
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

	"github.com/aurora-develop/grok2api/internal/grok"
	"github.com/aurora-develop/grok2api/internal/model"
	"github.com/aurora-develop/grok2api/internal/platform"
	"github.com/aurora-develop/grok2api/internal/storage"
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
	n := req.N
	if n <= 0 {
		n = 1
	}
	maxN := 10
	if spec.ModelName == "grok-imagine-image-lite" {
		maxN = 4
	}
	if n > maxN {
		n = maxN
	}
	responseFormat := req.ResponseFormat
	if responseFormat == "" {
		responseFormat = "url"
	}

	messages := []map[string]any{{"role": "user", "content": "Drawing: " + req.Prompt}}
	chatReq := &chatCompletionRequest{
		Model:    req.Model,
		Messages: messages,
	}
	streamOff := false
	chatReq.Stream = &streamOff

	imageURLs := s.captureImageURLs(c.Request, chatReq, spec)
	if len(imageURLs) < n {
		// Pad with what we have.
	}
	out := []map[string]any{}
	for i := 0; i < n && i < len(imageURLs); i++ {
		url := imageURLs[i]
		if responseFormat == "b64_json" {
			b64, err := fetchImageBase64(url)
			if err == nil {
				out = append(out, map[string]any{"b64_json": b64})
				continue
			}
		}
		out = append(out, map[string]any{"url": url})
	}
	c.JSON(http.StatusOK, gin.H{
		"created": time.Now().Unix(),
		"data":    out,
	})
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
		raw, _ := io.ReadAll(io.LimitReader(f, 30<<20))
		f.Close()
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
	out := []map[string]any{}
	for _, url := range imageURLs {
		if responseFormat == "b64_json" {
			b64, err := fetchImageBase64(url)
			if err == nil {
				out = append(out, map[string]any{"b64_json": b64})
				continue
			}
		}
		out = append(out, map[string]any{"url": url})
	}
	c.JSON(http.StatusOK, gin.H{"created": time.Now().Unix(), "data": out})
}

// captureImageURLs runs the non-streaming chat path and extracts any image URLs.
func (s *Server) captureImageURLs(r *http.Request, req *chatCompletionRequest, spec *model.Spec) []string {
	cw := &captureWriter{}

	lease, _ := reserveAccount(r.Context(), s.Directory, spec, nil)
	if lease == nil {
		return nil
	}
	defer s.Directory.Release(lease)

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
	err := s.runGrokChatOnce(cw, r, lease, spec, message, fileInputs, temp, topP, emitThink, false, req.Model)
	if err != nil {
		return nil
	}

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

// fetchImageBase64 downloads the image bytes and returns the base64 encoding.
func fetchImageBase64(url string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(body), nil
}

// --- Video jobs (async) ---

type videoJob struct {
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

	go s.runVideoJob(job, prompt, modelName, spec)
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
		if job.Status != "completed" || job.contentPath == "" {
			writeAppError(c, platform.NewAppError("Video content is not ready yet", platform.ErrUpstream, "video_not_ready", http.StatusConflict))
			return
		}
		c.Header("Content-Type", "video/mp4")
		c.Header("Content-Disposition", `inline; filename="`+id+`.mp4"`)
		c.File(job.contentPath)
		return
	}
	c.JSON(http.StatusOK, job.toDict())
}

// runVideoJob executes the video generation in the background.
func (s *Server) runVideoJob(job *videoJob, prompt, modelName string, spec *model.Spec) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	job.Status = "in_progress"
	job.Progress = 1
	preset := "normal"
	promptWithFlag := prompt + " --mode=normal"

	lease, _ := reserveAccount(ctx, s.Directory, spec, nil)
	if lease == nil {
		now := time.Now().Unix()
		job.Status = "failed"
		job.CompletedAt = &now
		job.Error = &struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{Code: "video_generation_failed", Message: "no available accounts"}
		return
	}
	defer s.Directory.Release(lease)

	payload := grok.BuildChatPayload(promptWithFlag, model.ModeId(lease.ModeID), nil, nil, nil, map[string]any{
		"enable_pro": false,
		"preset":     preset,
	})
	body, err := json.Marshal(payload)
	if err != nil {
		s.failVideoJob(job, "encode video payload: "+err.Error())
		return
	}
	bodyReader, err := s.Transport.PostStream(ctx, grok.Chat, lease.Token, body,
		grok.WithReferer("https://grok.com/imagine"))
	if err != nil {
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
				if n, err := parseIntStr(ev.Content); err == nil && n > job.Progress {
					job.Progress = n
				}
			}
		}
	}

	if len(adapter.ImageURLs) > 0 {
		now := time.Now().Unix()
		job.Status = "completed"
		job.Progress = 100
		job.CompletedAt = &now
		job.VideoURL = adapter.ImageURLs[0][0]
		return
	}
	s.failVideoJob(job, "no video URL in upstream response")
}

func (s *Server) failVideoJob(job *videoJob, message string) {
	now := time.Now().Unix()
	job.Status = "failed"
	job.CompletedAt = &now
	job.Error = &struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: "video_generation_failed", Message: message}
}

func (j *videoJob) toDict() map[string]any {
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

func registerVideoJob(job *videoJob) {
	videoJobsMutex.Lock()
	defer videoJobsMutex.Unlock()
	videoJobsMap[job.ID] = job
}

func lookupVideoJob(id string) *videoJob {
	videoJobsMutex.Lock()
	defer videoJobsMutex.Unlock()
	return videoJobsMap[id]
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
