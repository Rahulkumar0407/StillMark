package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	frameworkPlugins "github.com/maximhq/bifrost/framework/plugins"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	_ = InitEngine(DefaultMaxSessions, DefaultMaxEntries, DefaultSessionTTL)
	code := m.Run()
	CleanupEngine()
	os.Exit(code)
}

// In-memory capture for mock provider HTTP requests.
// Raw bytes remain in memory only and are never persisted to disk or logged.
type MockProviderCapture struct {
	mu           sync.Mutex
	capturedBody [][]byte
	cachedTokens int
}

func (m *MockProviderCapture) Record(body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([]byte, len(body))
	copy(copied, body)
	m.capturedBody = append(m.capturedBody, copied)
}

func (m *MockProviderCapture) GetCaptured(index int) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < len(m.capturedBody) {
		return m.capturedBody[index]
	}
	return nil
}

func (m *MockProviderCapture) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.capturedBody)
}

func createMockProviderServer(capture *MockProviderCapture) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		capture.Record(body)

		response := map[string]any{
			"id":      "chatcmpl-mock-cacheguard",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "mock-gpt-4o",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "Verified mock response.",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     150,
				"completion_tokens": 15,
				"total_tokens":      165,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": capture.cachedTokens,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	}))
}

// TestPluginDynamicLoading verifies loading cacheguard.so through Bifrost loader.
func TestPluginDynamicLoading(t *testing.T) {
	pluginPath, err := filepath.Abs("build/cacheguard.so")
	require.NoError(t, err)
	require.FileExists(t, pluginPath, "cacheguard.so must exist before running test")

	loader := &frameworkPlugins.SharedObjectPluginLoader{}

	name, err := loader.VerifyBasePlugin(pluginPath)
	if err != nil && strings.Contains(err.Error(), "different version of package") {
		t.Skip("Skipping plugin.Open under -race mode: shared library compiled without race instrumentation")
		return
	}
	require.NoError(t, err)
	assert.Equal(t, PluginName, name)

	basePlugin, err := loader.LoadPlugin(pluginPath, nil)
	if err != nil && strings.Contains(err.Error(), "different version of package") {
		t.Skip("Skipping plugin.Open under -race mode: shared library compiled without race instrumentation")
		return
	}
	require.NoError(t, err)
	require.NotNil(t, basePlugin)

	llmPlugin, ok := basePlugin.(schemas.LLMPlugin)
	require.True(t, ok)
	assert.Equal(t, PluginName, llmPlugin.GetName())

	err = llmPlugin.Cleanup()
	assert.NoError(t, err)

	// Re-initialize for subsequent tests
	_ = InitEngine(DefaultMaxSessions, DefaultMaxEntries, DefaultSessionTTL)
}

// TestA_IdenticalRequestsOutboundPayload verifies identical requests produce identical outbound payloads.
func TestA_IdenticalRequestsOutboundPayload(t *testing.T) {
	capture := &MockProviderCapture{cachedTokens: 0}
	server := createMockProviderServer(capture)
	defer server.Close()

	systemText := "You are a specialized diagnostic assistant."
	userText := "Summarize the system health status."

	buildReq := func() *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			RequestType: schemas.ChatCompletionRequest,
			ChatRequest: &schemas.BifrostChatRequest{
				Provider: schemas.OpenAI,
				Model:    "mock-gpt-4o",
				Input: []schemas.ChatMessage{
					{
						Role: schemas.ChatMessageRoleSystem,
						Content: &schemas.ChatMessageContent{
							ContentStr: &systemText,
						},
					},
					{
						Role: schemas.ChatMessageRoleUser,
						Content: &schemas.ChatMessageContent{
							ContentStr: &userText,
						},
					},
				},
			},
		}
	}

	req1 := buildReq()
	req2 := buildReq()

	ctx1, cancel1 := schemas.NewBifrostContextWithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	ctx2, cancel2 := schemas.NewBifrostContextWithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	_, _, err := PreLLMHook(ctx1, req1)
	require.NoError(t, err)
	diag1, ok1 := GetRequestDiagnostic(ctx1)
	require.True(t, ok1)

	_, _, err = PreLLMHook(ctx2, req2)
	require.NoError(t, err)
	diag2, ok2 := GetRequestDiagnostic(ctx2)
	require.True(t, ok2)

	assert.Equal(t, diag1.Snapshot.PrefixHMAC, diag2.Snapshot.PrefixHMAC)
	assert.NotEmpty(t, diag1.Snapshot.PrefixHMAC)

	payload1, err := json.Marshal(req1.ChatRequest)
	require.NoError(t, err)
	payload2, err := json.Marshal(req2.ChatRequest)
	require.NoError(t, err)

	resp1, err := http.Post(server.URL+"/v1/chat/completions", "application/json", bytes.NewReader(payload1))
	require.NoError(t, err)
	_ = resp1.Body.Close()

	resp2, err := http.Post(server.URL+"/v1/chat/completions", "application/json", bytes.NewReader(payload2))
	require.NoError(t, err)
	_ = resp2.Body.Close()

	require.Equal(t, 2, capture.Count())
	assert.Equal(t, capture.GetCaptured(0), capture.GetCaptured(1), "Test A Passed: Outbound payloads are byte-identical")
}

// TestB_ToolSchemaDriftDetection verifies modified tool schema changes fingerprint.
func TestB_ToolSchemaDriftDetection(t *testing.T) {
	toolName := "get_cluster_metrics"
	toolDesc := "Retrieve real-time cluster metrics"

	paramJSON1 := []byte(`{"type":"object","properties":{"region":{"type":"string","description":"Deployment region"}},"required":["region"]}`)
	var params1 schemas.ToolFunctionParameters
	err := json.Unmarshal(paramJSON1, &params1)
	require.NoError(t, err)

	paramJSON2 := []byte(`{"type":"object","properties":{"region":{"type":"string","description":"Deployment region"},"detailed":{"type":"boolean"}},"required":["region"]}`)
	var params2 schemas.ToolFunctionParameters
	err = json.Unmarshal(paramJSON2, &params2)
	require.NoError(t, err)

	req1 := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "mock-gpt-4o",
			Params: &schemas.ChatParameters{
				Tools: []schemas.ChatTool{
					{
						Type: schemas.ChatToolTypeFunction,
						Function: &schemas.ChatToolFunction{
							Name:        toolName,
							Description: &toolDesc,
							Parameters:  &params1,
						},
					},
				},
			},
		},
	}

	req2 := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "mock-gpt-4o",
			Params: &schemas.ChatParameters{
				Tools: []schemas.ChatTool{
					{
						Type: schemas.ChatToolTypeFunction,
						Function: &schemas.ChatToolFunction{
							Name:        toolName,
							Description: &toolDesc,
							Parameters:  &params2,
						},
					},
				},
			},
		},
	}

	ctx1, cancel1 := schemas.NewBifrostContextWithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	ctx2, cancel2 := schemas.NewBifrostContextWithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	_, _, err = PreLLMHook(ctx1, req1)
	require.NoError(t, err)
	diag1, ok1 := GetRequestDiagnostic(ctx1)
	require.True(t, ok1)

	_, _, err = PreLLMHook(ctx2, req2)
	require.NoError(t, err)
	diag2, ok2 := GetRequestDiagnostic(ctx2)
	require.True(t, ok2)

	assert.NotEqual(t, diag1.Snapshot.ToolsHMAC, diag2.Snapshot.ToolsHMAC)
	assert.NotEmpty(t, diag1.Snapshot.ToolsHMAC)
	assert.NotEmpty(t, diag2.Snapshot.ToolsHMAC)
}

// TestC_ResponseUsageMetadata verifies cache hit and cache miss observation.
func TestC_ResponseUsageMetadata(t *testing.T) {
	capture := &MockProviderCapture{cachedTokens: 128}
	server := createMockProviderServer(capture)
	defer server.Close()

	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "mock-gpt-4o",
		},
	}
	_, _, err := PreLLMHook(ctx, req)
	require.NoError(t, err)

	httpResp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	defer httpResp.Body.Close()

	var providerResp schemas.BifrostChatResponse
	err = json.NewDecoder(httpResp.Body).Decode(&providerResp)
	require.NoError(t, err)

	bifrostResp := &schemas.BifrostResponse{
		ChatResponse: &providerResp,
	}

	_, _, err = PostLLMHook(ctx, bifrostResp, nil)
	require.NoError(t, err)

	diag, ok := GetRequestDiagnostic(ctx)
	require.True(t, ok)
	assert.True(t, diag.HasUsage)
	assert.Equal(t, 150, diag.PromptTokens)
	assert.Equal(t, 128, diag.CachedReadTokens)
	assert.Equal(t, 0, diag.CachedWriteTokens)
}

// TestTask5_RawRequestCaptureInvestigation verifies JSON compacting and absence of credentials.
func TestTask5_RawRequestCaptureInvestigation(t *testing.T) {
	rawJSONWithSpaces := []byte("{\n  \"model\": \"mock-gpt-4o\",\n  \"stream\": false\n}")
	var buf bytes.Buffer
	err := schemas.Compact(&buf, rawJSONWithSpaces)
	require.NoError(t, err)

	expectedCompact := []byte(`{"model":"mock-gpt-4o","stream":false}`)
	assert.Equal(t, expectedCompact, buf.Bytes())

	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	systemText := "Classified system prompt"
	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "mock-gpt-4o",
			Input: []schemas.ChatMessage{
				{
					Role: schemas.ChatMessageRoleSystem,
					Content: &schemas.ChatMessageContent{
						ContentStr: &systemText,
					},
				},
			},
		},
	}
	_, _, err = PreLLMHook(ctx, req)
	require.NoError(t, err)

	diag, ok := GetRequestDiagnostic(ctx)
	require.True(t, ok)
	diagJSON, err := json.Marshal(diag)
	require.NoError(t, err)

	// Ensure no raw secrets, prompts, or auth headers are present in diagnostic
	assert.NotContains(t, string(diagJSON), "Classified system prompt")
	assert.NotContains(t, string(diagJSON), "Bearer")
	assert.NotContains(t, string(diagJSON), "api-key")
}

// TestHMACStabilityAndDomainSeparation verifies Task 2:
// - HMAC stability during same process lifetime.
// - Domain separation produces distinct hashes for identical content across domains.
func TestHMACStabilityAndDomainSeparation(t *testing.T) {
	data := []byte("identical payload")

	mac1 := computeHMAC(domainPrefixPrompt, data)
	mac2 := computeHMAC(domainPrefixPrompt, data)
	assert.Equal(t, mac1, mac2, "HMAC must be deterministic within same process lifetime")

	macTool := computeHMAC(domainPrefixTool, data)
	assert.NotEqual(t, mac1, macTool, "Domain separation must yield different HMACs for different domains")

	macSchema := computeHMAC(domainPrefixSchemaPath, data)
	assert.NotEqual(t, mac1, macSchema, "Domain separation must yield different HMACs for schema path domain")
	assert.NotEqual(t, macTool, macSchema)
}

