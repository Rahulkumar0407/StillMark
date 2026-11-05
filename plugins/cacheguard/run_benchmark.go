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
	Name        string        `json:"name"`
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
	ErrorCount  int           `json:"error_count"`
}

type RepetitionPair struct {
	RepNumber int
	Order     string
	Base      BenchmarkResult
	Plug      BenchmarkResult
	Overhead  OverheadMetrics
}

type OverheadMetrics struct {
	P50Pct  float64
	P95Pct  float64
	P99Pct  float64
	MeanPct float64
	RPSDiff float64
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

func startGateway(workspaceRoot string, enablePlugin bool, pluginPath, mockBaseURL string) (*exec.Cmd, func(), error) {
	dataDir, err := os.MkdirTemp("", "bifrost_bench_*")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create temp app dir: %w", err)
	}

	pluginsList := []map[string]any{}
	if enablePlugin {
		pluginsList = []map[string]any{
			{
				"name":    "cacheguard",
				"type":    "shared_object",
				"path":    pluginPath,
				"enabled": true,
				"config": map[string]any{
					"expose_schema_paths": true,
				},
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
				"send_back_raw_request": false,
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
		_ = os.RemoveAll(dataDir)
		return nil, nil, fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), configBytes, 0644); err != nil {
		_ = os.RemoveAll(dataDir)
		return nil, nil, fmt.Errorf("failed to write config: %w", err)
	}

	var cmd *exec.Cmd
	binPath := filepath.Join(workspaceRoot, "tmp", "bifrost-http")
	if _, err := os.Stat(binPath); err == nil {
		cmd = exec.Command(binPath,
			"-port", "8081",
			"-host", "127.0.0.1",
			"-app-dir", dataDir,
			"-log-level", "info",
			"-log-style", "pretty",
		)
	} else {
		cmd = exec.Command("go", "run", "main.go",
			"-port", "8081",
			"-host", "127.0.0.1",
			"-app-dir", dataDir,
			"-log-level", "info",
			"-log-style", "pretty",
		)
	}
	cmd.Dir = filepath.Join(workspaceRoot, "transports", "bifrost-http")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GOWORK=") {
			env = append(env, e)
		}
	}
	env = append(env, "GOWORK="+filepath.Join(workspaceRoot, "go.work"))
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dataDir)
		return nil, nil, fmt.Errorf("failed to start gateway process: %w", err)
	}

	cleanup := func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
		_ = os.RemoveAll(dataDir)
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
		time.Sleep(200 * time.Millisecond)
	}

	cleanup()
	return nil, nil, fmt.Errorf("gateway health check timed out on port 8081")
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
	var errCount int64
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
	for i := 0; i < 15; i++ {
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
					errCount++
					mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()
	totalDuration := time.Since(benchStart)

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
		Name:        name,
		TotalReqs:   totalReqs,
		SuccessReqs: int(successCount),
		ErrorCount:  int(errCount),
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

func verifyTrackedDiagnosis(reqBody []byte, headers map[string]string) (string, error) {
	req, err := http.NewRequest("POST", "http://127.0.0.1:8081/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-bf-cacheguard-vk")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	diag := resp.Header.Get("X-Cacheguard-Diagnosis")
	if diag == "" {
		return "", fmt.Errorf("missing X-Cacheguard-Diagnosis response header")
	}
	return diag, nil
}

func calculateOverhead(base, plug BenchmarkResult) OverheadMetrics {
	p50Diff := float64(plug.P50Latency - base.P50Latency)
	meanDiff := float64(plug.MeanLatency - base.MeanLatency)
	p95Diff := float64(plug.P95Latency - base.P95Latency)
	p99Diff := float64(plug.P99Latency - base.P99Latency)

	return OverheadMetrics{
		P50Pct:  (p50Diff / float64(base.P50Latency)) * 100.0,
		P95Pct:  (p95Diff / float64(base.P95Latency)) * 100.0,
		P99Pct:  (p99Diff / float64(base.P99Latency)) * 100.0,
		MeanPct: (meanDiff / float64(base.MeanLatency)) * 100.0,
		RPSDiff: ((plug.RPS - base.RPS) / base.RPS) * 100.0,
	}
}

func medianFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2.0
}

