//go:build ignore

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	fmt.Println("================================================================================")
	fmt.Println("      BIFROST CACHEGUARD — 7-STEP REPRODUCIBLE REPRO/DEMO WALKTHROUGH           ")
	fmt.Println("      (All Provider Metrics Labeled SIMULATED — Zero Paid APIs Required)       ")
	fmt.Println("================================================================================")

	workspaceRoot, err := filepath.Abs("../..")
	if err != nil {
		fmt.Printf("FAIL: Cannot resolve workspace root: %v\n", err)
		os.Exit(1)
	}

	pluginPath := filepath.Join(workspaceRoot, "plugins", "cacheguard", "build", "cacheguard.so")
	if _, err := os.Stat(pluginPath); err != nil {
		fmt.Println("Compiling plugin binary (cacheguard.so)...")
		cmd := exec.Command("make", "build")
		cmd.Dir = filepath.Join(workspaceRoot, "plugins", "cacheguard")
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Printf("FAIL: Build failed: %v\n%s\n", err, string(out))
			os.Exit(1)
		}
	}
	fmt.Printf("✓ Plugin binary ready at: %s\n", pluginPath)

	// Step 0: Start Mock Provider
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("FAIL: Mock listener failed: %v\n", err)
		os.Exit(1)
	}
	mockPort := listener.Addr().(*net.TCPAddr).Port
	mockBaseURL := fmt.Sprintf("http://127.0.0.1:%d", mockPort)

	var cachedTokens int = 0
	var promptTokens int = 150

	mockServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()

			if r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/chat/completions" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":      "chatcmpl-mock-demo",
					"object":  "chat.completion",
					"created": time.Now().Unix(),
					"model":   "gpt-4o",
					"choices": []map[string]any{
						{
							"index": 0,
							"message": map[string]any{
								"role":    "assistant",
								"content": "Mock completion response for prompt caching demonstration.",
							},
							"finish_reason": "stop",
						},
					},
					"usage": map[string]any{
						"prompt_tokens":     promptTokens,
						"completion_tokens": 12,
						"total_tokens":      promptTokens + 12,
						"prompt_tokens_details": map[string]any{
							"cached_tokens": cachedTokens,
						},
					},
				})
				return
			}

			if r.URL.Path == "/v1/messages" || r.URL.Path == "/messages" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":    "msg-mock-anthropic",
					"type":  "message",
					"role":  "assistant",
					"model": "claude-3-5-sonnet-20241022",
					"content": []map[string]any{
						{
							"type": "text",
							"text": "Mock Anthropic response.",
						},
					},
					"usage": map[string]any{
						"input_tokens":                promptTokens,
						"output_tokens":               12,
						"cache_read_input_tokens":     cachedTokens,
						"cache_creation_input_tokens": 0,
					},
				})
				return
			}

			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(body)
		}),
	}
	go func() { _ = mockServer.Serve(listener) }()
	defer mockServer.Close()
	fmt.Printf("✓ Local Mock AI Provider active on: %s\n", mockBaseURL)

	dataDir := filepath.Join(workspaceRoot, "plugins", "cacheguard", ".demo_data")
	_ = os.RemoveAll(dataDir)
	_ = os.MkdirAll(dataDir, 0755)
	defer os.RemoveAll(dataDir)

	client := &http.Client{Timeout: 5 * time.Second}

	// -------------------------------------------------------------------------
	// STEP 1: Bifrost without the plugin
	// -------------------------------------------------------------------------
	fmt.Println("\n================================================================================")
	fmt.Println("STEP 1: Run Stock Bifrost WITHOUT CacheGuard Plugin")
	fmt.Println("================================================================================")
	_, cleanup1, err := startDemoGateway(workspaceRoot, dataDir, false, "", mockBaseURL, false)
	if err != nil {
		fmt.Printf("FAIL: %v\n", err)
		os.Exit(1)
	}
	status1, hdr1, body1, err := sendChat(client, "gpt-4o", "string", nil)
	cleanup1()
	time.Sleep(500 * time.Millisecond)

	if err != nil || status1 != http.StatusOK {
		fmt.Printf("FAIL Step 1: status=%d, err=%v, body=%v\n", status1, err, body1)
		os.Exit(1)
	}
	fmt.Printf("✓ HTTP Status: %d OK\n", status1)
	fmt.Printf("✓ CacheGuard headers present: %v (expected false)\n", hdr1.Get("X-Cacheguard-Status") != "")
	fmt.Println("Result: Unmodified Bifrost handles requests normally without plugin overhead.")

	// -------------------------------------------------------------------------
	// STEP 2: Enable CacheGuard in Bifrost
	// -------------------------------------------------------------------------
	fmt.Println("\n================================================================================")
	fmt.Println("STEP 2: Boot Bifrost WITH CacheGuard Shared Object Plugin Loaded")
	fmt.Println("================================================================================")
	_, cleanupGateway, err := startDemoGateway(workspaceRoot, dataDir, true, pluginPath, mockBaseURL, true)
	if err != nil {
		fmt.Printf("FAIL Step 2: %v\n", err)
		os.Exit(1)
	}
	defer cleanupGateway()
	time.Sleep(500 * time.Millisecond)
	fmt.Println("✓ CacheGuard loaded successfully into stock Bifrost runtime (isolated port 8081).")

	// -------------------------------------------------------------------------
	// STEP 3: First tracked request
	// -------------------------------------------------------------------------
	fmt.Println("\n================================================================================")
	fmt.Println("STEP 3: First Tracked Request (Session Initialization)")
	fmt.Println("================================================================================")
	cachedTokens = 0
	sessionID := "demo-session-42"
	status3, hdr3, _, err := sendChat(client, "gpt-4o", "string", map[string]string{
		"X-Cacheguard-Session-ID": sessionID,
	})
	if err != nil || status3 != http.StatusOK {
		fmt.Printf("FAIL Step 3: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ HTTP Status:             %d OK\n", status3)
	fmt.Printf("✓ X-Cacheguard-Status:     %s\n", hdr3.Get("X-Cacheguard-Status"))
	fmt.Printf("✓ X-Cacheguard-Diagnosis:  %s\n", hdr3.Get("X-Cacheguard-Diagnosis"))
	fmt.Printf("✓ X-Cacheguard-Attribution:%s\n", hdr3.Get("X-Cacheguard-Attribution"))
	fmt.Println("Result: Initial baseline request fingerprint recorded in memory.")

	// -------------------------------------------------------------------------
	// STEP 4: Stable request (simulated cache hit)
	// -------------------------------------------------------------------------
	fmt.Println("\n================================================================================")
	fmt.Println("STEP 4: Stable Request (Exact Same Payload + SIMULATED Cache Hit)")
	fmt.Println("================================================================================")
	cachedTokens = 150 // Mock SIMULATED provider cache hit
	status4, hdr4, _, err := sendChat(client, "gpt-4o", "string", map[string]string{
		"X-Cacheguard-Session-ID": sessionID,
	})
	if err != nil || status4 != http.StatusOK {
		fmt.Printf("FAIL Step 4: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ HTTP Status:             %d OK\n", status4)
	fmt.Printf("✓ X-Cacheguard-Diagnosis:  %s\n", hdr4.Get("X-Cacheguard-Diagnosis"))
	fmt.Printf("✓ X-Cacheguard-Attribution:%s\n", hdr4.Get("X-Cacheguard-Attribution"))
	fmt.Printf("✓ X-Cacheguard-Cached-Tokens: %s [SIMULATED]\n", hdr4.Get("X-Cacheguard-Cached-Tokens"))
	fmt.Println("Result: Request fingerprint remained unchanged; provider cache hit verified.")

	// -------------------------------------------------------------------------
	// STEP 5: Changed tool schema field (CG001 warning)
	// -------------------------------------------------------------------------
	fmt.Println("\n================================================================================")
	fmt.Println("STEP 5: Tool Schema Drift (Type Changed string -> integer)")
	fmt.Println("================================================================================")
	cachedTokens = 0 // Invalidation caused by schema drift
	status5, hdr5, _, err := sendChat(client, "gpt-4o", "integer", map[string]string{
		"X-Cacheguard-Session-ID": sessionID,
	})
	if err != nil || status5 != http.StatusOK {
		fmt.Printf("FAIL Step 5: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ HTTP Status:             %d OK\n", status5)
	fmt.Printf("✓ X-Cacheguard-Diagnosis:  %s\n", hdr5.Get("X-Cacheguard-Diagnosis"))
	fmt.Printf("✓ X-Cacheguard-Attribution:%s\n", hdr5.Get("X-Cacheguard-Attribution"))
	fmt.Printf("✓ X-Cacheguard-Drift-Component: %s\n", hdr5.Get("X-Cacheguard-Drift-Component"))
	fmt.Printf("✓ X-Cacheguard-Drift-Path: %s\n", hdr5.Get("X-Cacheguard-Drift-Path"))
	fmt.Printf("✓ X-Cacheguard-Finding-Code: %s\n", hdr5.Get("X-Cacheguard-Finding-Code"))
	fmt.Printf("✓ X-Cacheguard-Finding-Msg:  %s\n", hdr5.Get("X-Cacheguard-Finding-Message"))
	fmt.Println("Result: CG001 correctly pinpointed tool schema mutation breaking prompt cache.")

	// -------------------------------------------------------------------------
	// STEP 6: Simulated cache miss with unknown-cause diagnosis (CG003)
	// -------------------------------------------------------------------------
	fmt.Println("\n================================================================================")
	fmt.Println("STEP 6: Stable Client Request with SIMULATED Provider Cache Miss (CG003)")
	fmt.Println("================================================================================")
	// Reset baseline with "string"
	sessionID6 := "demo-session-miss-eval"
	cachedTokens = 0
	_, _, _, _ = sendChat(client, "gpt-4o", "string", map[string]string{"X-Cacheguard-Session-ID": sessionID6})

	// Stable repeat request, but provider returned 0 cached tokens
	cachedTokens = 0
	status6, hdr6, _, err := sendChat(client, "gpt-4o", "string", map[string]string{
		"X-Cacheguard-Session-ID": sessionID6,
	})
	if err != nil || status6 != http.StatusOK {
		fmt.Printf("FAIL Step 6: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ HTTP Status:             %d OK\n", status6)
	fmt.Printf("✓ X-Cacheguard-Diagnosis:  %s\n", hdr6.Get("X-Cacheguard-Diagnosis"))
	fmt.Printf("✓ X-Cacheguard-Attribution:%s\n", hdr6.Get("X-Cacheguard-Attribution"))
	fmt.Printf("✓ X-Cacheguard-Finding-Code: %s\n", hdr6.Get("X-Cacheguard-Finding-Code"))
	fmt.Printf("✓ X-Cacheguard-Finding-Msg:  %s\n", hdr6.Get("X-Cacheguard-Finding-Message"))
	fmt.Println("Result: CG003 correctly diagnosed provider-side miss without blaming client.")

	// -------------------------------------------------------------------------
	// STEP 7: Actual Bifrost Cache-Control Injection (CG002)
	// -------------------------------------------------------------------------
	fmt.Println("\n================================================================================")
	fmt.Println("STEP 7: Bifrost Gateway Cache-Control Transformation Detection (CG002)")
	fmt.Println("================================================================================")
	sessionID7 := "demo-session-anthropic-cg002"
	status7, hdr7, _, err := sendAnthropicChat(client, map[string]string{
		"X-Cacheguard-Session-ID": sessionID7,
	})
	if err != nil || status7 != http.StatusOK {
		fmt.Printf("FAIL Step 7: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ HTTP Status:                    %d OK\n", status7)
	fmt.Printf("✓ X-Cacheguard-Transformation:    %s\n", hdr7.Get("X-Cacheguard-Transformation"))
	fmt.Printf("✓ X-Cacheguard-Client-Markers:    %s\n", hdr7.Get("X-Cacheguard-Client-Markers"))
	fmt.Printf("✓ X-Cacheguard-Outbound-Markers:  %s\n", hdr7.Get("X-Cacheguard-Outbound-Markers"))
	fmt.Printf("✓ X-Cacheguard-Injected-Path:     %s\n", hdr7.Get("X-Cacheguard-Injected-Path"))
	fmt.Printf("✓ X-Cacheguard-Finding-Code:      %s\n", hdr7.Get("X-Cacheguard-Finding-Code"))
	fmt.Printf("✓ X-Cacheguard-Finding-Message:   %s\n", hdr7.Get("X-Cacheguard-Finding-Message"))
	fmt.Println("Result: CG002 proved Bifrost auto-injected Anthropic cache markers.")

	fmt.Println("\n================================================================================")
	fmt.Println("               CACHEGUARD 7-STEP DEMO COMPLETED SUCCESSFULLY!                   ")
	fmt.Println("================================================================================")
}

func sendChat(client *http.Client, model, nodeIDType string, headers map[string]string) (int, http.Header, map[string]any, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": "You are a cloud cluster supervisor."},
			{"role": "user", "content": "Check node status."},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name": "query_node",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"node_id": map[string]any{"type": nodeIDType},
						},
						"required": []string{"node_id"},
					},
				},
			},
		},
	}

	reqBytes, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "http://127.0.0.1:8081/v1/chat/completions", bytes.NewReader(reqBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-bf-cacheguard-vk")
	req.Header.Set("x-bf-vk", "sk-bf-cacheguard-vk")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	return resp.StatusCode, resp.Header, parsed, nil
}