// TestKeyRotationOnNewPluginInstance verifies Task 2:
// Ephemeral key rotation on new plugin instance ensures fingerprints are not cross-process correlatable.
func TestKeyRotationOnNewPluginInstance(t *testing.T) {
	data := []byte("secret prompt instructions")

	macBefore := computeHMAC(domainPrefixPrompt, data)
	require.NotEmpty(t, macBefore)

	// Simulate plugin restart / reload
	CleanupEngine()
	err := InitEngine(100, 10, time.Hour)
	require.NoError(t, err)

	macAfter := computeHMAC(domainPrefixPrompt, data)
	require.NotEmpty(t, macAfter)

	assert.NotEqual(t, macBefore, macAfter, "Key rotation must produce distinct HMACs across process lifetimes")
}

// TestSessionValidation verifies Task 3:
// Format and length validation for X-CacheGuard-Session-ID.
func TestSessionValidation(t *testing.T) {
	// Valid IDs
	assert.True(t, ValidateSessionID("session_12345"))
	assert.True(t, ValidateSessionID("sess-abcdef-0123456789"))
	assert.True(t, ValidateSessionID("12345678"))              // exactly 8 chars
	assert.True(t, ValidateSessionID(strings.Repeat("a", 64))) // exactly 64 chars

	// Invalid IDs
	assert.False(t, ValidateSessionID("short"))                 // < 8 chars
	assert.False(t, ValidateSessionID("sess with space"))       // spaces not allowed
	assert.False(t, ValidateSessionID("sess@special!#"))        // special chars not allowed
	assert.False(t, ValidateSessionID(""))                      // empty
	assert.False(t, ValidateSessionID(strings.Repeat("a", 65))) // > 64 chars
}

// TestTenantIsolationAndSafeRefusal verifies Task 3:
// Sessions refuse cross-request comparison when tenant isolation cannot be proven.
func TestTenantIsolationAndSafeRefusal(t *testing.T) {
	systemText := "System prompt"
	buildReq := func() *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			RequestType: schemas.ChatCompletionRequest,
			ChatRequest: &schemas.BifrostChatRequest{
				Provider: schemas.OpenAI,
				Model:    "mock-gpt-4o",
				Input: []schemas.ChatMessage{
					{
						Role:    schemas.ChatMessageRoleSystem,
						Content: &schemas.ChatMessageContent{ContentStr: &systemText},
					},
				},
			},
		}
	}

	// Case 1: Session ID provided, but NO trusted tenant in context -> Refuse comparison
	ctxUntrusted := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctxUntrusted.SetValue(ctxSessionKey, "session_tenant_123")

	_, _, err := PreLLMHook(ctxUntrusted, buildReq())
	require.NoError(t, err)
	diagUntrusted, ok := GetRequestDiagnostic(ctxUntrusted)
	require.True(t, ok)
	assert.Equal(t, DiagnosisRefused, diagUntrusted.Diagnosis, "Must refuse comparison when tenant isolation cannot be verified")
	assert.False(t, diagUntrusted.TrustedTenant)

	// Case 2: Session ID with trusted Governance Customer ID -> Permitted
	ctxTrustedCustomer := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctxTrustedCustomer.SetValue(ctxSessionKey, "session_tenant_123")
	ctxTrustedCustomer.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, "tenant_corp_alpha")

	_, _, err = PreLLMHook(ctxTrustedCustomer, buildReq())
	require.NoError(t, err)
	diagCustomer, ok := GetRequestDiagnostic(ctxTrustedCustomer)
	require.True(t, ok)
	assert.True(t, diagCustomer.TrustedTenant)
	assert.Equal(t, DiagnosisInitialRequest, diagCustomer.Diagnosis)

	// Case 3: Session ID with trusted Governance Virtual Key ID -> Permitted
	ctxTrustedVK := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctxTrustedVK.SetValue(ctxSessionKey, "session_tenant_456")
	ctxTrustedVK.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyID, "vk_987654321")

	_, _, err = PreLLMHook(ctxTrustedVK, buildReq())
	require.NoError(t, err)
	diagVK, ok := GetRequestDiagnostic(ctxTrustedVK)
	require.True(t, ok)
	assert.True(t, diagVK.TrustedTenant)
	assert.Equal(t, DiagnosisInitialRequest, diagVK.Diagnosis)
}

// TestProviderAndModelIsolation verifies Task 3:
// Requests with the same session ID across different providers or models never share comparison history.
func TestProviderAndModelIsolation(t *testing.T) {
	systemText := "Diagnostic prompt"
	sessionID := "session_iso_test_01"
	tenantID := "tenant_isolated_corp"

	buildReq := func(provider schemas.ModelProvider, model string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			RequestType: schemas.ChatCompletionRequest,
			ChatRequest: &schemas.BifrostChatRequest{
				Provider: provider,
				Model:    model,
				Input: []schemas.ChatMessage{
					{
						Role:    schemas.ChatMessageRoleSystem,
						Content: &schemas.ChatMessageContent{ContentStr: &systemText},
					},
				},
			},
		}
	}

	// Turn 1: Provider OpenAI, model mock-gpt-4o
	ctx1 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx1.SetValue(ctxSessionKey, sessionID)
	ctx1.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, tenantID)

	_, _, err := PreLLMHook(ctx1, buildReq(schemas.OpenAI, "mock-gpt-4o"))
	require.NoError(t, err)
	_, _, err = PostLLMHook(ctx1, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{}}, nil)
	require.NoError(t, err)

	// Turn 2: Same session ID, same tenant, but Provider Anthropic, model claude-3-opus
	ctx2 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx2.SetValue(ctxSessionKey, sessionID)
	ctx2.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, tenantID)

	_, _, err = PreLLMHook(ctx2, buildReq(schemas.Anthropic, "claude-3-opus"))
	require.NoError(t, err)
	diag2, ok := GetRequestDiagnostic(ctx2)
	require.True(t, ok)
	assert.Equal(t, DiagnosisInitialRequest, diag2.Diagnosis, "Different provider must not share comparison history")
}

// TestNestedSchemaDifferenceDetection verifies Task 4:
// Exact JSON path differences are reported without storing raw schema content.
func TestNestedSchemaDifferenceDetection(t *testing.T) {
	sessionID := "session_nested_diff_01"
	tenantID := "tenant_nested_diff_corp"

	// Schema 1: properties.config.properties.timeout.type = "integer"
	schema1 := []byte(`{
		"type": "object",
		"properties": {
			"config": {
				"type": "object",
				"properties": {
					"timeout": {
						"type": "integer"
					}
				}
			}
		}
	}`)
	var params1 schemas.ToolFunctionParameters
	err := json.Unmarshal(schema1, &params1)
	require.NoError(t, err)

	// Schema 2: properties.config.properties.timeout.type = "string"
	schema2 := []byte(`{
		"type": "object",
		"properties": {
			"config": {
				"type": "object",
				"properties": {
					"timeout": {
						"type": "string"
					}
				}
			}
		}
	}`)
	var params2 schemas.ToolFunctionParameters
	err = json.Unmarshal(schema2, &params2)
	require.NoError(t, err)

	toolDesc := "Execute configuration"
	tool1 := schemas.ChatTool{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name:        "apply_config",
			Description: &toolDesc,
			Parameters:  &params1,
		},
	}
	tool2 := schemas.ChatTool{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name:        "apply_config",
			Description: &toolDesc,
			Parameters:  &params2,
		},
	}

	buildReq := func(tool schemas.ChatTool) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			RequestType: schemas.ChatCompletionRequest,
			ChatRequest: &schemas.BifrostChatRequest{
				Provider: schemas.OpenAI,
				Model:    "mock-gpt-4o",
				Params: &schemas.ChatParameters{
					Tools: []schemas.ChatTool{tool},
				},
			},
		}
	}

	// Turn 1
	ctx1 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx1.SetValue(ctxSessionKey, sessionID)
	ctx1.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, tenantID)

	_, _, err = PreLLMHook(ctx1, buildReq(tool1))
	require.NoError(t, err)
	_, _, err = PostLLMHook(ctx1, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{}}, nil)
	require.NoError(t, err)

	// Turn 2: modified timeout type
	ctx2 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx2.SetValue(ctxSessionKey, sessionID)
	ctx2.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, tenantID)

	_, _, err = PreLLMHook(ctx2, buildReq(tool2))
	require.NoError(t, err)
	diag2, ok := GetRequestDiagnostic(ctx2)
	require.True(t, ok)

	assert.Equal(t, DiagnosisConfirmedDrift, diag2.Diagnosis)
	assert.True(t, diag2.Drift.Detected)
	assert.Equal(t, "tools", diag2.Drift.Component)
	assert.Equal(t, "tools[0].function.parameters.properties.config.properties.timeout.type", diag2.Drift.Path)
	assert.Equal(t, "modified", diag2.Drift.ChangeType)
	assert.True(t, diag2.Drift.Confirmed)
}

// TestAddedAndRemovedTools verifies Task 4:
// Detecting when tools are added or removed between turns.
func TestAddedAndRemovedTools(t *testing.T) {
	sessionID := "session_tool_count_01"
	tenantID := "tenant_tool_count_corp"

	toolDesc := "Tool description"
	toolA := schemas.ChatTool{
		Type:     schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{Name: "tool_alpha", Description: &toolDesc},
	}
	toolB := schemas.ChatTool{
		Type:     schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{Name: "tool_beta", Description: &toolDesc},
	}

	buildReq := func(tools []schemas.ChatTool) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			RequestType: schemas.ChatCompletionRequest,
			ChatRequest: &schemas.BifrostChatRequest{
				Provider: schemas.OpenAI,
				Model:    "mock-gpt-4o",
				Params: &schemas.ChatParameters{
					Tools: tools,
				},
			},
		}
	}

	// Turn 1: 1 tool
	ctx1 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx1.SetValue(ctxSessionKey, sessionID)
	ctx1.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, tenantID)
	_, _, err := PreLLMHook(ctx1, buildReq([]schemas.ChatTool{toolA}))
	require.NoError(t, err)
	_, _, err = PostLLMHook(ctx1, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{}}, nil)
	require.NoError(t, err)

	// Turn 2: 2 tools (added)
	ctx2 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx2.SetValue(ctxSessionKey, sessionID)
	ctx2.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, tenantID)
	_, _, err = PreLLMHook(ctx2, buildReq([]schemas.ChatTool{toolA, toolB}))
	require.NoError(t, err)
	diag2, ok := GetRequestDiagnostic(ctx2)
	require.True(t, ok)
	assert.Equal(t, DiagnosisConfirmedDrift, diag2.Diagnosis)
	assert.Equal(t, "added", diag2.Drift.ChangeType)
	assert.Equal(t, "tools", diag2.Drift.Component)
	_, _, err = PostLLMHook(ctx2, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{}}, nil)
	require.NoError(t, err)

	// Turn 3: 1 tool (removed)
	ctx3 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx3.SetValue(ctxSessionKey, sessionID)
	ctx3.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, tenantID)
	_, _, err = PreLLMHook(ctx3, buildReq([]schemas.ChatTool{toolB}))
	require.NoError(t, err)
	diag3, ok := GetRequestDiagnostic(ctx3)
	require.True(t, ok)
	assert.Equal(t, DiagnosisConfirmedDrift, diag3.Diagnosis)
	assert.Equal(t, "removed", diag3.Drift.ChangeType)
}