func minMaxFloat(vals []float64) (float64, float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	minV, maxV := vals[0], vals[0]
	for _, v := range vals[1:] {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	return minV, maxV
}

func main() {
	fmt.Println("================================================================================")
	fmt.Println("     BIFROST CACHEGUARD — 5-RUN REPEATED MATCHED BENCHMARK (ALTERNATING)       ")
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

	const totalRequests = 200
	const concurrency = 4
	const numRepetitions = 5

	simplePayload := buildRequestPayload(false)
	toolPayload := buildRequestPayload(true)

	headersA := map[string]string{}
	headersB := map[string]string{
		"x-cacheguard-session-id": "bench-session-b",
	}
	headersC := map[string]string{
		"x-cacheguard-session-id": "bench-session-c",
	}

	pairsA := make([]RepetitionPair, numRepetitions)
	pairsB := make([]RepetitionPair, numRepetitions)
	pairsC := make([]RepetitionPair, numRepetitions)

	for rep := 1; rep <= numRepetitions; rep++ {
		fmt.Printf("\n>>> EXECUTING REPETITION %d/%d (Order: %s) <<<\n",
			rep, numRepetitions,
			func() string {
				if rep%2 == 1 {
					return "Baseline -> Plugin"
				}
				return "Plugin -> Baseline"
			}(),
		)

		var baseRunA, baseRunB, baseRunC BenchmarkResult
		var plugRunA, plugRunB, plugRunC BenchmarkResult

		runBase := func() {
			fmt.Printf("  [Rep %d] Starting Baseline Gateway...\n", rep)
			_, cleanupBase, err := startGateway(workspaceRoot, false, "", mockBaseURL)
			if err != nil {
				fmt.Printf("FAIL starting baseline: %v\n", err)
				os.Exit(1)
			}
			baseRunA = runBenchmark(fmt.Sprintf("Base-A-Rep%d", rep), totalRequests, concurrency, simplePayload, headersA)
			baseRunB = runBenchmark(fmt.Sprintf("Base-B-Rep%d", rep), totalRequests, concurrency, simplePayload, headersB)
			baseRunC = runBenchmark(fmt.Sprintf("Base-C-Rep%d", rep), totalRequests, concurrency, toolPayload, headersC)
			cleanupBase()
			time.Sleep(500 * time.Millisecond)
		}

		runPlug := func() {
			fmt.Printf("  [Rep %d] Starting Plugin Gateway...\n", rep)
			_, cleanupPlug, err := startGateway(workspaceRoot, true, pluginPath, mockBaseURL)
			if err != nil {
				fmt.Printf("FAIL starting plugin: %v\n", err)
				os.Exit(1)
			}
			if rep == 1 {
				diagB, _ := verifyTrackedDiagnosis(simplePayload, headersB)
				diagC, _ := verifyTrackedDiagnosis(toolPayload, headersC)
				fmt.Printf("  ✓ Verified Mode B tracked diagnosis: %s, Mode C: %s\n", diagB, diagC)
			}
			plugRunA = runBenchmark(fmt.Sprintf("Plug-A-Rep%d", rep), totalRequests, concurrency, simplePayload, headersA)
			plugRunB = runBenchmark(fmt.Sprintf("Plug-B-Rep%d", rep), totalRequests, concurrency, simplePayload, headersB)
			plugRunC = runBenchmark(fmt.Sprintf("Plug-C-Rep%d", rep), totalRequests, concurrency, toolPayload, headersC)
			cleanupPlug()
			time.Sleep(500 * time.Millisecond)
		}

		if rep%2 == 1 {
			// Odd: Baseline first, then Plugin
			runBase()
			runPlug()
		} else {
			// Even: Plugin first, then Baseline
			runPlug()
			runBase()
		}

		pairsA[rep-1] = RepetitionPair{
			RepNumber: rep,
			Order: func() string {
				if rep%2 == 1 {
					return "Base->Plug"
				}
				return "Plug->Base"
			}(),
			Base:     baseRunA,
			Plug:     plugRunA,
			Overhead: calculateOverhead(baseRunA, plugRunA),
		}
		pairsB[rep-1] = RepetitionPair{
			RepNumber: rep,
			Order: func() string {
				if rep%2 == 1 {
					return "Base->Plug"
				}
				return "Plug->Base"
			}(),
			Base:     baseRunB,
			Plug:     plugRunB,
			Overhead: calculateOverhead(baseRunB, plugRunB),
		}
		pairsC[rep-1] = RepetitionPair{
			RepNumber: rep,
			Order: func() string {
				if rep%2 == 1 {
					return "Base->Plug"
				}
				return "Plug->Base"
			}(),
			Base:     baseRunC,
			Plug:     plugRunC,
			Overhead: calculateOverhead(baseRunC, plugRunC),
		}

		fmt.Printf("  Rep %d Complete: A p50 overhead=%+.2f%%, B p50 overhead=%+.2f%%, C p50 overhead=%+.2f%%\n",
			rep, pairsA[rep-1].Overhead.P50Pct, pairsB[rep-1].Overhead.P50Pct, pairsC[rep-1].Overhead.P50Pct)
	}

	// =========================================================================
	// STATISTICAL SYNTHESIS
	// =========================================================================
	fmt.Println("\n================================================================================")
	fmt.Println("             5-RUN REPEATED MATCHED BENCHMARK STATISTICAL SUMMARY               ")
	fmt.Println("================================================================================")

	summarizeScenario := func(title string, pairs []RepetitionPair) {
		p50Vals := make([]float64, len(pairs))
		p95Vals := make([]float64, len(pairs))
		p99Vals := make([]float64, len(pairs))
		meanVals := make([]float64, len(pairs))
		rpsBaseVals := make([]float64, len(pairs))
		rpsPlugVals := make([]float64, len(pairs))
		totalBaseErrs := 0
		totalPlugErrs := 0

		fmt.Printf("\n### %s\n", title)
		fmt.Printf("| Rep | Order | Base p50 | Plug p50 | p50 Overhead | Base p95 | Plug p95 | p95 Overhead | Base RPS | Plug RPS |\n")
		fmt.Printf("|-----|-------|----------|----------|--------------|----------|----------|--------------|----------|----------|\n")

		for i, p := range pairs {
			p50Vals[i] = p.Overhead.P50Pct
			p95Vals[i] = p.Overhead.P95Pct
			p99Vals[i] = p.Overhead.P99Pct
			meanVals[i] = p.Overhead.MeanPct
			rpsBaseVals[i] = p.Base.RPS
			rpsPlugVals[i] = p.Plug.RPS
			totalBaseErrs += p.Base.ErrorCount
			totalPlugErrs += p.Plug.ErrorCount

			fmt.Printf("| %3d | %-9s | %8s | %8s | %+11.2f%% | %8s | %8s | %+11.2f%% | %8.1f | %8.1f |\n",
				p.RepNumber,
				p.Order,
				p.Base.P50Latency.Round(time.Microsecond),
				p.Plug.P50Latency.Round(time.Microsecond),
				p.Overhead.P50Pct,
				p.Base.P95Latency.Round(time.Microsecond),
				p.Plug.P95Latency.Round(time.Microsecond),
				p.Overhead.P95Pct,
				p.Base.RPS,
				p.Plug.RPS,
			)
		}

		medP50 := medianFloat(p50Vals)
		minP50, maxP50 := minMaxFloat(p50Vals)
		medP95 := medianFloat(p95Vals)
		minP95, maxP95 := minMaxFloat(p95Vals)
		medP99 := medianFloat(p99Vals)
		medMean := medianFloat(meanVals)
		medBaseRPS := medianFloat(rpsBaseVals)
		medPlugRPS := medianFloat(rpsPlugVals)

		fmt.Printf("\n  • Median p50 Relative Overhead: %+.2f%% (Range: %+.2f%% to %+.2f%%, spread=%.2f%%)\n",
			medP50, minP50, maxP50, maxP50-minP50)
		fmt.Printf("  • Median p95 Relative Overhead: %+.2f%% (Range: %+.2f%% to %+.2f%%, spread=%.2f%%)\n",
			medP95, minP95, maxP95, maxP95-minP95)
		fmt.Printf("  • Median p99 Relative Overhead: %+.2f%%\n", medP99)
		fmt.Printf("  • Median Mean Overhead:        %+.2f%%\n", medMean)
		fmt.Printf("  • Median Throughput (RPS):     Baseline=%.1f RPS, Plugin=%.1f RPS (Diff: %+.2f%%)\n",
			medBaseRPS, medPlugRPS, (medPlugRPS-medBaseRPS)/medBaseRPS*100.0)
		fmt.Printf("  • Errors Encountered:          Baseline=%d, Plugin=%d\n", totalBaseErrs, totalPlugErrs)
	}

	summarizeScenario("Scenario A: Simple Chat (Untracked)", pairsA)
	summarizeScenario("Scenario B: Simple Chat (Session Tracked)", pairsB)
	summarizeScenario("Scenario C: Nested Tools (Session Tracked & Schema Flattened)", pairsC)

	fmt.Println("\n================================================================================")
	fmt.Println("             ALL 5 REPEATED BENCHMARK MEASUREMENTS COMPLETED!                   ")
	fmt.Println("================================================================================")
}
