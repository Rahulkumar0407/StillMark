//go:build ignore

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type BenchmarkResult struct {
	Mode        string        `json:"mode"`
	TotalReqs   int           `json:"total_requests"`
	SuccessReqs int           `json:"successful_requests"`
	Duration    time.Duration `json:"duration"`
	RPS         float64       `json:"rps"`
	MinLatency  time.Duration `json:"min_latency"`
	MeanLatency time.Duration `json:"mean_latency"`
	P50Latency  time.Duration `json:"p50_latency"`
	P90Latency  time.Duration `json:"p90_latency"`
	P95Latency  time.Duration `json:"p95_latency"`
	P99Latency  time.Duration `json:"p99_latency"`
	MaxLatency  time.Duration `json:"max_latency"`
}

func startMockServer() (*http.Server, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"id": "chatcmpl-bench",
				"object": "chat.completion",
				"created": 1700000000,
				"model": "gpt-4o",
				"choices": [{"index":0,"message":{"role":"assistant","content":"bench ok"},"finish_reason":"stop"}],
				"usage": {"prompt_tokens":120,"completion_tokens":10,"total_tokens":130,"prompt_tokens_details":{"cached_tokens":0}}
			}`))
		}),
	}

	go func() {
		_ = server.Serve(listener)
	}()

	return server, baseURL, nil
}

func startGateway(workspaceRoot, dataDir string, enablePlugin bool, pluginPath, mockBaseURL string) (*exec.Cmd, func(), error) {
	pluginsList := []map[string]any{}
	if enablePlugin {
		pluginsList = []map[string]any{
			{
				"name":    "cacheguard",
				"type":    "shared_object",
				"path":    pluginPath,
				"enabled": true,
			},
		}
	}

	configContent := map[string]any{
		"$schema":         "https://www.getbifrost.ai/schema",
		"setup_token":     "test-setup-token",
		"source_of_truth": "config.json",
		"client_config": map[string]any{
			"port": "8081",
			"host": "127.0.0.1",
		},
		"plugins": pluginsList,
		"providers": map[string]any{
			"openai": map[string]any{
				"send_back_raw_request": false, // Raw outbound capture disabled for performance testing
				"keys": []map[string]any{
					{
						"name":   "Mock Bench Key",
						"value":  "sk-mock-key-for-bench",
						"models": []string{"gpt-4o"},
					},
				},
				"network_config": map[string]any{
					"base_url": mockBaseURL,
				},
			},
		},
		"governance": map[string]any{
			"virtual_keys": []map[string]any{
				{
					"name":      "Bench Virtual Key",
					"id":        "vk-cacheguard-bench",
					"value":     "sk-bf-cacheguard-vk",
					"is_active": true,
					"provider_configs": []map[string]any{
						{
							"provider":       "openai",
							"allowed_models": []string{"*"},
							"key_ids":        []string{"*"},
							"weight":         1.0,
						},
					},
				},
			},
		},
	}

	configBytes, err := json.MarshalIndent(configContent, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), configBytes, 0644); err != nil {
		return nil, nil, fmt.Errorf("failed to write config: %w", err)
	}

	cmd := exec.Command("go", "run", "main.go",
		"-port", "8081",
		"-host", "127.0.0.1",
		"-app-dir", dataDir,
		"-log-level", "info",
		"-log-style", "pretty",
	)
	cmd.Dir = filepath.Join(workspaceRoot, "transports", "bifrost-http")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var gatewayOut bytes.Buffer
	cmd.Stdout = &gatewayOut
	cmd.Stderr = &gatewayOut

	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GOWORK=") {
			env = append(env, e)
		}
	}
	env = append(env, "GOWORK="+filepath.Join(workspaceRoot, "go.work"))
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start gateway process: %w", err)
	}

	cleanup := func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	}

	client := &http.Client{Timeout: 1 * time.Second}
	startTime := time.Now()
	for time.Since(startTime) < 30*time.Second {
		resp, err := client.Get("http://127.0.0.1:8081/health")
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return cmd, cleanup, nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}

	cleanup()
	return nil, nil, fmt.Errorf("gateway health check timed out on port 8081; logs:\n%s", gatewayOut.String())
}

func buildRequestPayload(withTools bool) []byte {
	m := map[string]any{
		"model": "gpt-4o",
		"messages": []map[string]any{
			{"role": "system", "content": "You are a high-performance gateway tester."},
			{"role": "user", "content": "Benchmark request sample payload for prompt caching test."},
		},
	}

	if withTools {
		m["tools"] = []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "query_metrics",
					"description": "Fetch node performance metrics",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cluster_id": map[string]any{
								"type": "string",
							},
							"options": map[string]any{
								"$ref": "#/$defs/MetricsOptions",
							},
						},
						"required": []string{"cluster_id"},
						"$defs": map[string]any{
							"MetricsOptions": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"interval_seconds": map[string]any{"type": "integer"},
									"detailed":         map[string]any{"type": "boolean"},
								},
							},
						},
					},
				},
			},
		}
	}

	b, _ := json.Marshal(m)
	return b
}

func runBenchmark(name string, totalReqs int, concurrency int, reqBody []byte, headers map[string]string) BenchmarkResult {
	latencies := make([]time.Duration, totalReqs)
	var successCount int64
	var mu sync.Mutex

	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	// Warm-up requests
	for i := 0; i < 30; i++ {
		req, _ := http.NewRequest("POST", "http://127.0.0.1:8081/v1/chat/completions", bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-bf-cacheguard-vk")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if resp, err := client.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}

	// Measured run
	workChan := make(chan int, totalReqs)
	for i := 0; i < totalReqs; i++ {
		workChan <- i
	}
	close(workChan)

	var wg sync.WaitGroup
	benchStart := time.Now()

	for c := 0; c < concurrency; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range workChan {
				req, _ := http.NewRequest("POST", "http://127.0.0.1:8081/v1/chat/completions", bytes.NewReader(reqBody))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer sk-bf-cacheguard-vk")
				for k, v := range headers {
					req.Header.Set(k, v)
				}

				t0 := time.Now()
				resp, err := client.Do(req)
				elapsed := time.Since(t0)

				if err == nil && resp.StatusCode == http.StatusOK {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					mu.Lock()
					latencies[idx] = elapsed
					successCount++
					mu.Unlock()
				} else {
					if resp != nil {
						_ = resp.Body.Close()
					}
					mu.Lock()
					latencies[idx] = elapsed
					mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()
	totalDuration := time.Since(benchStart)

	// Filter valid latencies
	valid := make([]time.Duration, 0, totalReqs)
	var sum time.Duration
	for _, l := range latencies {
		if l > 0 {
			valid = append(valid, l)
			sum += l
		}
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i] < valid[j] })

	res := BenchmarkResult{
		Mode:        name,
		TotalReqs:   totalReqs,
		SuccessReqs: int(successCount),
		Duration:    totalDuration,
		RPS:         float64(successCount) / totalDuration.Seconds(),
	}

	if len(valid) > 0 {
		res.MinLatency = valid[0]
		res.MaxLatency = valid[len(valid)-1]
		res.MeanLatency = time.Duration(int64(sum) / int64(len(valid)))
		res.P50Latency = percentile(valid, 0.50)
		res.P90Latency = percentile(valid, 0.90)
		res.P95Latency = percentile(valid, 0.95)
		res.P99Latency = percentile(valid, 0.99)
	}

	return res
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Round(float64(len(sorted)-1) * p))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func main() {
	fmt.Println("================================================================================")
	fmt.Println("    BIFROST CACHEGUARD — REAL GATEWAY PERFORMANCE BENCHMARK (MODES A, B, C)     ")
	fmt.Println("================================================================================")

	workspaceRoot, err := filepath.Abs("../..")
	if err != nil {
		fmt.Printf("FAIL: Cannot resolve workspace root: %v\n", err)
		os.Exit(1)
	}

	pluginPath := filepath.Join(workspaceRoot, "plugins", "cacheguard", "build", "cacheguard.so")
	if _, err := os.Stat(pluginPath); err != nil {
		fmt.Printf("Compiling plugin first...\n")
		buildCmd := exec.Command("make", "build")
		buildCmd.Dir = filepath.Join(workspaceRoot, "plugins", "cacheguard")
		if out, err := buildCmd.CombinedOutput(); err != nil {
			fmt.Printf("FAIL: Plugin build failed: %v\n%s\n", err, string(out))
			os.Exit(1)
		}
	}

	mockServer, mockBaseURL, err := startMockServer()
	if err != nil {
		fmt.Printf("FAIL: Mock provider failed: %v\n", err)
		os.Exit(1)
	}
	defer mockServer.Close()
	fmt.Printf("✓ Mock provider active on %s\n", mockBaseURL)

	dataDir := filepath.Join(workspaceRoot, "plugins", "cacheguard", ".bench_data")
	_ = os.RemoveAll(dataDir)
	_ = os.MkdirAll(dataDir, 0755)
	defer os.RemoveAll(dataDir)

	const totalRequests = 600
	const concurrency = 6

	simplePayload := buildRequestPayload(false)
	toolPayload := buildRequestPayload(true)

	// Mode A: Stock Bifrost without CacheGuard
	fmt.Println("\n--- [MODE A] Stock Bifrost (No CacheGuard Plugin) ---")
	_, cleanupA, err := startGateway(workspaceRoot, dataDir, false, "", mockBaseURL)
	if err != nil {
		fmt.Printf("FAIL starting Mode A gateway: %v\n", err)
		os.Exit(1)
	}
	resA := runBenchmark("Mode A: Stock Bifrost (Baseline)", totalRequests, concurrency, simplePayload, nil)
	cleanupA()
	time.Sleep(1 * time.Second) // allow port release

	// Mode B: Bifrost with CacheGuard enabled, no session ID
	fmt.Println("\n--- [MODE B] Bifrost with CacheGuard Enabled (No Session ID) ---")
	_, cleanupB, err := startGateway(workspaceRoot, dataDir, true, pluginPath, mockBaseURL)
	if err != nil {
		fmt.Printf("FAIL starting Mode B gateway: %v\n", err)
		os.Exit(1)
	}
	resB := runBenchmark("Mode B: CacheGuard Enabled (No Session ID)", totalRequests, concurrency, simplePayload, nil)
	cleanupB()
	time.Sleep(1 * time.Second)

	// Mode C: Bifrost with CacheGuard enabled, valid session tracking and nested tool schema
	fmt.Println("\n--- [MODE C] Bifrost with CacheGuard Enabled (Session Tracking + Nested Tools) ---")
	_, cleanupC, err := startGateway(workspaceRoot, dataDir, true, pluginPath, mockBaseURL)
	if err != nil {
		fmt.Printf("FAIL starting Mode C gateway: %v\n", err)
		os.Exit(1)
	}
	resC := runBenchmark("Mode C: CacheGuard Enabled (Session + Nested Tools)", totalRequests, concurrency, toolPayload, map[string]string{
		"x-bf-cg-session-id": "bench-session-alpha",
	})
	cleanupC()

	fmt.Println("\n================================================================================")
	fmt.Println("                             BENCHMARK RESULTS SUMMARY                          ")
	fmt.Println("================================================================================")
	fmt.Printf("%-48s | %8s | %9s | %9s | %9s | %9s\n", "Mode", "RPS", "Mean", "p50", "p95", "p99")
	fmt.Println(strings.Repeat("-", 100))

	printRow := func(r BenchmarkResult) {
		fmt.Printf("%-48s | %8.1f | %9s | %9s | %9s | %9s\n",
			r.Mode,
			r.RPS,
			r.MeanLatency.Round(time.Microsecond),
			r.P50Latency.Round(time.Microsecond),
			r.P95Latency.Round(time.Microsecond),
			r.P99Latency.Round(time.Microsecond),
		)
	}

	printRow(resA)
	printRow(resB)
	printRow(resC)
	fmt.Println(strings.Repeat("-", 100))

	// Relative overhead analysis
	overheadP50B := float64(resB.P50Latency-resA.P50Latency) / float64(resA.P50Latency) * 100.0
	overheadP50C := float64(resC.P50Latency-resA.P50Latency) / float64(resA.P50Latency) * 100.0
	rpsDiffB := (resB.RPS - resA.RPS) / resA.RPS * 100.0
	rpsDiffC := (resC.RPS - resA.RPS) / resA.RPS * 100.0

	fmt.Printf("\nRelative Overhead Analysis (vs Stock Baseline):\n")
	fmt.Printf("  • Mode B (No session)    p50 latency delta: %+6.2f%% (%+v), RPS delta: %+6.2f%%\n",
		overheadP50B, (resB.P50Latency - resA.P50Latency).Round(time.Microsecond), rpsDiffB)
	fmt.Printf("  • Mode C (Session+Tools) p50 latency delta: %+6.2f%% (%+v), RPS delta: %+6.2f%%\n",
		overheadP50C, (resC.P50Latency - resA.P50Latency).Round(time.Microsecond), rpsDiffC)
	fmt.Println("================================================================================")
}