// TestSessionTrackerBoundedMemoryAndEviction verifies Task 3:
// Memory bounding via MaxSessions and MaxEntries limits.
func TestSessionTrackerBoundedMemoryAndEviction(t *testing.T) {
	// Create tracker with max 2 sessions and max 2 entries per session
	tracker := NewSessionTracker(2, 2, 100*time.Millisecond)

	snap1 := RequestSnapshot{PrefixHMAC: "hmac1", Timestamp: time.Now()}
	snap2 := RequestSnapshot{PrefixHMAC: "hmac2", Timestamp: time.Now()}
	snap3 := RequestSnapshot{PrefixHMAC: "hmac3", Timestamp: time.Now()}

	tracker.Record("session_1", snap1)
	tracker.Record("session_2", snap2)
	assert.Equal(t, 2, tracker.Count())

	// Adding third session must evict oldest session (session_1)
	tracker.Record("session_3", snap3)
	assert.Equal(t, 2, tracker.Count())

	_, exists := tracker.GetLatest("session_1")
	assert.False(t, exists, "Oldest session must be evicted when maxSessions exceeded")

	latest3, exists3 := tracker.GetLatest("session_3")
	assert.True(t, exists3)
	assert.Equal(t, "hmac3", latest3.PrefixHMAC)

	// Test entry capacity per session (max 2 entries ring buffer)
	tracker.Record("session_3", snap1)
	tracker.Record("session_3", snap2)
	data := tracker.sessions["session_3"]
	assert.Equal(t, 2, len(data.Snapshots), "Ring buffer must not exceed maxEntries")
	assert.Equal(t, "hmac2", data.Snapshots[len(data.Snapshots)-1].PrefixHMAC)

	// Test TTL expiration
	time.Sleep(150 * time.Millisecond)
	_, existsAfterTTL := tracker.GetLatest("session_3")
	assert.False(t, existsAfterTTL, "Expired session must return false")
}

// TestConcurrentRequestsRaceDetector verifies thread safety with go test -race.
func TestConcurrentRequestsRaceDetector(t *testing.T) {
	const goroutines = 50
	const iterations = 5

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			sessID := fmt.Sprintf("session_race_%04d", id)
			tenantID := fmt.Sprintf("tenant_%d", id%5)

			for i := 0; i < iterations; i++ {
				ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
				ctx.SetValue(ctxSessionKey, sessID)
				ctx.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, tenantID)

				sys := fmt.Sprintf("System prompt %d", i)
				req := &schemas.BifrostRequest{
					RequestType: schemas.ChatCompletionRequest,
					ChatRequest: &schemas.BifrostChatRequest{
						Provider: schemas.OpenAI,
						Model:    "mock-gpt-4o",
						Input: []schemas.ChatMessage{
							{
								Role:    schemas.ChatMessageRoleSystem,
								Content: &schemas.ChatMessageContent{ContentStr: &sys},
							},
						},
					},
				}

				_, _, _ = PreLLMHook(ctx, req)
				_, _, _ = PostLLMHook(ctx, &schemas.BifrostResponse{
					ChatResponse: &schemas.BifrostChatResponse{
						Usage: &schemas.BifrostLLMUsage{
							PromptTokens: 100,
							PromptTokensDetails: &schemas.ChatPromptTokensDetails{
								CachedReadTokens: 50,
							},
						},
					},
				}, nil)

				// Test HTTP header hook concurrently
				respMeta := &schemas.HTTPResponseMetadata{
					Headers: make(map[string]string),
				}
				_ = HTTPTransportResponseHeadersHook(ctx, nil, respMeta)
			}
		}(g)
	}

	wg.Wait()
}

// -----------------------------------------------------------------------------
// ATTRIBUTION ENGINE UNIT TESTS (Tasks 2 & 3)
// -----------------------------------------------------------------------------

