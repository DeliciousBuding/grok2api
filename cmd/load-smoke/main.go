package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type headerList []string

func (h *headerList) String() string {
	return strings.Join(*h, ",")
}

func (h *headerList) Set(v string) error {
	*h = append(*h, v)
	return nil
}

type sample struct {
	status int
	ms     float64
	err    error
}

type runConfig struct {
	Method      string
	Target      string
	Headers     http.Header
	Body        []byte
	Concurrency int
	Duration    time.Duration
	Timeout     time.Duration
}

func main() {
	var headers headerList
	baseURL := flag.String("base-url", "http://127.0.0.1:8000", "gateway base URL")
	path := flag.String("path", "/health", "request path")
	method := flag.String("method", http.MethodGet, "HTTP method")
	bodyPath := flag.String("body", "", "optional request body file")
	concurrency := flag.Int("concurrency", 16, "concurrent workers")
	duration := flag.Duration("duration", 15*time.Second, "test duration")
	timeout := flag.Duration("timeout", 10*time.Second, "per-request timeout")
	maxErrorRate := flag.Float64("max-error-rate", 0.01, "maximum failed request ratio")
	maxP95Ms := flag.Float64("max-p95-ms", 2000, "maximum p95 latency in milliseconds; 0 disables")
	flag.Var(&headers, "header", "HTTP header, repeatable, format 'Name: value'")
	flag.Parse()

	if *concurrency <= 0 {
		fmt.Fprintln(os.Stderr, "concurrency must be > 0")
		os.Exit(2)
	}
	if *duration <= 0 {
		fmt.Fprintln(os.Stderr, "duration must be > 0")
		os.Exit(2)
	}

	body, err := loadBody(*bodyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	reqHeaders := parseHeaders(headers)
	target := strings.TrimRight(*baseURL, "/") + "/" + strings.TrimLeft(*path, "/")

	summary := runSmoke(runConfig{
		Method:      *method,
		Target:      target,
		Headers:     reqHeaders,
		Body:        body,
		Concurrency: *concurrency,
		Duration:    *duration,
		Timeout:     *timeout,
	})
	printSummary(summary, *duration)
	if summary.total == 0 {
		os.Exit(1)
	}
	if summary.errorRate > *maxErrorRate {
		os.Exit(1)
	}
	if *maxP95Ms > 0 && summary.p95 > *maxP95Ms {
		os.Exit(1)
	}
}

func runSmoke(cfg runConfig) summary {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	client := &http.Client{Timeout: cfg.Timeout}
	results := make(chan sample, cfg.Concurrency*4)
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				start := time.Now()
				status, err := doRequest(ctx, client, cfg.Method, cfg.Target, cfg.Headers, cfg.Body)
				results <- sample{status: status, ms: float64(time.Since(start).Microseconds()) / 1000, err: err}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	return summarize(results)
}

func loadBody(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	if strings.HasPrefix(path, "@") {
		path = strings.TrimPrefix(path, "@")
	}
	return os.ReadFile(path)
}

func parseHeaders(headers []string) http.Header {
	out := http.Header{}
	for _, raw := range headers {
		name, value, ok := strings.Cut(raw, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out.Add(name, strings.TrimSpace(value))
	}
	return out
}

func doRequest(ctx context.Context, client *http.Client, method, url string, headers http.Header, body []byte) (int, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, err
	}
	for k, vals := range headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if !statusOK(resp.StatusCode) {
		return resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

type summary struct {
	total      int
	success    int
	failed     int
	errorRate  float64
	p50        float64
	p95        float64
	p99        float64
	statusCode map[int]int
}

func summarize(results <-chan sample) summary {
	s := summary{statusCode: map[int]int{}}
	latencies := []float64{}
	for r := range results {
		s.total++
		if r.status > 0 {
			s.statusCode[r.status]++
		}
		if r.err != nil {
			s.failed++
			continue
		}
		s.success++
		latencies = append(latencies, r.ms)
	}
	if s.total > 0 {
		s.errorRate = float64(s.failed) / float64(s.total)
	}
	s.p50 = percentile(latencies, 0.50)
	s.p95 = percentile(latencies, 0.95)
	s.p99 = percentile(latencies, 0.99)
	return s
}

func printSummary(s summary, duration time.Duration) {
	rps := 0.0
	if duration > 0 {
		rps = float64(s.total) / duration.Seconds()
	}
	fmt.Printf("requests=%d success=%d failed=%d error_rate=%.4f rps=%.2f p50_ms=%.2f p95_ms=%.2f p99_ms=%.2f\n",
		s.total, s.success, s.failed, s.errorRate, rps, s.p50, s.p95, s.p99)
	if len(s.statusCode) > 0 {
		keys := make([]int, 0, len(s.statusCode))
		for k := range s.statusCode {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			fmt.Printf("status_%d=%d\n", k, s.statusCode[k])
		}
	}
}

func percentile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	if q <= 0 {
		return cp[0]
	}
	idx := int(q*float64(len(cp)-1) + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func statusOK(status int) bool {
	return status >= 200 && status < 300
}