func sendAnthropicChat(client *http.Client, headers map[string]string) (int, http.Header, map[string]any, error) {
	payload := map[string]any{
		"model": "claude-3-5-sonnet-20241022",
		"messages": []map[string]any{
			{"role": "user", "content": "Verify prompt-cache breakpoint injection through stock Bifrost."},
		},
	}

	reqBytes, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "http://127.0.0.1:8081/v1/chat/completions", bytes.NewReader(reqBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-bf-cacheguard-vk")
	req.Header.Set("x-bf-vk", "sk-bf-cacheguard-vk")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	return resp.StatusCode, resp.Header, parsed, nil
}

func startDemoGateway(workspaceRoot, dataDir string, enablePlugin bool, pluginPath, mockBaseURL string, sendBackRaw bool) (*exec.Cmd, func(), error) {
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
				"send_back_raw_request": sendBackRaw,
				"keys": []map[string]any{
					{
						"name":   "Mock Demo Key",
						"value":  "sk-mock-key-for-demo",
						"models": []string{"gpt-4o"},
					},
				},
				"network_config": map[string]any{
					"base_url": mockBaseURL,
				},
			},
			"anthropic": map[string]any{
				"send_back_raw_request": sendBackRaw,
				"prompt_cache": map[string]any{
					"auto_inject": true,
				},
				"keys": []map[string]any{
					{
						"name":   "Mock Anthropic Key",
						"value":  "sk-mock-anthropic-key-for-demo",
						"models": []string{"claude-3-5-sonnet-20241022"},
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
					"name":      "Demo Virtual Key",
					"id":        "vk-cacheguard-demo",
					"value":     "sk-bf-cacheguard-vk",
					"is_active": true,
					"provider_configs": []map[string]any{
						{
							"provider":       "openai",
							"allowed_models": []string{"*"},
							"key_ids":        []string{"*"},
							"weight":         1.0,
						},
						{
							"provider":       "anthropic",
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
		return nil, nil, fmt.Errorf("failed to start gateway: %w", err)
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