func createMockToolWithNestedSchema(nodeIDType string) schemas.ChatTool {
	desc := "Execute configuration"
	rawJSON := fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"node_id": {
				"type": "%s"
			}
		}
	}`, nodeIDType)
	var params schemas.ToolFunctionParameters
	_ = json.Unmarshal([]byte(rawJSON), &params)
	return schemas.ChatTool{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name:        "apply_config",
			Description: &desc,
			Parameters:  &params,
		},
	}
}

func TestAttribution_ClientInputChanged(t *testing.T) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	// Turn 1: Client sends string node_id
	tool1 := createMockToolWithNestedSchema("string")
	toolsHMAC1, paths1 := FlattenToolSchemas([]schemas.ChatTool{tool1})
	snap1 := RequestSnapshot{
		Timestamp:          time.Now(),
		PrefixHMAC:         computeHMAC(domainPrefixPrompt, []byte("System prompt")),
		ToolsHMAC:          toolsHMAC1,
		PathHMACs:          paths1,
		HasSystemPrompt:    true,
		ToolCount:          1,
		OutboundObservable: true,
		OutboundPrefixHMAC: computeHMAC(domainPrefixPrompt, []byte("System prompt")),
		OutboundToolsHMAC:  toolsHMAC1,
		OutboundPathHMACs:  paths1,
		OutboundToolCount:  1,
	}

	// Turn 2: Client deliberately changes node_id from string to integer
	tool2 := createMockToolWithNestedSchema("integer")
	toolsHMAC2, paths2 := FlattenToolSchemas([]schemas.ChatTool{tool2})
	snap2 := RequestSnapshot{
		Timestamp:          time.Now(),
		PrefixHMAC:         computeHMAC(domainPrefixPrompt, []byte("System prompt")),
		ToolsHMAC:          toolsHMAC2,
		PathHMACs:          paths2,
		HasSystemPrompt:    true,
		ToolCount:          1,
		OutboundObservable: true,
		OutboundPrefixHMAC: computeHMAC(domainPrefixPrompt, []byte("System prompt")),
		OutboundToolsHMAC:  toolsHMAC2,
		OutboundPathHMACs:  paths2,
		OutboundToolCount:  1,
	}

	drift := CompareSnapshots(snap1, snap2)
	assert.True(t, drift.Detected, "drift must be detected")
	assert.Equal(t, AttributionClientInputChanged, drift.Attribution, "must attribute to client_input_changed")
	assert.Equal(t, "tools", drift.Component)
	assert.Equal(t, "tools[0].function.parameters.properties.node_id.type", drift.Path)
	assert.Equal(t, "modified", drift.ChangeType)
	assert.Contains(t, drift.Reason, "Client input changed")
}

func TestAttribution_GatewayOutputChanged(t *testing.T) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	// Both turns have identical incoming client tool definitions
	clientTool := createMockToolWithNestedSchema("string")
	clientToolsHMAC, clientPaths := FlattenToolSchemas([]schemas.ChatTool{clientTool})

	// Turn 1: Outbound payload has 1 tool (clientTool)
	snap1 := RequestSnapshot{
		Timestamp:          time.Now(),
		PrefixHMAC:         computeHMAC(domainPrefixPrompt, []byte("System prompt")),
		ToolsHMAC:          clientToolsHMAC,
		PathHMACs:          clientPaths,
		HasSystemPrompt:    true,
		ToolCount:          1,
		OutboundObservable: true,
		OutboundPrefixHMAC: computeHMAC(domainPrefixPrompt, []byte("System prompt")),
		OutboundToolsHMAC:  clientToolsHMAC,
		OutboundPathHMACs:  clientPaths,
		OutboundToolCount:  1,
	}

	// Turn 2: Client input is EQUIVALENT (same toolsHMAC and prefixHMAC).
	// But gateway injected a server-side MCP tool into the outbound request!
	injectedMCPTool := schemas.ChatTool{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name: "mcp_cluster_doctor",
		},
	}
	outboundTools := []schemas.ChatTool{clientTool, injectedMCPTool}
	outboundToolsHMAC, outboundPaths := FlattenToolSchemas(outboundTools)

	snap2 := RequestSnapshot{
		Timestamp:          time.Now(),
		PrefixHMAC:         computeHMAC(domainPrefixPrompt, []byte("System prompt")), // IDENTICAL client input
		ToolsHMAC:          clientToolsHMAC,                                          // IDENTICAL client input
		PathHMACs:          clientPaths,                                              // IDENTICAL client input
		HasSystemPrompt:    true,
		ToolCount:          1,
		OutboundObservable: true,
		OutboundPrefixHMAC: computeHMAC(domainPrefixPrompt, []byte("System prompt")),
		OutboundToolsHMAC:  outboundToolsHMAC, // CHANGED OUTBOUND CONTENT
		OutboundPathHMACs:  outboundPaths,
		OutboundToolCount:  2,
	}

	drift := CompareSnapshots(snap1, snap2)
	assert.True(t, drift.Detected, "drift must be detected")
	assert.Equal(t, AttributionGatewayOutputChanged, drift.Attribution, "must attribute to gateway_output_changed")
	assert.Equal(t, "tools", drift.Component)
	assert.Equal(t, "added", drift.ChangeType)
	assert.Contains(t, drift.Reason, "Gateway transformation changed outbound request")
}

func TestAttribution_OriginUnverifiedWhenRawCaptureDisabled(t *testing.T) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	// Both turns have identical client input
	clientTool := createMockToolWithNestedSchema("string")
	clientToolsHMAC, clientPaths := FlattenToolSchemas([]schemas.ChatTool{clientTool})

	// In production, raw request capture is disabled by default (OutboundObservable = false)
	snap1 := RequestSnapshot{
		Timestamp:          time.Now(),
		PrefixHMAC:         computeHMAC(domainPrefixPrompt, []byte("System prompt")),
		ToolsHMAC:          clientToolsHMAC,
		PathHMACs:          clientPaths,
		HasSystemPrompt:    true,
		ToolCount:          1,
		OutboundObservable: false, // Disabled
	}

	snap2 := RequestSnapshot{
		Timestamp:          time.Now(),
		PrefixHMAC:         computeHMAC(domainPrefixPrompt, []byte("System prompt")),
		ToolsHMAC:          clientToolsHMAC,
		PathHMACs:          clientPaths,
		HasSystemPrompt:    true,
		ToolCount:          1,
		OutboundObservable: false, // Disabled
	}

	drift := CompareSnapshots(snap1, snap2)
	assert.False(t, drift.Detected, "no client drift detected")
	assert.Equal(t, AttributionOriginUnverified, drift.Attribution, "must attribute to origin_unverified")
	assert.Contains(t, drift.Reason, "outbound provider request is unobservable (raw capture disabled)")
}

func TestAttribution_StableInputAndOutput(t *testing.T) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	tool := createMockToolWithNestedSchema("string")
	toolsHMAC, paths := FlattenToolSchemas([]schemas.ChatTool{tool})

	snap1 := RequestSnapshot{
		Timestamp:          time.Now(),
		PrefixHMAC:         computeHMAC(domainPrefixPrompt, []byte("System prompt")),
		ToolsHMAC:          toolsHMAC,
		PathHMACs:          paths,
		HasSystemPrompt:    true,
		ToolCount:          1,
		OutboundObservable: true,
		OutboundPrefixHMAC: computeHMAC(domainPrefixPrompt, []byte("System prompt")),
		OutboundToolsHMAC:  toolsHMAC,
		OutboundPathHMACs:  paths,
		OutboundToolCount:  1,
	}

	snap2 := snap1
	snap2.Timestamp = time.Now()

	drift := CompareSnapshots(snap1, snap2)
	assert.False(t, drift.Detected)
	assert.Equal(t, AttributionNone, drift.Attribution)
	assert.Contains(t, drift.Reason, "Request structure is identical across client input and outbound provider request")
}

func TestAttribution_ProviderOrModelChangeDetection(t *testing.T) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	ctx1 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx1.SetValue(ctxSessionKey, "session-domain-switch-test")
	ctx1.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, "tenant_a")

	req1 := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o",
			Input: []schemas.ChatMessage{
				{
					Role:    schemas.ChatMessageRoleSystem,
					Content: &schemas.ChatMessageContent{ContentStr: func() *string { s := "Hello"; return &s }()},
				},
			},
		},
	}

	_, _, err := PreLLMHook(ctx1, req1)
	require.NoError(t, err)

	diag1, ok1 := GetRequestDiagnostic(ctx1)
	require.True(t, ok1)
	assert.False(t, diag1.DomainChanged, "first request should not be flagged as domain changed")

	// Turn 2 in same session switches to Anthropic claude-3-5-sonnet
	ctx2 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx2.SetValue(ctxSessionKey, "session-domain-switch-test")
	ctx2.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, "tenant_a")

	req2 := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.Anthropic,
			Model:    "claude-3-5-sonnet",
			Input: []schemas.ChatMessage{
				{
					Role:    schemas.ChatMessageRoleSystem,
					Content: &schemas.ChatMessageContent{ContentStr: func() *string { s := "Hello"; return &s }()},
				},
			},
		},
	}

	_, _, err = PreLLMHook(ctx2, req2)
	require.NoError(t, err)

	diag2, ok2 := GetRequestDiagnostic(ctx2)
	require.True(t, ok2)
	assert.True(t, diag2.DomainChanged, "provider or model switch must be detected")
	assert.Equal(t, "openai|gpt-4o", diag2.PreviousDomain)
	assert.Equal(t, DiagnosisInitialRequest, diag2.Diagnosis, "switching provider must create new isolated baseline; cannot compare across different provider caches")
}

func TestTransformation_InjectionDisabled(t *testing.T) {
	snap := RequestSnapshot{
		ClientCacheMarkerCount:   0,
		OutboundObservable:       true,
		OutboundCacheMarkerCount: 0,
	}
	finding := DetectTransformation(snap, "anthropic", "claude-3-5-sonnet")
	assert.Equal(t, TransformationNone, finding.Type)
	assert.False(t, finding.BifrostAdded)
	assert.False(t, finding.WarrantsReview)
}

func TestTransformation_InjectionEnabled(t *testing.T) {
	snap := RequestSnapshot{
		ClientCacheMarkerCount:   0,
		OutboundObservable:       true,
		OutboundCacheMarkerCount: 1,
		OutboundMarkerPaths:      []string{"messages[0].content[0].cache_control"},
	}
	finding := DetectTransformation(snap, "anthropic", "claude-3-5-sonnet")
	assert.Equal(t, TransformationCacheControlInjected, finding.Type)
	assert.True(t, finding.BifrostAdded)
	assert.True(t, finding.DiagnosticMode)
	assert.Equal(t, "messages[0].content[0].cache_control", finding.Path)
	assert.Equal(t, 0, finding.ClientMarkers)
	assert.Equal(t, 1, finding.OutboundMarkers)
	assert.False(t, finding.WarrantsReview)
	assert.Contains(t, finding.Summary, "auto-injection inserted 1 breakpoint")
}

func TestTransformation_ExistingClientBreakpointsPreserved(t *testing.T) {
	snap := RequestSnapshot{
		ClientCacheMarkerCount:   1,
		ClientMarkerPaths:        []string{"messages[0].content[0].cache_control"},
		OutboundObservable:       true,
		OutboundCacheMarkerCount: 1,
		OutboundMarkerPaths:      []string{"messages[0].content[0].cache_control"},
	}
	finding := DetectTransformation(snap, "anthropic", "claude-3-5-sonnet")
	assert.Equal(t, TransformationNone, finding.Type)
	assert.False(t, finding.BifrostAdded)
	assert.Equal(t, 1, finding.ClientMarkers)
	assert.Equal(t, 1, finding.OutboundMarkers)
}

func TestTransformation_MultipleBreakpointsClamped(t *testing.T) {
	snap := RequestSnapshot{
		ClientCacheMarkerCount:   5,
		ClientMarkerPaths:        []string{"messages[0].content[0].cache_control"},
		OutboundObservable:       true,
		OutboundCacheMarkerCount: 4,
		OutboundMarkerPaths:      []string{"messages[1].content[0].cache_control"},
	}
	finding := DetectTransformation(snap, "anthropic", "claude-3-5-sonnet")
	assert.Equal(t, TransformationCacheControlClamped, finding.Type)
	assert.False(t, finding.BifrostAdded)
	assert.Equal(t, 5, finding.ClientMarkers)
	assert.Equal(t, 4, finding.OutboundMarkers)
	assert.Contains(t, finding.Summary, "clamped cache breakpoints from 5 client markers to 4 outbound markers")
}

func TestTransformation_MissingOutboundCapture(t *testing.T) {
	snap := RequestSnapshot{
		ClientCacheMarkerCount: 0,
		OutboundObservable:     false,
	}
	finding := DetectTransformation(snap, "anthropic", "claude-3-5-sonnet")
	assert.Equal(t, TransformationNone, finding.Type)
	assert.False(t, finding.DiagnosticMode)
}

func TestTransformation_Issue9416_OpenRouterGeminiMultiMarker(t *testing.T) {
	snap := RequestSnapshot{
		ClientCacheMarkerCount: 2,
		ClientMarkerPaths: []string{
			"messages[0].content[0].cache_control",
			"messages[1].content[0].cache_control",
		},
		OutboundObservable:       true,
		OutboundCacheMarkerCount: 2,
	}
	// Target OpenRouter with Google Gemini model
	finding := DetectTransformation(snap, "openrouter", "google/gemini-2.5-pro")
	assert.Equal(t, TransformationMultiMarkerObserved, finding.Type)
	assert.False(t, finding.WarrantsReview, "multi-marker on OpenRouter+Gemini is officially supported (final marker used as prefix)")
	assert.Contains(t, finding.Summary, "final marker as the prompt-cache prefix")

	// Verify diagnostic model generation maps this to CG006 info
	diag := &RequestDiagnostic{
		Provider:       "openrouter",
		Model:          "google/gemini-2.5-pro",
		Transformation: finding,
		Snapshot:       snap,
	}
	findings := BuildDiagnosticFindings(diag)
	require.NotEmpty(t, findings)
	var foundCG006 bool
	for _, f := range findings {
		if f.Code == CodeMultiMarkerConfigObserved {
			foundCG006 = true
			assert.Equal(t, SeverityInfo, f.Severity)
			assert.Equal(t, ConfidenceObserved, f.Confidence)
			assert.Contains(t, f.SuggestedAction, "Verify that the final breakpoint corresponds to your intended static prompt prefix")
		}
	}
	assert.True(t, foundCG006, "expected CG006 diagnostic finding")
}

func TestTransformation_SensitiveDataRedaction(t *testing.T) {
	rawPayload := []byte(`{
		"model": "claude-3-5-sonnet",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "text",
						"text": "TOP_SECRET_PROMPT_CONTENT_MUST_NOT_LEAK",
						"cache_control": {"type": "ephemeral"}
					}
				]
			}
		]
	}`)

	count, paths := ExtractOutboundCacheMarkers(rawPayload)
	assert.Equal(t, 1, count)
	require.Len(t, paths, 1)
	assert.Equal(t, "messages[0].content[0].cache_control", paths[0])
	// Assert no part of the prompt is included in the path
	for _, p := range paths {
		assert.NotContains(t, p, "TOP_SECRET")
		assert.NotContains(t, p, "PROMPT")
	}
}

func TestAttribution_GatewayInjectionDoesNotTriggerFalseDrift(t *testing.T) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	// Turn 1 and Turn 2 both have identical client inputs, and Bifrost consistently
	// injects the cache breakpoint on both.
	snap1 := RequestSnapshot{
		Timestamp:                time.Now(),
		PrefixHMAC:               computeHMAC(domainPrefixPrompt, []byte("Prompt text")),
		HasSystemPrompt:          true,
		ClientCacheMarkerCount:   0,
		OutboundObservable:       true,
		OutboundPrefixHMAC:       computeHMAC(domainPrefixPrompt, []byte("Prompt text")),
		OutboundCacheMarkerCount: 1,
		OutboundMarkerPaths:      []string{"messages[0].content[0].cache_control"},
	}

	snap2 := snap1
	snap2.Timestamp = time.Now().Add(1 * time.Second)

	drift := CompareSnapshots(snap1, snap2)
	assert.False(t, drift.Detected, "consistent gateway injection across turns must NOT be flagged as drift")
	assert.Equal(t, AttributionNone, drift.Attribution, "consistent gateway behavior is AttributionNone")
}

// ============================================================================
// TASK 5: OPERATOR SCENARIO TESTS (A through I) & DIAGNOSTIC FINDINGS
// ============================================================================

func TestSanitizeFieldPath(t *testing.T) {
	// Standard safe path
	assert.Equal(t, "tools[0].function.parameters.properties.name.type",
		SanitizeFieldPath("tools[0].function.parameters.properties.name.type"))

	// Unsafe control characters and script tags stripped while preserving safe path brackets
	assert.Equal(t, "tools[0]scriptinjectionscript",
		SanitizeFieldPath("tools[0]\n\r\t<script>injection</script>"))

	// Long path bounded to 128 chars
	veryLong := "tools[0].function.parameters.properties." + strings.Repeat("nested_level_", 15)
	sanitized := SanitizeFieldPath(veryLong)
	assert.LessOrEqual(t, len(sanitized), 128)
	assert.True(t, strings.HasSuffix(sanitized, "..."))
}

func TestFindingRateLimiter(t *testing.T) {
	limiter := NewFindingRateLimiter(100*time.Millisecond, 10)
	now := time.Now()

	// First event should be allowed
	assert.True(t, limiter.ShouldLog("tenant1|CG001|tools[0]", now))

	// Repeated identical warning within cooldown must be suppressed
	assert.False(t, limiter.ShouldLog("tenant1|CG001|tools[0]", now.Add(10*time.Millisecond)))

	// Different scope key should be allowed
	assert.True(t, limiter.ShouldLog("tenant2|CG001|tools[0]", now.Add(20*time.Millisecond)))

	// After cooldown, should be allowed again
	assert.True(t, limiter.ShouldLog("tenant1|CG001|tools[0]", now.Add(150*time.Millisecond)))
}

func TestValidateAndApplyConfig(t *testing.T) {
	// 1. Defaults on nil
	cfgDefault, err := ValidateAndApplyConfig(nil)
	assert.NoError(t, err)
	assert.True(t, cfgDefault.Enabled)
	assert.True(t, cfgDefault.DiagnosticLogging)
	assert.Equal(t, "info", cfgDefault.SeverityThreshold)
	assert.Equal(t, DefaultMaxSessions, cfgDefault.MaxSessions)
	assert.Equal(t, DefaultMaxEntries, cfgDefault.MaxEntriesPerSession)
	assert.Equal(t, "30m", cfgDefault.SessionTTL)
	assert.False(t, cfgDefault.RawOutboundInspection)

	// 2. Custom struct
	custom := PluginConfig{
		Enabled:               true,
		DiagnosticLogging:     true,
		SeverityThreshold:     "warning",
		MaxSessions:           500,
		MaxEntriesPerSession:  5,
		SessionTTL:            "1h",
		RawOutboundInspection: false,
	}
	cfgCustom, err := ValidateAndApplyConfig(custom)
	assert.NoError(t, err)
	assert.Equal(t, "warning", cfgCustom.SeverityThreshold)
	assert.Equal(t, 500, cfgCustom.MaxSessions)
	assert.Equal(t, "1h", cfgCustom.SessionTTL)

	// 3. Invalid inputs clamped safely (fail-open)
	invalidJSON := map[string]any{
		"max_sessions":       -10,
		"session_ttl":        "invalid_duration",
		"severity_threshold": "unknown_level",
	}
	cfgClamped, err := ValidateAndApplyConfig(invalidJSON)
	assert.NoError(t, err)
	assert.Equal(t, DefaultMaxSessions, cfgClamped.MaxSessions)
	assert.Equal(t, "30m", cfgClamped.SessionTTL)
	assert.Equal(t, "info", cfgClamped.SeverityThreshold)
}

// Scenario A: Tool schema changed -> Clear diagnostic with safe field path (CG001)
func TestScenarioA_ToolSchemaChangedDiagnostic(t *testing.T) {
	diag := &RequestDiagnostic{
		SessionID: "sess-operator-a",
		TenantID:  "customer:cust-1",
		Drift: DriftResult{
			Detected:    true,
			Attribution: AttributionClientInputChanged,
			Component:   "tools",
			Path:        "tools[0].function.parameters.properties.city.type\n\t",
		},
		Snapshot: RequestSnapshot{
			OutboundObservable: true,
		},
	}
	findings := BuildDiagnosticFindings(diag)
	require.NotEmpty(t, findings)
	f := findings[0]
	assert.Equal(t, CodeToolSchemaChanged, f.Code)
	assert.Equal(t, SeverityWarning, f.Severity)
	assert.Equal(t, "tools", f.Component)
	assert.Equal(t, "tools[0].function.parameters.properties.city.type", f.Path)
	assert.Equal(t, EvidenceClientInput, f.EvidenceSource)
	assert.Equal(t, ConfidenceConfirmed, f.Confidence)
	assert.Contains(t, f.Message, "Tool parameter structure changed")
	assert.Contains(t, f.SuggestedAction, "Keep shared tool definitions consistent")
}

// Scenario B: Stable request with zero cached tokens -> Unknown-cause guidance, not a false eviction claim (CG003)
func TestScenarioB_StableMissUnknownCause(t *testing.T) {
	diag := &RequestDiagnostic{
		SessionID:        "sess-operator-b",
		TenantID:         "customer:cust-1",
		Diagnosis:        DiagnosisStableMiss,
		HasUsage:         true,
		HasPromptDetails: true,
		CachedReadTokens: 0,
		Snapshot: RequestSnapshot{
			OutboundObservable: true,
		},
	}
	findings := BuildDiagnosticFindings(diag)
	require.NotEmpty(t, findings)
	var foundCG003 bool
	for _, f := range findings {
		if f.Code == CodeStableMissUnknownCause {
			foundCG003 = true
			assert.Equal(t, SeverityInfo, f.Severity)
			assert.Equal(t, EvidenceProviderUsage, f.EvidenceSource)
			assert.Equal(t, ConfidenceObserved, f.Confidence)
			assert.Contains(t, f.Message, "Zero cached read tokens observed")
			assert.Contains(t, f.SuggestedAction, "Check provider cache eligibility, minimum prompt length, TTL, routing")
		}
	}
	assert.True(t, foundCG003)
}

// Scenario C: Bifrost injects a cache-control marker -> Verified transformation finding (CG002)
func TestScenarioC_GatewayBreakpointInjected(t *testing.T) {
	diag := &RequestDiagnostic{
		SessionID: "sess-operator-c",
		Transformation: TransformationFinding{
			Type:            TransformationCacheControlInjected,
			Component:       "cache_control",
			Path:            "messages[0].content[0].cache_control",
			OutboundMarkers: 1,
			BifrostAdded:    true,
		},
		Snapshot: RequestSnapshot{
			OutboundObservable: true,
		},
	}
	findings := BuildDiagnosticFindings(diag)
	require.NotEmpty(t, findings)
	var foundCG002 bool
	for _, f := range findings {
		if f.Code == CodeGatewayBreakpointInjected {
			foundCG002 = true
			assert.Equal(t, SeverityInfo, f.Severity)
			assert.Equal(t, EvidenceGatewayState, f.EvidenceSource)
			assert.Equal(t, ConfidenceConfirmed, f.Confidence)
			assert.Contains(t, f.Message, "Bifrost inserted a cache-control marker")
		}
	}
	assert.True(t, foundCG002)
}

// Scenario E: Outbound capture disabled -> Explicit verification limitation (CG004)
func TestScenarioE_OutboundCaptureDisabledLimitation(t *testing.T) {
	// Case 1: Unverified attribution with outbound capture disabled emits CG004
	diagUnverified := &RequestDiagnostic{
		SessionID:   "sess-operator-e",
		TenantID:    "customer:cust-1",
		Attribution: AttributionOriginUnverified,
		Snapshot: RequestSnapshot{
			OutboundObservable: false,
		},
	}
	findings := BuildDiagnosticFindings(diagUnverified)
	require.NotEmpty(t, findings)
	var foundCG004 bool
	for _, f := range findings {
		if f.Code == CodeOutboundVerificationUnavailable {
			foundCG004 = true
			assert.Equal(t, SeverityInfo, f.Severity)
			assert.False(t, f.OutboundObservable)
			assert.Contains(t, f.Message, "CacheGuard cannot determine whether provider-bound content changed")
			assert.Contains(t, f.SuggestedAction, "Use the isolated diagnostic workflow with synthetic requests")
		}
	}
	assert.True(t, foundCG004)

	// Case 2: Routine request with AttributionNone does NOT emit noisy CG004
	diagRoutine := &RequestDiagnostic{
		SessionID:   "sess-operator-e",
		TenantID:    "customer:cust-1",
		Attribution: AttributionNone,
		Diagnosis:   DiagnosisStableHit,
		Snapshot: RequestSnapshot{
			OutboundObservable: false,
		},
	}
	findingsRoutine := BuildDiagnosticFindings(diagRoutine)
	for _, f := range findingsRoutine {
		assert.NotEqual(t, CodeOutboundVerificationUnavailable, f.Code, "CG004 must not pollute routine requests")
	}
}

// Scenario F: No session ID -> Requests operate normally without cross-request comparison
func TestScenarioF_NoSessionIDNormalOperation(t *testing.T) {
	Init(PluginConfig{Enabled: true})
	defer Cleanup()

	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	strVal := "hello"
	// Untrusted or absent session ID
	req := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "gpt-4o",
			Provider: "openai",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &strVal}},
			},
		},
	}

	preReq, short, err := PreLLMHook(ctx, req)
	assert.NoError(t, err)
	assert.Nil(t, short)
	assert.Equal(t, req, preReq)

	diag, ok := GetRequestDiagnostic(ctx)
	assert.True(t, ok)
	assert.Equal(t, DiagnosisUntracked, diag.Diagnosis)
	assert.Equal(t, AttributionNone, diag.Attribution)
	assert.Empty(t, diag.SessionID)
}

// Scenario H & I: Invalid config and plugin disabled
func TestScenarioH_And_I_ConfigAndDisabledPlugin(t *testing.T) {
	// 1. Invalid configuration handled gracefully without failing Init
	err := Init("broken-non-json-config")
	assert.NoError(t, err, "Init must fail-open on invalid config")
	assert.True(t, isPluginEnabled(), "falls back to default enabled config")
	Cleanup()

	// 2. Disabled plugin passes through all hooks untouched
	err = Init(PluginConfig{Enabled: false})
	assert.NoError(t, err)
	assert.False(t, isPluginEnabled())

	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	req := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "claude-3-5-sonnet",
			Provider: "anthropic",
		},
	}

	// PreLLMHook must return immediately without incrementing counter
	oldCount := GetObservedRequestCount()
	preReq, short, err := PreLLMHook(ctx, req)
	assert.NoError(t, err)
	assert.Nil(t, short)
	assert.Equal(t, req, preReq)
	assert.Equal(t, oldCount, GetObservedRequestCount(), "disabled plugin must not count requests")

	// PostLLMHook must return response untouched
	resp := &schemas.BifrostResponse{}
	postResp, postErr, err := PostLLMHook(ctx, resp, nil)
	assert.NoError(t, err)
	assert.Nil(t, postErr)
	assert.Equal(t, resp, postResp)

	Cleanup()
}

// ============================================================================
// TASK 6: BENCHMARKS
// ============================================================================

func BenchmarkComputeHMAC(b *testing.B) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	data := []byte("You are an expert coding assistant specializing in prompt-caching gateways.")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = computeHMAC(domainPrefixPrompt, data)
	}
}

func BenchmarkFlattenToolSchemas(b *testing.B) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	desc := "Fetches weather information"
	var params schemas.ToolFunctionParameters
	_ = json.Unmarshal([]byte(`{
		"type": "object",
		"properties": {
			"city": {"type": "string"},
			"days": {"type": "integer"},
			"units": {"type": "string", "enum": ["celsius", "fahrenheit"]}
		},
		"required": ["city"]
	}`), &params)

	tools := []schemas.ChatTool{
		{
			Type: "function",
			Function: &schemas.ChatToolFunction{
				Name:        "get_weather",
				Description: &desc,
				Parameters:  &params,
			},
		},
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = FlattenToolSchemas(tools)
	}
}

func BenchmarkSessionTrackerLookup(b *testing.B) {
	tracker := NewSessionTracker(1000, 10, 30*time.Minute)
	key := "customer:123|anthropic|claude-3-5-sonnet|sess-bench"
	tracker.Record(key, RequestSnapshot{
		Timestamp:  time.Now(),
		PrefixHMAC: "hmac-prefix-sample-value",
	})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = tracker.GetLatest(key)
	}
}

func BenchmarkDiagnosticEvaluation(b *testing.B) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	diag := &RequestDiagnostic{
		SessionID: "sess-bench-1",
		TenantID:  "customer:123",
		Provider:  "anthropic",
		Model:     "claude-3-5-sonnet",
		Drift: DriftResult{
			Detected:    true,
			Attribution: AttributionClientInputChanged,
			Component:   "tools",
			Path:        "tools[0].function.parameters.properties.city.type",
		},
		Diagnosis: DiagnosisConfirmedDrift,
		Transformation: TransformationFinding{
			Type:            TransformationCacheControlInjected,
			Component:       "cache_control",
			Path:            "messages[0].content[0].cache_control",
			OutboundMarkers: 1,
			BifrostAdded:    true,
		},
		Snapshot: RequestSnapshot{
			OutboundObservable: true,
		},
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = BuildDiagnosticFindings(diag)
	}
}

// ============================================================================
// SECURITY AND CORRECTNESS AUDIT TESTS
// ============================================================================

func TestSecurity_DeeplyNestedSchemaRecursionBound(t *testing.T) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	// Build a 50-level deeply nested map to simulate algorithmic stack overflow attempt
	root := make(map[string]any)
	curr := root
	for i := 0; i < 50; i++ {
		next := make(map[string]any)
		curr["nested"] = next
		curr = next
	}
	curr["leaf"] = "value"

	var params schemas.ToolFunctionParameters
	b, err := json.Marshal(root)
	require.NoError(t, err)
	err = json.Unmarshal(b, &params)
	require.NoError(t, err)

	tools := []schemas.ChatTool{
		{
			Type: "function",
			Function: &schemas.ChatToolFunction{
				Name:       "deeply_nested_tool",
				Parameters: &params,
			},
		},
	}

	// Must terminate safely and quickly without stack overflow
	overallHMAC, paths := FlattenToolSchemas(tools)
	assert.NotEmpty(t, overallHMAC)
	assert.LessOrEqual(t, len(paths), maxSchemaRecursionDepth+2)
}

func TestSecurity_LargeSchemaPathCountBound(t *testing.T) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	// Build a schema with 1,000 distinct properties
	props := make(map[string]any)
	for i := 0; i < 1000; i++ {
		props[fmt.Sprintf("prop_%04d", i)] = map[string]any{"type": "string"}
	}
	root := map[string]any{
		"type":       "object",
		"properties": props,
	}

	var params schemas.ToolFunctionParameters
	b, err := json.Marshal(root)
	require.NoError(t, err)
	err = json.Unmarshal(b, &params)
	require.NoError(t, err)

	tools := []schemas.ChatTool{
		{
			Type: "function",
			Function: &schemas.ChatToolFunction{
				Name:       "huge_tool",
				Parameters: &params,
			},
		},
	}

	// Paths must be capped at maxFlattenedPaths (512)
	overallHMAC, paths := FlattenToolSchemas(tools)
	assert.NotEmpty(t, overallHMAC)
	assert.LessOrEqual(t, len(paths), maxFlattenedPaths)
}

func TestSecurity_HookPanicRecoveryFailOpen(t *testing.T) {
	Init(PluginConfig{Enabled: true})
	defer Cleanup()

	// Passing nil pointers or weird structures must fail-open without panicking
	assert.NotPanics(t, func() {
		_, _ = HTTPTransportPreHook(nil, nil)
	})

	assert.NotPanics(t, func() {
		req := &schemas.BifrostRequest{}
		outReq, short, err := PreLLMHook(nil, req)
		assert.NoError(t, err)
		assert.Nil(t, short)
		assert.Equal(t, req, outReq)
	})

	assert.NotPanics(t, func() {
		resp := &schemas.BifrostResponse{}
		outResp, outErr, err := PostLLMHook(nil, resp, nil)
		assert.NoError(t, err)
		assert.Nil(t, outErr)
		assert.Equal(t, resp, outResp)
	})

	assert.NotPanics(t, func() {
		_ = HTTPTransportResponseHeadersHook(nil, nil, nil)
	})
}

func TestCorrectness_MultiTurnConversationGrowthPreservesCachePrefix(t *testing.T) {
	Init(PluginConfig{Enabled: true})
	defer Cleanup()

	sessID := "sess-multiturn-grow"

	ctx1 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx1.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, "cust-growth")
	ctx1.SetValue(ctxSessionKey, sessID)

	sysPrompt := "You are a specialized code reviewer."
	user1 := "Hello! Can you help me review Go code?"
	assistant1 := "Of course! Please provide your Go code snippet."

	// Turn 1: System prompt + User 1
	req1 := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "claude-3-5-sonnet",
			Provider: "anthropic",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &sysPrompt}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &user1}},
			},
		},
	}
	_, _, err := PreLLMHook(ctx1, req1)
	require.NoError(t, err)

	resp1 := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Model: "claude-3-5-sonnet",
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens: 50,
			},
		},
	}
	_, _, err = PostLLMHook(ctx1, resp1, nil)
	require.NoError(t, err)

	// Turn 2: Normal conversation continuation: Turn 1 is appended with Assistant response and User Turn 2
	user2 := "Here is my function: func Add(a, b int) int { return a + b }"
	ctx2 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx2.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, "cust-growth")
	ctx2.SetValue(ctxSessionKey, sessID)

	req2 := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "claude-3-5-sonnet",
			Provider: "anthropic",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &sysPrompt}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &user1}},
				{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: &assistant1}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &user2}},
			},
		},
	}
	_, _, err = PreLLMHook(ctx2, req2)
	require.NoError(t, err)

	resp2 := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Model: "claude-3-5-sonnet",
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens: 100,
				PromptTokensDetails: &schemas.ChatPromptTokensDetails{
					CachedReadTokens: 50, // Prefix cache reuse!
				},
			},
		},
	}
	_, _, err = PostLLMHook(ctx2, resp2, nil)
	require.NoError(t, err)

	diag2, ok := GetRequestDiagnostic(ctx2)
	require.True(t, ok)
	assert.False(t, diag2.Drift.Detected, "appended conversation turns must NOT trigger false drift")
	assert.Equal(t, DiagnosisStableHit, diag2.Diagnosis, "reused prefix must be classified as stable_hit")
	assert.Equal(t, AttributionOriginUnverified, diag2.Attribution, "attribution is unverified when outbound capture is off")
}

func TestCorrectness_MultiTurnConversationPrefixMutationDetectsDrift(t *testing.T) {
	Init(PluginConfig{Enabled: true})
	defer Cleanup()

	sessID := "sess-multiturn-mutate"

	ctx1 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx1.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, "cust-mutate")
	ctx1.SetValue(ctxSessionKey, sessID)

	sys1 := "Original system prompt v1"
	user1 := "Hello"

	req1 := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "claude-3-5-sonnet",
			Provider: "anthropic",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &sys1}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &user1}},
			},
		},
	}
	_, _, err := PreLLMHook(ctx1, req1)
	require.NoError(t, err)

	resp1 := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Model: "claude-3-5-sonnet",
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 50},
		},
	}
	_, _, err = PostLLMHook(ctx1, resp1, nil)
	require.NoError(t, err)

	// Turn 2: Client alters the system prompt prefix!
	sys2 := "Altered system prompt v2 that busts cache"
	ctx2 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx2.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, "cust-mutate")
	ctx2.SetValue(ctxSessionKey, sessID)

	req2 := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "claude-3-5-sonnet",
			Provider: "anthropic",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &sys2}},
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &user1}},
			},
		},
	}
	_, _, err = PreLLMHook(ctx2, req2)
	require.NoError(t, err)

	diag2, ok := GetRequestDiagnostic(ctx2)
	require.True(t, ok)
	assert.True(t, diag2.Drift.Detected, "altered system prompt must be detected as drift")
	assert.Equal(t, AttributionClientInputChanged, diag2.Attribution)
	assert.Equal(t, DiagnosisConfirmedDrift, diag2.Diagnosis)
}

// TestHandling_ProviderErrorDoesNotCommitSnapshotOrEmitCG003 verifies that when a provider returns
// an error, the failed turn does NOT overwrite session history in tracker and does NOT emit false CG003 warnings.
func TestHandling_ProviderErrorDoesNotCommitSnapshotOrEmitCG003(t *testing.T) {
	InitEngine(100, 10, 5*time.Minute)
	defer CleanupEngine()

	sessID := "sess-error-guard-01"
	custID := "cust-err-test"

	// Turn 1: Healthy request
	ctx1 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx1.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, custID)
	ctx1.SetValue(ctxSessionKey, sessID)

	prompt1 := "Valid initial instruction"
	req1 := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "gpt-4o",
			Provider: "openai",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &prompt1}},
			},
		},
	}
	_, _, err := PreLLMHook(ctx1, req1)
	require.NoError(t, err)

	resp1 := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens: 100,
				PromptTokensDetails: &schemas.ChatPromptTokensDetails{
					CachedReadTokens: 50,
				},
			},
		},
	}
	_, _, err = PostLLMHook(ctx1, resp1, nil)
	require.NoError(t, err)

	// Verify tracker recorded Turn 1
	compKey := fmt.Sprintf("customer:%s|openai|gpt-4o|%s", custID, sessID)
	snap1, exists := globalTracker.GetLatest(compKey)
	require.True(t, exists)

	// Turn 2: Request fails at provider with 500 error
	ctx2 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx2.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, custID)
	ctx2.SetValue(ctxSessionKey, sessID)

	prompt2 := "A changed prompt during failure"
	req2 := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "gpt-4o",
			Provider: "openai",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &prompt2}},
			},
		},
	}
	_, _, err = PreLLMHook(ctx2, req2)
	require.NoError(t, err)

	code500 := 500
	providerErr := &schemas.BifrostError{
		StatusCode: &code500,
		Error: &schemas.ErrorField{
			Message: "Internal server error from provider",
		},
	}
	_, outErr, err := PostLLMHook(ctx2, nil, providerErr)
	require.NoError(t, err)
	assert.NotNil(t, outErr)

	// Verify that Turn 2 failure was NOT committed to the tracker (snapshot must still match Turn 1)
	snapAfterErr, exists := globalTracker.GetLatest(compKey)
	require.True(t, exists)
	assert.Equal(t, snap1.PrefixHMAC, snapAfterErr.PrefixHMAC, "tracker snapshot must not be replaced by a failed request")

	// Verify that CG003 was NOT emitted on error
	diag2, ok := GetRequestDiagnostic(ctx2)
	require.True(t, ok)
	for _, f := range diag2.Findings {
		assert.NotEqual(t, CodeStableMissUnknownCause, f.Code, "failed request must never emit CG003")
	}
}

// TestHandling_MissingUsageDoesNotEmitCG003 verifies that when a provider does not report
// token usage (e.g. usage block is nil), CacheGuard does not falsely diagnose a stable_miss.
func TestHandling_MissingUsageDoesNotEmitCG003(t *testing.T) {
	InitEngine(100, 10, 5*time.Minute)
	defer CleanupEngine()

	sessID := "sess-no-usage-01"
	custID := "cust-no-usage"

	// Turn 1: Initial request
	ctx1 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx1.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, custID)
	ctx1.SetValue(ctxSessionKey, sessID)

	prompt := "Repeat prompt without usage"
	req1 := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "gpt-4o",
			Provider: "openai",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &prompt}},
			},
		},
	}
	_, _, _ = PreLLMHook(ctx1, req1)
	resp1 := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: nil, // provider omitted usage metadata
		},
	}
	_, _, _ = PostLLMHook(ctx1, resp1, nil)

	// Turn 2: Repeat stable request, again with nil usage
	ctx2 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx2.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, custID)
	ctx2.SetValue(ctxSessionKey, sessID)

	req2 := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "gpt-4o",
			Provider: "openai",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &prompt}},
			},
		},
	}
	_, _, err := PreLLMHook(ctx2, req2)
	require.NoError(t, err)

	resp2 := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: nil, // usage metadata absent
		},
	}
	_, _, err = PostLLMHook(ctx2, resp2, nil)
	require.NoError(t, err)

	diag2, ok := GetRequestDiagnostic(ctx2)
	require.True(t, ok)
	assert.False(t, diag2.HasUsage)
	assert.NotEqual(t, DiagnosisStableMiss, diag2.Diagnosis, "absent usage must not be labeled stable_miss")

	for _, f := range diag2.Findings {
		assert.NotEqual(t, CodeStableMissUnknownCause, f.Code, "missing usage metadata must not emit CG003")
	}
}

// ============================================================================
// TASK 3 REGRESSION TESTS — BOUNDED MEMORY & CONCURRENCY
// ============================================================================

func TestSessionDomainTracker_BoundedCapacityAndEviction(t *testing.T) {
	tracker := NewSessionDomainTracker(10, 5*time.Minute)
	defer tracker.Clear()

	// Insert 20 distinct sessions into a tracker with capacity 10
	for i := 0; i < 20; i++ {
		sessID := fmt.Sprintf("sess-%d", i)
		changed, _ := tracker.CheckAndRecord("t1", sessID, "openai", "gpt-4o")
		assert.False(t, changed)
	}

	// Verify tracker size is strictly bounded to maxSessions
	assert.LessOrEqual(t, tracker.Count(), 10, "domain tracker must not exceed max capacity")

	// Oldest entries (e.g. sess-0..9) should have been evicted; newest (e.g. sess-19) present
	changed, prev := tracker.CheckAndRecord("t1", "sess-19", "openai", "gpt-4o")
	assert.False(t, changed, "recent session should match without domain change")
	assert.Equal(t, "openai|gpt-4o", prev)
}

func TestSessionDomainTracker_TTLEviction(t *testing.T) {
	tracker := NewSessionDomainTracker(100, 50*time.Millisecond)
	defer tracker.Clear()

	sessID := "sess-expire-test"
	changed, prev := tracker.CheckAndRecord("t1", sessID, "openai", "gpt-4o")
	assert.False(t, changed)
	assert.Equal(t, "", prev)

	// Wait for TTL expiration
	time.Sleep(70 * time.Millisecond)

	// Record with different domain: since previous expired, it should be treated as initial/new
	changedAfterExpire, prevAfterExpire := tracker.CheckAndRecord("t1", sessID, "anthropic", "claude-3-5-sonnet")
	assert.False(t, changedAfterExpire, "expired session domain should not flag change on new baseline")
	assert.Equal(t, "", prevAfterExpire)
}

func TestSessionDomainTracker_ConcurrentAdversarialLoad(t *testing.T) {
	const maxCap = 25
	tracker := NewSessionDomainTracker(maxCap, 1*time.Minute)
	defer tracker.Clear()

	var wg sync.WaitGroup
	const goroutines = 20
	const opsPerGoroutine = 100

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				sessID := fmt.Sprintf("sess-%d-%d", gid, i)
				tracker.CheckAndRecord(fmt.Sprintf("t%d", gid), sessID, fmt.Sprintf("prov-%d", gid%3), fmt.Sprintf("mod-%d", i%2))
			}
		}(g)
	}

	wg.Wait()
	assert.LessOrEqual(t, tracker.Count(), maxCap, "concurrent load must not breach maxSessions boundary")
}

// ============================================================================
// TASK 4 REGRESSION TESTS — FINGERPRINT BOUNDARY & INCOMPLETE STATUS
// ============================================================================

func TestFingerprint_MultipleSystemAndDeveloperMessages(t *testing.T) {
	InitEngine(100, 10, 5*time.Minute)
	defer CleanupEngine()

	sys1 := "You are a helpful assistant."
	sys2 := "Always respond concisely."
	devMsg := "Developer instruction: no formatting."

	req1 := &schemas.BifrostChatRequest{
		Model:    "gpt-4o",
		Provider: "openai",
		Input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &sys1}},
			{Role: schemas.ChatMessageRoleDeveloper, Content: &schemas.ChatMessageContent{ContentStr: &devMsg}},
			{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &sys2}},
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &sys1}},
		},
	}

	hmac1, _ := ExtractPrefixSnapshot(req1)
	assert.NotEmpty(t, hmac1)

	// Mutate the second system message
	sys2Mutated := "Always respond with extreme detail."
	req2 := &schemas.BifrostChatRequest{
		Model:    "gpt-4o",
		Provider: "openai",
		Input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &sys1}},
			{Role: schemas.ChatMessageRoleDeveloper, Content: &schemas.ChatMessageContent{ContentStr: &devMsg}},
			{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &sys2Mutated}},
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &sys1}},
		},
	}

	hmac2, _ := ExtractPrefixSnapshot(req2)
	assert.NotEqual(t, hmac1, hmac2, "mutating any system message must alter system HMAC")

	snap1 := RequestSnapshot{PrefixHMAC: hmac1}
	snap2 := RequestSnapshot{PrefixHMAC: hmac2}
	drift := CompareSnapshots(snap1, snap2)
	assert.True(t, drift.Detected)
	assert.Equal(t, "system_prompt", drift.Component)
}

func TestFingerprint_MultimodalContent(t *testing.T) {
	InitEngine(100, 10, 5*time.Minute)
	defer CleanupEngine()

	imgURL1 := "https://example.com/image1.png"
	imgURL2 := "https://example.com/image2.png"
	textBlock := "Analyze this image"

	req1 := &schemas.BifrostChatRequest{
		Model:    "gpt-4o",
		Provider: "openai",
		Input: []schemas.ChatMessage{
			{
				Role: schemas.ChatMessageRoleSystem,
				Content: &schemas.ChatMessageContent{
					ContentBlocks: []schemas.ChatContentBlock{
						{Type: schemas.ChatContentBlockTypeText, Text: &textBlock},
						{Type: schemas.ChatContentBlockTypeImage, ImageURLStruct: &schemas.ChatInputImage{URL: imgURL1}},
					},
				},
			},
		},
	}

	req2 := &schemas.BifrostChatRequest{
		Model:    "gpt-4o",
		Provider: "openai",
		Input: []schemas.ChatMessage{
			{
				Role: schemas.ChatMessageRoleSystem,
				Content: &schemas.ChatMessageContent{
					ContentBlocks: []schemas.ChatContentBlock{
						{Type: schemas.ChatContentBlockTypeText, Text: &textBlock},
						{Type: schemas.ChatContentBlockTypeImage, ImageURLStruct: &schemas.ChatInputImage{URL: imgURL2}},
					},
				},
			},
		},
	}

	hmac1, _ := ExtractPrefixSnapshot(req1)
	hmac2, _ := ExtractPrefixSnapshot(req2)

	assert.NotEqual(t, hmac1, hmac2, "different multimodal image URLs must alter HMAC")
}

func TestFingerprint_OversizedToolDescriptionSetsIncomplete(t *testing.T) {
	InitEngine(100, 10, 5*time.Minute)
	defer CleanupEngine()

	hugeDesc := strings.Repeat("A", 2500) // Exceeds 2048 bytes
	var params schemas.ToolFunctionParameters
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"id":{"type":"string"}}}`), &params)

	tools := []schemas.ChatTool{
		{
			Type: schemas.ChatToolTypeFunction,
			Function: &schemas.ChatToolFunction{
				Name:        "huge_tool",
				Description: &hugeDesc,
				Parameters:  &params,
			},
		},
	}

	_, _, incomplete := FlattenToolSchemasWithStatus(tools)
	assert.True(t, incomplete, "tool description exceeding 2048 bytes must flag incomplete")

	snap1 := RequestSnapshot{
		PrefixHMAC:           "same-sys",
		ToolsHMAC:            "same-tools",
		ComparisonIncomplete: true,
	}
	snap2 := RequestSnapshot{
		PrefixHMAC:           "same-sys",
		ToolsHMAC:            "same-tools",
		ComparisonIncomplete: true,
	}

	// CompareSnapshots must NOT falsely classify as confirmed stable!
	drift := CompareSnapshots(snap1, snap2)
	assert.False(t, drift.Detected)
	assert.Equal(t, AttributionOriginUnverified, drift.Attribution, "incomplete comparison must be marked unverified, never confirmed stable")
	assert.Contains(t, drift.Reason, "comparison incomplete")
}

