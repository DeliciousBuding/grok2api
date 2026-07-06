package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

type scenarioConfig struct {
	Name         string
	ErrorEvery   int
	ErrorStatus  int
	DelayEvery   int
	Delay        time.Duration
	TimeoutEvery int
	TimeoutDelay time.Duration
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

type verdictConfig struct {
	MaxErrorRate float64
	MaxP95Ms     float64
}

type sample struct {
	status int
	ms     float64
	err    error
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

func main() {
	var headers headerList
	baseURL := flag.String("base-url", "", "optional existing gateway base URL; empty starts a local synthetic target")
	path := flag.String("path", "/health", "request path")
	method := flag.String("method", http.MethodGet, "HTTP method")
	bodyPath := flag.String("body", "", "optional request body file")
	scenario := flag.String("scenario", "mixed", "embedded scenario: steady, latency, errors, timeouts, mixed")
	concurrency := flag.Int("concurrency", 8, "concurrent workers")
	duration := flag.Duration("duration", 10*time.Second, "test duration")
	timeout := flag.Duration("timeout", 2*time.Second, "per-request timeout")
	maxErrorRate := flag.Float64("max-error-rate", 0.20, "maximum failed request ratio")
	maxP95Ms := flag.Float64("max-p95-ms", 2000, "maximum p95 latency in milliseconds; 0 disables")
	errorEvery := flag.Int("error-every", -1, "embedded mode: return error every Nth request; -1 uses scenario default")
	delayEvery := flag.Int("delay-every", -1, "embedded mode: delay every Nth request; -1 uses scenario default")
	delay := flag.Duration("delay", 0, "embedded mode: artificial delay override")
	timeoutEvery := flag.Int("timeout-every", -1, "embedded mode: exceed client timeout every Nth request; -1 uses scenario default")
	errorStatus := flag.Int("error-status", 0, "embedded mode: error status override")
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
	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "timeout must be > 0")
		os.Exit(2)
	}

	body, err := loadBody(*bodyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	targetBase := strings.TrimRight(*baseURL, "/")
	embedded := targetBase == ""
	var server *httptest.Server
	if embedded {
		cfg, err := scenarioDefaults(*scenario, *timeout)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		applyScenarioOverrides(&cfg, *errorEvery, *delayEvery, *delay, *timeoutEvery, *errorStatus)
		server = httptest.NewServer(newScenarioHandler(cfg))
		defer server.Close()
		targetBase = server.URL
		fmt.Printf("mode=embedded scenario=%s error_every=%d delay_every=%d timeout_every=%d\n",
			cfg.Name, cfg.ErrorEvery, cfg.DelayEvery, cfg.TimeoutEvery)
	} else {
		fmt.Printf("mode=target scenario=passive\n")
	}

	run := runConfig{
		Method:      *method,
		Target:      targetBase + "/" + strings.TrimLeft(*path, "/"),
		Headers:     parseHeaders(headers),
		Body:        body,
		Concurrency: *concurrency,
		Duration:    *duration,
		Timeout:     *timeout,
	}
	summary := runSmoke(run)
	printSummary(summary, *duration)
	verdict, reasons := evaluateVerdict(summary, verdictConfig{MaxErrorRate: *maxErrorRate, MaxP95Ms: *maxP95Ms})
	if len(reasons) == 0 {
		fmt.Printf("verdict=%s\n", verdict)
	} else {
		fmt.Printf("verdict=%s reasons=%s\n", verdict, strings.Join(reasons, ";"))
	}
	if verdict != "PASS" {
		os.Exit(1)
	}
}

func scenarioDefaults(name string, requestTimeout time.Duration) (scenarioConfig, error) {
	cfg := scenarioConfig{Name: strings.ToLower(strings.TrimSpace(name)), ErrorStatus: http.StatusServiceUnavailable}
	switch cfg.Name {
	case "steady":
		return cfg, nil
	case "latency":
		cfg.DelayEvery = 2
		cfg.Delay = 150 * time.Millisecond
	case "errors":
		cfg.ErrorEvery = 10
	case "timeouts":
		cfg.TimeoutEvery = 10
		cfg.TimeoutDelay = requestTimeout + 100*time.Millisecond
	case "mixed":
		cfg.ErrorEvery = 10
		cfg.DelayEvery = 3
		cfg.Delay = 150 * time.Millisecond
	default:
		return scenarioConfig{}, fmt.Errorf("unknown scenario %q", name)
	}
	return cfg, nil
}

func applyScenarioOverrides(cfg *scenarioConfig, errorEvery, delayEvery int, delay time.Duration, timeoutEvery int, errorStatus int) {
	if errorEvery >= 0 {
		cfg.ErrorEvery = errorEvery
	}
	if delayEvery >= 0 {
		cfg.DelayEvery = delayEvery
	}
	if delay > 0 {
		cfg.Delay = delay
	}
	if timeoutEvery >= 0 {
		cfg.TimeoutEvery = timeoutEvery
	}
	if errorStatus > 0 {
		cfg.ErrorStatus = errorStatus
	}
	if cfg.TimeoutEvery > 0 && cfg.TimeoutDelay <= 0 {
		cfg.TimeoutDelay = 2 * time.Second
	}
}

func newScenarioHandler(cfg scenarioConfig) http.Handler {
	var n atomic.Uint64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := int(n.Add(1))
		if cfg.TimeoutEvery > 0 && count%cfg.TimeoutEvery == 0 {
			time.Sleep(cfg.TimeoutDelay)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("late\n"))
			return
		}
		if cfg.DelayEvery > 0 && count%cfg.DelayEvery == 0 {
			time.Sleep(cfg.Delay)
		}
		if cfg.ErrorEvery > 0 && count%cfg.ErrorEvery == 0 {
			http.Error(w, "synthetic upstream failure", cfg.ErrorStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
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
				status, err := doRequest(context.Background(), client, cfg.Method, cfg.Target, cfg.Headers, cfg.Body)
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

func evaluateVerdict(s summary, cfg verdictConfig) (string, []string) {
	reasons := []string{}
	if s.total == 0 {
		reasons = append(reasons, "no_requests")
	}
	if s.errorRate > cfg.MaxErrorRate {
		reasons = append(reasons, fmt.Sprintf("error_rate %.4f > %.4f", s.errorRate, cfg.MaxErrorRate))
	}
	if cfg.MaxP95Ms > 0 && s.p95 > cfg.MaxP95Ms {
		reasons = append(reasons, fmt.Sprintf("p95_ms %.2f > %.2f", s.p95, cfg.MaxP95Ms))
	}
	if len(reasons) > 0 {
		return "FAIL", reasons
	}
	return "PASS", nil
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
