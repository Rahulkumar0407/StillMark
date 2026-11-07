//go:build ignore

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type MockSSEConfig struct {
	mu            sync.Mutex
	cachedTokens  int
	includeUsage  bool
	failWithError bool
	delayPerChunk time.Duration
}

func startMockSSEProvider(cfg *MockSSEConfig) (*http.Server, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()

			cfg.mu.Lock()
			cachedToks := cfg.cachedTokens
			withUsage := cfg.includeUsage
			failErr := cfg.failWithError
			delay := cfg.delayPerChunk
			cfg.mu.Unlock()

			if failErr {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"simulated upstream provider failure","type":"api_error"}}`))
				return
			}

			// Check if streaming requested
			var reqMap map[string]any
			_ = json.Unmarshal(body, &reqMap)
			isStream, _ := reqMap["stream"].(bool)

			if !isStream {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"chatcmpl-mock","choices":[{"message":{"role":"assistant","content":"unary"}}]}`))
				return
			}

			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)

			flusher, ok := w.(http.Flusher)
			if !ok {
				return
			}

			// Chunk 1
			_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "},"finish_reason":null}]}`)
			flusher.Flush()

			if delay > 0 {
				time.Sleep(delay)
			}

			// Chunk 2
			_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"streaming world!"},"finish_reason":"stop"}]}`)
			flusher.Flush()

			if delay > 0 {
				time.Sleep(delay)
			}

			if withUsage {
				// Usage chunk
				usageJSON := fmt.Sprintf(`{"id":"chatcmpl-stream","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":5,"total_tokens":125,"prompt_tokens_details":{"cached_tokens":%d}}}`, cachedToks)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", usageJSON)
				flusher.Flush()
			}

			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		}),
	}

	go func() {
		_ = server.Serve(listener)
	}()

	return server, baseURL, nil
}

func startStreamingGateway(workspaceRoot, dataDir string, enablePlugin bool, pluginPath, mockBaseURL string) (*exec.Cmd, func(), error) {
	pluginsList := []map[string]any{}
	if enablePlugin {
		pluginsList = []map[string]any{
			{
				"name":    "stillmark",
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
			"port": "8086", // Non-8080 port
			"host": "127.0.0.1",
		},
		"plugins": pluginsList,
		"providers": map[string]any{
			"openai": map[string]any{
				"send_back_raw_request": false,
				"keys": []map[string]any{
					{
						"name":   "Mock Stream Key",
						"value":  "sk-mock-key-stream",
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
					"name":      "Stream Virtual Key",
					"id":        "vk-stillmark-stream",
					"value":     "sk-bf-stream-vk",
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
		return nil, nil, err
	}

	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), configBytes, 0644); err != nil {
		return nil, nil, err
	}

	var cmd *exec.Cmd
	binPath := filepath.Join(workspaceRoot, "tmp", "bifrost-http")
	if _, err := os.Stat(binPath); err == nil {
		cmd = exec.Command(binPath,
			"-port", "8086",
			"-host", "127.0.0.1",
			"-app-dir", dataDir,
			"-log-level", "info",
			"-log-style", "pretty",
		)
	} else {
		cmd = exec.Command("go", "run", "main.go",
			"-port", "8086",
			"-host", "127.0.0.1",
			"-app-dir", dataDir,
			"-log-level", "info",
			"-log-style", "pretty",
		)
	}
	cmd.Dir = filepath.Join(workspaceRoot, "transports", "bifrost-http")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GOWORK=") {
			env = append(env, e)
		}
	}
	env = append(env, "GOWORK="+filepath.Join(workspaceRoot, "go.work"))
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	}

	client := &http.Client{Timeout: 1 * time.Second}
	startTime := time.Now()
	for time.Since(startTime) < 45*time.Second {
		resp, err := client.Get("http://127.0.0.1:8086/health")
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
	return nil, nil, fmt.Errorf("gateway health check timed out on port 8086")
}

func readSSEResponse(r io.Reader) ([]string, error) {
	bodyBytes, err := io.ReadAll(r)
	raw := string(bodyBytes)
	lines := strings.Split(raw, "\n")
	return lines, err
}

func main() {
	fmt.Println("================================================================================")
	fmt.Println("         BIFROST STILLMARK — REAL HTTP SSE STREAMING VERIFICATION             ")
	fmt.Println("================================================================================")

	workspaceRoot, err := filepath.Abs("../..")
	if err != nil {
		fmt.Printf("FAIL: Cannot resolve workspace root: %v\n", err)
		os.Exit(1)
	}

	pluginPath := filepath.Join(workspaceRoot, "plugins", "stillmark", "build", "stillmark.so")
	if _, err := os.Stat(pluginPath); err != nil {
		fmt.Println("Compiling plugin shared object...")
		buildCmd := exec.Command("make", "build")
		buildCmd.Dir = filepath.Join(workspaceRoot, "plugins", "stillmark")
		if out, err := buildCmd.CombinedOutput(); err != nil {
			fmt.Printf("FAIL: Plugin build failed: %v\n%s\n", err, string(out))
			os.Exit(1)
		}
	}

	mockConfig := &MockSSEConfig{
		cachedTokens:  50,
		includeUsage:  true,
		delayPerChunk: 150 * time.Millisecond,
	}

	mockServer, mockBaseURL, err := startMockSSEProvider(mockConfig)
	if err != nil {
		fmt.Printf("FAIL: Mock provider failed: %v\n", err)
		os.Exit(1)
	}
	defer mockServer.Close()
	fmt.Printf("✓ Local Mock SSE Provider active on: %s\n", mockBaseURL)

	dataDir, err := os.MkdirTemp("", "bifrost_stream_*")
	if err != nil {
		fmt.Printf("FAIL: Cannot create temp data dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dataDir)

	// -------------------------------------------------------------------------
	// 1. BOOT GATEWAY WITH STILLMARK
	// -------------------------------------------------------------------------
	fmt.Println("\nStarting Bifrost Gateway on port 8086 with StillMark plugin...")
	_, cleanupGateway, err := startStreamingGateway(workspaceRoot, dataDir, true, pluginPath, mockBaseURL)
	if err != nil {
		fmt.Printf("FAIL: Cannot start streaming gateway: %v\n", err)
		os.Exit(1)
	}
	defer cleanupGateway()

	failTest := func(msg string, args ...any) {
		fmt.Printf("FAIL: "+msg+"\n", args...)
		if cleanupGateway != nil {
			cleanupGateway()
		}
		if mockServer != nil {
			mockServer.Close()
		}
		_ = os.RemoveAll(dataDir)
		os.Exit(1)
	}
	time.Sleep(1500 * time.Millisecond)
	fmt.Println("✓ Gateway is online and healthy on port 8086.")

	client := &http.Client{Timeout: 10 * time.Second}

	reqBody := []byte(`{
		"model": "gpt-4o",
		"stream": true,
		"messages": [
			{"role": "system", "content": "You are a streaming test assistant."},
			{"role": "user", "content": "Stream me a message."}
		]
	}`)

	// TEST 1: SUCCESSFUL STREAMING TURN 1 & TURN 2
	fmt.Println("\n[TEST 1] Successful Streaming Completion & Header Check")
	req1, _ := http.NewRequest("POST", "http://127.0.0.1:8086/v1/chat/completions", bytes.NewReader(reqBody))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer sk-bf-stream-vk")
	req1.Header.Set("x-stillmark-session-id", "stream-sess-alpha")

	resp1, err := client.Do(req1)
	if err != nil {
		failTest("Stream request 1 failed: %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp1.Body)
		failTest("Turn 1 returned status %d: %s", resp1.StatusCode, string(b))
	}

	diag1 := resp1.Header.Get("X-Stillmark-Diagnosis")
	cachedHdr1 := resp1.Header.Get("X-Stillmark-Cached-Tokens")
	status1 := resp1.Header.Get("X-Stillmark-Status")

	fmt.Printf("  • Turn 1 HTTP Status:      %d\n", resp1.StatusCode)

	fmt.Printf("  • Turn 1 Status Header:    %s (Expected: active)\n", status1)
	fmt.Printf("  • Turn 1 Diagnosis Header: %s (Expected: initial_request)\n", diag1)
	fmt.Printf("  • Turn 1 Cached Tokens:    %q (Expected: empty in SSE header - resolved post-stream)\n", cachedHdr1)

	lines1, err := readSSEResponse(resp1.Body)
	if err != nil {
		fmt.Printf("FAIL: Failed to read SSE body: %v\n", err)
		os.Exit(1)
	}
	var chunkCount int
	var hasDone bool
	for _, l := range lines1 {
		if strings.HasPrefix(l, "data: {") {
			chunkCount++
		}
		if strings.Contains(l, "[DONE]") {
			hasDone = true
		}
	}
	fmt.Printf("  • Streamed Chunks Received: %d, Has [DONE]: %t\n", chunkCount, hasDone)
	if diag1 != "initial_request" || status1 != "active" || cachedHdr1 != "" || !hasDone {
		fmt.Printf("FAIL: Turn 1 SSE assertions failed\n")
		os.Exit(1)
	}
	fmt.Println("✓ TEST 1 PASS: Turn 1 completed cleanly with expected initial headers.")

	// Turn 2: Repeat stable streaming request
	fmt.Println("\n[TEST 2] Repeat Stable Streaming Request (Timing of Headers vs Tokens)")
	req2, _ := http.NewRequest("POST", "http://127.0.0.1:8086/v1/chat/completions", bytes.NewReader(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer sk-bf-stream-vk")
	req2.Header.Set("x-stillmark-session-id", "stream-sess-alpha")

	resp2, err := client.Do(req2)
	if err != nil {
		fmt.Printf("FAIL: Stream request 2 failed: %v\n", err)
		os.Exit(1)
	}
	defer resp2.Body.Close()

	diag2 := resp2.Header.Get("X-Stillmark-Diagnosis")
	cachedHdr2 := resp2.Header.Get("X-Stillmark-Cached-Tokens")
	findingHdr2 := resp2.Header.Get("X-Stillmark-Finding-Code")

	fmt.Printf("  • Turn 2 SSE Diagnosis Header: %s (Expected: structurally_stable - NOT premature stable_miss!)\n", diag2)
	fmt.Printf("  • Turn 2 Cached Tokens Header: %q (Expected: empty in SSE header)\n", cachedHdr2)
	fmt.Printf("  • Turn 2 Finding Code Header:  %q (Expected: empty in SSE header - resolved post-stream)\n", findingHdr2)

	lines2, _ := readSSEResponse(resp2.Body)
	fmt.Printf("  • Turn 2 Chunks Read:          %d\n", len(lines2))

	if diag2 != "structurally_stable" {
		fmt.Printf("FAIL: Diagnosis must be structurally_stable before stream tokens arrive; was: %s\n", diag2)
		os.Exit(1)
	}
	if cachedHdr2 != "" || findingHdr2 != "" {
		fmt.Printf("FAIL: Future-dependent usage metrics/findings appeared prematurely in SSE headers!\n")
		os.Exit(1)
	}
	fmt.Println("✓ TEST 2 PASS: Turn 2 proved no premature future-dependent diagnosis appears in SSE headers.")

	// TEST 3: PROVIDER ERROR DURING STREAMING DOES NOT COMMIT SNAPSHOT
	fmt.Println("\n[TEST 3] Upstream Provider Error & Session Invariance")
	mockConfig.mu.Lock()
	mockConfig.failWithError = true
	mockConfig.mu.Unlock()

	reqErr, _ := http.NewRequest("POST", "http://127.0.0.1:8086/v1/chat/completions", bytes.NewReader(reqBody))
	reqErr.Header.Set("Content-Type", "application/json")
	reqErr.Header.Set("Authorization", "Bearer sk-bf-stream-vk")
	reqErr.Header.Set("x-stillmark-session-id", "stream-sess-alpha")

	respErr, err := client.Do(reqErr)
	if err == nil {
		_ = respErr.Body.Close()
		fmt.Printf("  • Upstream Error Response Status: %d\n", respErr.StatusCode)
	}

	// Restore mock
	mockConfig.mu.Lock()
	mockConfig.failWithError = false
	mockConfig.mu.Unlock()

	// Turn after error: sending the exact same payload should still see session intact
	reqAfterErr, _ := http.NewRequest("POST", "http://127.0.0.1:8086/v1/chat/completions", bytes.NewReader(reqBody))
	reqAfterErr.Header.Set("Content-Type", "application/json")
	reqAfterErr.Header.Set("Authorization", "Bearer sk-bf-stream-vk")
	reqAfterErr.Header.Set("x-stillmark-session-id", "stream-sess-alpha")

	respAfterErr, err := client.Do(reqAfterErr)
	if err != nil {
		fmt.Printf("FAIL: Request after error failed: %v\n", err)
		os.Exit(1)
	}
	defer respAfterErr.Body.Close()
	diagAfterErr := respAfterErr.Header.Get("X-Stillmark-Diagnosis")
	_, _ = io.Copy(io.Discard, respAfterErr.Body)

	fmt.Printf("  • Diagnosis after Error:       %s (Expected: structurally_stable - session was uncorrupted)\n", diagAfterErr)
	if diagAfterErr != "structurally_stable" {
		fmt.Printf("FAIL: Failed request corrupted session history! diag=%s\n", diagAfterErr)
		os.Exit(1)
	}
	fmt.Println("✓ TEST 3 PASS: Upstream error did not corrupt session history.")

	// TEST 4: MID-STREAM CANCELLATION
	fmt.Println("\n[TEST 4] Mid-Stream Client Cancellation")
	ctxCancel, cancelFunc := context.WithCancel(context.Background())
	reqCancel, _ := http.NewRequestWithContext(ctxCancel, "POST", "http://127.0.0.1:8086/v1/chat/completions", bytes.NewReader(reqBody))
	reqCancel.Header.Set("Content-Type", "application/json")
	reqCancel.Header.Set("Authorization", "Bearer sk-bf-stream-vk")
	reqCancel.Header.Set("x-stillmark-session-id", "stream-sess-cancel")

	respCancel, err := client.Do(reqCancel)
	if err == nil {
		// Read one chunk then cancel
		reader := bufio.NewReader(respCancel.Body)
		_, _ = reader.ReadString('\n')
		cancelFunc() // Cancel mid-stream!
		_ = respCancel.Body.Close()
		fmt.Println("  • Cancelled stream after first chunk.")
	} else {
		cancelFunc()
	}

	time.Sleep(200 * time.Millisecond)

	// Verify session history for stream-sess-cancel: should be treated as initial because turn was cancelled
	reqCheckCancel, _ := http.NewRequest("POST", "http://127.0.0.1:8086/v1/chat/completions", bytes.NewReader(reqBody))
	reqCheckCancel.Header.Set("Content-Type", "application/json")
	reqCheckCancel.Header.Set("Authorization", "Bearer sk-bf-stream-vk")
	reqCheckCancel.Header.Set("x-stillmark-session-id", "stream-sess-cancel")

	respCheckCancel, err := client.Do(reqCheckCancel)
	if err != nil {
		fmt.Printf("FAIL: Request after cancel failed: %v\n", err)
		os.Exit(1)
	}
	defer respCheckCancel.Body.Close()
	diagCheckCancel := respCheckCancel.Header.Get("X-Stillmark-Diagnosis")
	_, _ = io.Copy(io.Discard, respCheckCancel.Body)

	fmt.Printf("  • Turn after Cancellation:     %s (Expected: initial_request - cancelled turn was not committed)\n", diagCheckCancel)
	if diagCheckCancel != "initial_request" {
		fmt.Printf("FAIL: Cancelled request committed snapshot to session history! diag=%s\n", diagCheckCancel)
		os.Exit(1)
	}
	fmt.Println("✓ TEST 4 PASS: Cancelled request was not committed to session tracker.")

	// TEST 5: MISSING USAGE DETAILS IN SSE
	fmt.Println("\n[TEST 5] Missing Usage in Stream")
	mockConfig.mu.Lock()
	mockConfig.includeUsage = false
	mockConfig.mu.Unlock()

	reqNoUsage, _ := http.NewRequest("POST", "http://127.0.0.1:8086/v1/chat/completions", bytes.NewReader(reqBody))
	reqNoUsage.Header.Set("Content-Type", "application/json")
	reqNoUsage.Header.Set("Authorization", "Bearer sk-bf-stream-vk")
	reqNoUsage.Header.Set("x-stillmark-session-id", "stream-sess-nousage")

	respNoUsage, err := client.Do(reqNoUsage)
	if err != nil {
		fmt.Printf("FAIL: Request without usage failed: %v\n", err)
		os.Exit(1)
	}
	defer respNoUsage.Body.Close()
	_, _ = io.Copy(io.Discard, respNoUsage.Body)
	fmt.Println("✓ TEST 5 PASS: Missing usage in stream handled fail-open without panic or failure.")

	// TEST 6: CONCURRENT STREAMING SESSIONS
	fmt.Println("\n[TEST 6] Concurrent Streaming Sessions")
	mockConfig.mu.Lock()
	mockConfig.includeUsage = true
	mockConfig.mu.Unlock()

	var wg sync.WaitGroup
	const concurrentStreams = 6
	errCount := 0
	var errMu sync.Mutex

	for i := 0; i < concurrentStreams; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sessID := fmt.Sprintf("stream-concurrent-%d", idx)
			req, _ := http.NewRequest("POST", "http://127.0.0.1:8086/v1/chat/completions", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sk-bf-stream-vk")
			req.Header.Set("x-stillmark-session-id", sessID)

			resp, err := client.Do(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				errMu.Lock()
				errCount++
				errMu.Unlock()
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
		}(i)
	}

	wg.Wait()
	fmt.Printf("  • Concurrent Streams Completed: %d/%d, Errors: %d\n", concurrentStreams-errCount, concurrentStreams, errCount)
	if errCount > 0 {
		fmt.Printf("FAIL: Concurrent streams encountered %d errors\n", errCount)
		os.Exit(1)
	}
	fmt.Println("✓ TEST 6 PASS: All concurrent streaming sessions completed successfully.")

	fmt.Println("\n================================================================================")
	fmt.Println("      ALL REAL HTTP SSE STREAMING VERIFICATION TESTS PASSED CLEANLY!            ")
	fmt.Println("================================================================================")
}