func TestFingerprint_LargeSchemaPathLimitSetsIncomplete(t *testing.T) {
	InitEngine(100, 10, 5*time.Minute)
	defer CleanupEngine()

	props := make(map[string]any)
	for i := 0; i < 550; i++ {
		props[fmt.Sprintf("field_%d", i)] = map[string]any{"type": "string"}
	}
	wideBytes, _ := json.Marshal(map[string]any{
		"type":       "object",
		"properties": props,
	})
	var wideParams schemas.ToolFunctionParameters
	_ = json.Unmarshal(wideBytes, &wideParams)

	tools := []schemas.ChatTool{
		{
			Type: schemas.ChatToolTypeFunction,
			Function: &schemas.ChatToolFunction{
				Name:       "wide_tool",
				Parameters: &wideParams,
			},
		},
	}

	_, _, incomplete := FlattenToolSchemasWithStatus(tools)
	assert.True(t, incomplete, "schema exceeding 512 paths must flag incomplete")
}

// ============================================================================
// TASK 5 REGRESSION TESTS — CACHE METRICS & STREAMING RESPONSE HEADERS
// ============================================================================

func TestCacheMetric_MissingPromptTokensDetailsDoesNotEmitStableMiss(t *testing.T) {
	InitEngine(100, 10, 5*time.Minute)
	defer CleanupEngine()

	sessID := "sess-details-nil-01"
	custID := "cust-details-nil"

	// Turn 1
	ctx1 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx1.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, custID)
	ctx1.SetValue(ctxSessionKey, sessID)

	prompt := "Stable prompt"
	req1 := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "gpt-4o",
			Provider: "openai",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &prompt}},
			},
		},
	}
	_, _, _ = PreLLMHook(ctx1, req1)
	resp1 := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 100},
		},
	}
	_, _, _ = PostLLMHook(ctx1, resp1, nil)

	// Turn 2: Repeat stable request, provider returns usage with PromptTokensDetails = nil
	ctx2 := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx2.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, custID)
	ctx2.SetValue(ctxSessionKey, sessID)

	req2 := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "gpt-4o",
			Provider: "openai",
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &prompt}},
			},
		},
	}
	_, _, _ = PreLLMHook(ctx2, req2)

	// Verify in PreLLMHook that diagnosis is structurally_stable, NOT stable_miss
	diagPre, ok := GetRequestDiagnostic(ctx2)
	require.True(t, ok)
	assert.Equal(t, DiagnosisStructurallyStable, diagPre.Diagnosis, "PreLLMHook must assign structurally_stable before usage is known")

	// PostLLMHook with PromptTokensDetails = nil
	resp2 := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens:        100,
				PromptTokensDetails: nil, // provider omitted details
			},
		},
	}
	_, _, err := PostLLMHook(ctx2, resp2, nil)
	require.NoError(t, err)

	diagPost, ok := GetRequestDiagnostic(ctx2)
	require.True(t, ok)
	assert.Equal(t, DiagnosisUsageUnavailable, diagPost.Diagnosis, "missing PromptTokensDetails must result in usage_unavailable, NOT stable_miss")

	// Verify CG003 is NOT emitted
	for _, f := range diagPost.Findings {
		assert.NotEqual(t, CodeStableMissUnknownCause, f.Code, "missing PromptTokensDetails must not emit CG003")
	}
}

func TestCacheMetric_ResolvesHitAndMissWhenDetailsPresent(t *testing.T) {
	InitEngine(100, 10, 5*time.Minute)
	defer CleanupEngine()

	// Hit case
	diagHit := &RequestDiagnostic{
		StructurallyStable: true,
		Diagnosis:          DiagnosisStructurallyStable,
	}
	ctxHit := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctxHit.SetValue(ctxDiagKey, diagHit)
	respHit := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens: 100,
				PromptTokensDetails: &schemas.ChatPromptTokensDetails{
					CachedReadTokens: 64,
				},
			},
		},
	}
	_, _, _ = PostLLMHook(ctxHit, respHit, nil)
	assert.Equal(t, DiagnosisStableHit, diagHit.Diagnosis)
	assert.Equal(t, 64, diagHit.CachedReadTokens)

	// Miss case (explicit zero cached tokens)
	diagMiss := &RequestDiagnostic{
		StructurallyStable: true,
		Diagnosis:          DiagnosisStructurallyStable,
	}
	ctxMiss := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctxMiss.SetValue(ctxDiagKey, diagMiss)
	respMiss := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens: 100,
				PromptTokensDetails: &schemas.ChatPromptTokensDetails{
					CachedReadTokens: 0,
				},
			},
		},
	}
	_, _, _ = PostLLMHook(ctxMiss, respMiss, nil)
	assert.Equal(t, DiagnosisStableMiss, diagMiss.Diagnosis)
	assert.Equal(t, 0, diagMiss.CachedReadTokens)
}

// ============================================================================
// TASK 6 REGRESSION TESTS — PRIVACY & DATA LEAK REDACTION
// ============================================================================

func TestPrivacy_HeadersOmitSensitiveFields(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	diag := &RequestDiagnostic{
		Diagnosis:      DiagnosisConfirmedDrift,
		Attribution:    AttributionClientInputChanged,
		DomainChanged:  true,
		PreviousDomain: "secret-provider|secret-model",
		Drift: DriftResult{
			Detected:   true,
			Component:  "tools",
			Path:       "tools[0].function.parameters.properties.raw_secret_key.type\n\r",
			ChangeType: "modified",
			Reason:     "client secret param was modified",
		},
		HasUsage:         true,
		HasPromptDetails: false, // Cached-Tokens should NOT be emitted
		PromptTokens:     100,
		CachedReadTokens: 0,
	}
	ctx.SetValue(ctxDiagKey, diag)

	meta := &schemas.HTTPResponseMetadata{
		Headers: make(map[string]string),
	}

	err := HTTPTransportResponseHeadersHook(ctx, nil, meta)
	require.NoError(t, err)

	// Sensitive fields must be omitted
	assert.Empty(t, meta.Headers["X-Cacheguard-Previous-Domain"], "previous domain must be omitted from headers")
	assert.Empty(t, meta.Headers["X-Cacheguard-Drift-Reason"], "drift reason derived from client must be omitted")
	assert.Empty(t, meta.Headers["X-Cacheguard-Cached-Tokens"], "cached tokens must be omitted when details unavailable")

	// Domain changed flag is kept
	assert.Equal(t, "true", meta.Headers["X-Cacheguard-Domain-Changed"])

	// Detailed schema path is opt-in via server config only: default is false, so path must be omitted for privacy
	assert.Empty(t, meta.Headers["X-Cacheguard-Drift-Path"], "detailed schema path must be omitted by default to avoid leaking property names")

	// Verify client-supplied header CANNOT bypass operator privacy policy
	metaClientAttempt := &schemas.HTTPResponseMetadata{
		Headers: make(map[string]string),
	}
	reqClientAttempt := &schemas.HTTPRequest{
		Headers: map[string]string{
			"x-cacheguard-expose-paths": "true",
		},
	}
	err = HTTPTransportResponseHeadersHook(ctx, reqClientAttempt, metaClientAttempt)
	require.NoError(t, err)
	assert.Empty(t, metaClientAttempt.Headers["X-Cacheguard-Drift-Path"], "arbitrary client header must not enable schema path disclosure")

	// When explicitly opted in via trusted SERVER configuration, path is emitted and sanitized
	_ = Init(map[string]any{
		"expose_schema_paths": true,
	})
	defer func() {
		_ = Init(map[string]any{
			"expose_schema_paths": false,
		})
	}()

	metaServerOptIn := &schemas.HTTPResponseMetadata{
		Headers: make(map[string]string),
	}
	err = HTTPTransportResponseHeadersHook(ctx, nil, metaServerOptIn)
	require.NoError(t, err)
	cleanPath := metaServerOptIn.Headers["X-Cacheguard-Drift-Path"]
	assert.NotEmpty(t, cleanPath, "detailed schema path should be emitted when enabled by server config")
	assert.NotContains(t, cleanPath, "\n")
	assert.NotContains(t, cleanPath, "\r")
}

func TestCacheMetric_NonNilDetailsWithoutCacheMetricSuppressesCG003(t *testing.T) {
	InitEngine(100, 10, 5*time.Minute)
	defer CleanupEngine()

	// Stable turn with PromptTokensDetails present, but containing ONLY non-cache tokens (e.g. AudioTokens)
	diag := &RequestDiagnostic{
		StructurallyStable: true,
		Diagnosis:          DiagnosisStructurallyStable,
	}
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	ctx.SetValue(ctxDiagKey, diag)

	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens: 120,
				PromptTokensDetails: &schemas.ChatPromptTokensDetails{
					AudioTokens:      25,
					CachedReadTokens: 0, // Go normalized absent cached_read_tokens to zero!
				},
			},
		},
	}

	_, _, err := PostLLMHook(ctx, resp, nil)
	require.NoError(t, err)

	// Must NOT label as stable_miss because the provider never supplied cache metrics
	assert.Equal(t, DiagnosisUsageUnavailable, diag.Diagnosis, "non-nil details with only audio/image tokens must be diagnosed usage_unavailable")
	assert.False(t, diag.HasPromptDetails, "HasPromptDetails must be false when no cache metric was provided")

	findings := BuildDiagnosticFindings(diag)
	for _, f := range findings {
		assert.NotEqual(t, CodeStableMissUnknownCause, f.Code, "CG003 must be suppressed when provider never provided cache metric")
	}
}

func TestSecurity_HMACConcurrencyWithCleanup(t *testing.T) {
	var wg sync.WaitGroup
	const goroutines = 20

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				data := []byte(fmt.Sprintf("concurrent-payload-%d-%d", idx, j))
				_ = computeHMAC(domainPrefixPrompt, data)
				if j%10 == 0 {
					// Simulate concurrent key initialization
					InitEngine(100, 10, 1*time.Minute)
				}
			}
		}(i)
	}

	wg.Wait()
}

// ============================================================================
// MICRO-BENCHMARKS: CPU & ALLOCATION PROFILING FOR NESTED TOOLS WORKLOAD
// ============================================================================

func BenchmarkPreLLMHook_NestedTools(b *testing.B) {
	InitEngine(1000, 10, 30*time.Minute)
	defer CleanupEngine()

	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"query": {"type": "string"},
			"filters": {
				"type": "object",
				"properties": {
					"status": {"type": "string"},
					"range": {
						"type": "object",
						"properties": {
							"start": {"type": "integer"},
							"end": {"type": "integer"}
						}
					}
				}
			}
		}
	}`)
	var params schemas.ToolFunctionParameters
	_ = json.Unmarshal(schemaJSON, &params)

	toolDesc := "Query metrics with filters"
	tools := []schemas.ChatTool{
		{
			Type: schemas.ChatToolTypeFunction,
			Function: &schemas.ChatToolFunction{
				Name:        "query_metrics",
				Description: &toolDesc,
				Parameters:  &params,
			},
		},
	}

	sysPrompt := "You are an analytical assistant."
	req := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Model:    "gpt-4o",
			Provider: schemas.OpenAI,
			Input: []schemas.ChatMessage{
				{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &sysPrompt}},
			},
			Params: &schemas.ChatParameters{
				Tools: tools,
			},
		},
	}

	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(10*time.Second))
	ctx.SetValue(schemas.BifrostContextKeyGovernanceCustomerID, "bench-tenant")
	ctx.SetValue(ctxSessionKey, "bench-session-id")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, _, _ = PreLLMHook(ctx, req)
	}
}

func BenchmarkToolSchema_FlatteningOnly(b *testing.B) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"query": {"type": "string"},
			"filters": {
				"type": "object",
				"properties": {
					"status": {"type": "string"},
					"range": {
						"type": "object",
						"properties": {
							"start": {"type": "integer"},
							"end": {"type": "integer"}
						}
					}
				}
			}
		}
	}`)
	var params schemas.ToolFunctionParameters
	_ = json.Unmarshal(schemaJSON, &params)
	toolDesc := "Query metrics"
	tools := []schemas.ChatTool{
		{
			Type: schemas.ChatToolTypeFunction,
			Function: &schemas.ChatToolFunction{
				Name:        "query_metrics",
				Description: &toolDesc,
				Parameters:  &params,
			},
		},
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, _, _ = FlattenToolSchemasWithStatus(tools)
	}
}

func BenchmarkHMAC_ComputeOnly(b *testing.B) {
	InitEngine(100, 10, 10*time.Minute)
	defer CleanupEngine()

	data := []byte("system:You are a high-performance Bifrost AI gateway tester;tools:query_metrics:v1;")
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = computeHMAC(domainPrefixPrompt, data)
	}
}

func TestStreamingTransformationTiming_HeadersHookDoesNotEmitPrematureCG002OrCG007(t *testing.T) {
	InitEngine(100, 10, 5*time.Minute)
	defer CleanupEngine()

	// In a streaming request lifecycle:
	// 1. PreLLMHook runs and sets up diag
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(5*time.Second))
	chatReq := &schemas.BifrostChatRequest{
		Model: "claude-3-5-sonnet-20241022",
		Input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("Hello")}},
		},
	}
	req := &schemas.BifrostRequest{ChatRequest: chatReq}
	_, _, err := PreLLMHook(ctx, req)
	require.NoError(t, err)

	// 2. HTTPTransportResponseHeadersHook runs BEFORE stream chunks arrive.
	// At this stage, PostLLMHook has not run, so RawRequest has not been observed.
	// Transformation must remain None, and neither CG002 nor CG007 can appear in headers.
	meta := &schemas.HTTPResponseMetadata{
		Headers: make(map[string]string),
	}
	err = HTTPTransportResponseHeadersHook(ctx, nil, meta)
	require.NoError(t, err)

	assert.Empty(t, meta.Headers["X-Cacheguard-Transformation"], "transformation cannot be emitted before PostLLMHook observes outbound request")
	assert.Empty(t, meta.Headers["X-Cacheguard-Injected-Path"], "injected path cannot be emitted prematurely")
	assert.Empty(t, meta.Headers["X-Cacheguard-Finding-Code"], "CG002/CG007 must not appear in streaming response headers before observation")

	// 3. Subsequently, in PostLLMHook (chunk 1), raw request is observed
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			ExtraFields: schemas.BifrostResponseExtraFields{
				RawRequest: map[string]any{
					"messages": []any{
						map[string]any{
							"role": "user",
							"content": []any{
								map[string]any{
									"type": "text",
									"text": "Hello",
									"cache_control": map[string]any{
										"type": "ephemeral",
									},
								},
							},
						},
					},
				},
			},
		},
	}
	// PostLLMHook observes the transformation
	_, _, err = PostLLMHook(ctx, resp, nil)
	require.NoError(t, err)

	// Findings are now available post-stream
	diagVal := ctx.Value(ctxDiagKey)
	require.NotNil(t, diagVal)
	diag := diagVal.(*RequestDiagnostic)
	assert.Equal(t, TransformationCacheControlInjected, diag.Transformation.Type)
	require.NotNil(t, diag.PrimaryFinding)
	assert.Equal(t, CodeGatewayBreakpointInjected, diag.PrimaryFinding.Code)
}

