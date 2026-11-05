package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

const (
	PluginName                              = "cacheguard"
	ctxDiagKey    schemas.BifrostContextKey = "cacheguard_diagnostic_key"
	ctxSessionKey schemas.BifrostContextKey = "cacheguard_session_key"
)

// RequestDiagnostic holds non-sensitive metadata for the current request.
// NEVER stores raw prompts, message contents, or authorization headers.
type RequestDiagnostic struct {
	SessionID          string                `json:"session_id,omitempty"`
	CompositeKey       string                `json:"-"`
	TrustedTenant      bool                  `json:"trusted_tenant"`
	TenantID           string                `json:"tenant_id,omitempty"`
	Model              string                `json:"model"`
	Provider           string                `json:"provider"`
	ToolCount          int                   `json:"tool_count"`
	HasSystemPrompt    bool                  `json:"has_system_prompt"`
	Drift              DriftResult           `json:"drift"`
	Diagnosis          DiagnosisCategory     `json:"diagnosis"`
	Attribution        AttributionType       `json:"attribution"`
	DomainChanged      bool                  `json:"domain_changed"`
	PreviousDomain     string                `json:"previous_domain,omitempty"`
	HasUsage           bool                  `json:"has_usage"`
	HasPromptDetails   bool                  `json:"has_prompt_details"`
	StructurallyStable bool                  `json:"structurally_stable"`
	PromptTokens       int                   `json:"prompt_tokens"`
	CachedReadTokens   int                   `json:"cached_read_tokens"`
	CachedWriteTokens  int                   `json:"cached_write_tokens"`
	Transformation     TransformationFinding `json:"transformation"`
	Findings           []DiagnosticFinding   `json:"findings,omitempty"`
	PrimaryFinding     *DiagnosticFinding    `json:"primary_finding,omitempty"`
	Snapshot           RequestSnapshot       `json:"-"`
}

var (
	requestCounter atomic.Int64
	isInitialized  atomic.Bool

	activeConfigMu sync.RWMutex
	activeConfig   PluginConfig = DefaultPluginConfig()

	globalRateLimiterMu sync.RWMutex
	globalRateLimiter   *FindingRateLimiter
)

func isPluginEnabled() bool {
	activeConfigMu.RLock()
	defer activeConfigMu.RUnlock()
	return activeConfig.Enabled
}

// GetName returns the plugin name identifier (Required by Bifrost plugin contract).
func GetName() string {
	return PluginName
}

// Init initializes the CacheGuard plugin with process-local key, bounded tracker, and operator config.
func Init(config any) error {
	cfg, err := ValidateAndApplyConfig(config)
	if err != nil {
		fmt.Printf("[CacheGuard] Configuration warning (using defaults): %v\n", err)
	}

	activeConfigMu.Lock()
	activeConfig = cfg
	activeConfigMu.Unlock()

	globalRateLimiterMu.Lock()
	globalRateLimiter = NewFindingRateLimiter(15*time.Second, 1000)
	globalRateLimiterMu.Unlock()

	ttl, _ := time.ParseDuration(cfg.SessionTTL)
	if err := InitEngine(cfg.MaxSessions, cfg.MaxEntriesPerSession, ttl); err != nil {
		return err
	}
	isInitialized.Store(true)
	return nil
}

// Cleanup cleans up resources upon unloading (Required by Bifrost plugin contract).
func Cleanup() error {
	requestCounter.Store(0)
	isInitialized.Store(false)

	activeConfigMu.Lock()
	activeConfig = DefaultPluginConfig()
	activeConfigMu.Unlock()

	globalRateLimiterMu.Lock()
	if globalRateLimiter != nil {
		globalRateLimiter.Clear()
		globalRateLimiter = nil
	}
	globalRateLimiterMu.Unlock()

	CleanupEngine()
	return nil
}

func getHTTPHeader(headers map[string]string, key string) string {
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// HTTPTransportPreHook extracts and validates the opt-in session ID header.
func HTTPTransportPreHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (resp *schemas.HTTPResponse, err error) {
	defer func() {
		if r := recover(); r != nil {
			// Fail-open: do not disrupt request pipeline
			resp = nil
			err = nil
		}
	}()

	if !isPluginEnabled() {
		return nil, nil
	}
	if req == nil || ctx == nil {
		return nil, nil
	}

	diag := &RequestDiagnostic{
		Diagnosis:   DiagnosisUntracked,
		Attribution: AttributionNone,
	}

	for k, v := range req.Headers {
		if strings.EqualFold(k, headerSessionID) {
			trimmed := strings.TrimSpace(v)
			if ValidateSessionID(trimmed) {
				ctx.SetValue(ctxSessionKey, trimmed)
				diag.SessionID = trimmed
			}
			break
		}
	}

	ctx.SetValue(ctxDiagKey, diag)
	return nil, nil
}

// PreLLMHook observes incoming requests, extracts safe component fingerprints,
// and detects differences against previous snapshots in the same session.
func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (outReq *schemas.BifrostRequest, short *schemas.LLMPluginShortCircuit, err error) {
	defer func() {
		if r := recover(); r != nil {
			// Fail-open: return original request without modification
			outReq = req
			short = nil
			err = nil
		}
	}()

	if !isPluginEnabled() {
		return req, nil, nil
	}
	if req == nil {
		return req, nil, nil
	}

	requestCounter.Add(1)

	var diag *RequestDiagnostic
	if ctx != nil {
		if val := ctx.Value(ctxDiagKey); val != nil {
			if d, ok := val.(*RequestDiagnostic); ok {
				diag = d
			}
		}
	}
	if diag == nil {
		diag = &RequestDiagnostic{
			Diagnosis:   DiagnosisUntracked,
			Attribution: AttributionNone,
		}
		if ctx != nil {
			ctx.SetValue(ctxDiagKey, diag)
		}
	}

	if req.ChatRequest != nil {
		diag.Model = req.ChatRequest.Model
		diag.Provider = string(req.ChatRequest.Provider)

		// 1. Process System / Developer instructions or explicit cache prefix safely with HMAC
		prefixHMAC, hasSys := ExtractPrefixSnapshot(req.ChatRequest)
		diag.HasSystemPrompt = hasSys
		diag.Snapshot.PrefixHMAC = prefixHMAC
		diag.Snapshot.HasSystemPrompt = hasSys

		// 2. Process Tools and nested schemas safely with canonical path flattening
		if req.ChatRequest.Params != nil && len(req.ChatRequest.Params.Tools) > 0 {
			diag.ToolCount = len(req.ChatRequest.Params.Tools)
			toolsHMAC, pathHMACs, incomplete := FlattenToolSchemasWithStatus(req.ChatRequest.Params.Tools)
			diag.Snapshot.ToolsHMAC = toolsHMAC
			diag.Snapshot.PathHMACs = pathHMACs
			if incomplete {
				diag.Snapshot.ComparisonIncomplete = true
			}
		}

		// 3. Count client-supplied cache markers
		clientMarkers, clientPaths := CountChatCacheMarkers(req.ChatRequest)
		diag.Snapshot.ClientCacheMarkerCount = clientMarkers
		diag.Snapshot.ClientMarkerPaths = clientPaths

		diag.Snapshot.ToolCount = diag.ToolCount
	}

	// 4. Session and Tenant Isolation
	var sessionID string
	if diag.SessionID != "" {
		sessionID = diag.SessionID
	} else if ctx != nil {
		if val := ctx.Value(ctxSessionKey); val != nil {
			if s, ok := val.(string); ok {
				sessionID = s
			}
		}
	}

	if sessionID != "" {
		diag.SessionID = sessionID
		tenantID, trusted := ExtractTrustedTenant(ctx)
		diag.TrustedTenant = trusted
		diag.TenantID = tenantID

		if !trusted {
			// Refuse cross-request comparison when tenant isolation cannot be verified
			diag.Diagnosis = DiagnosisRefused
			diag.Attribution = AttributionNone
		} else {
			// Check if provider or model changed for this session
			globalDomainTrackerMu.RLock()
			domainTracker := globalDomainTracker
			globalDomainTrackerMu.RUnlock()
			if domainTracker != nil {
				if changed, prev := domainTracker.CheckAndRecord(tenantID, sessionID, diag.Provider, diag.Model); changed {
					diag.DomainChanged = true
					diag.PreviousDomain = prev
				}
			}

			compositeKey := fmt.Sprintf("%s|%s|%s|%s", tenantID, diag.Provider, diag.Model, sessionID)
			diag.CompositeKey = compositeKey

			globalTrackerMu.RLock()
			tracker := globalTracker
			globalTrackerMu.RUnlock()

			if tracker != nil {
				if prev, exists := tracker.GetLatest(compositeKey); exists {
					drift := CompareSnapshots(prev, diag.Snapshot)
					diag.Drift = drift
					diag.Attribution = drift.Attribution
					if drift.Detected {
						diag.Diagnosis = DiagnosisConfirmedDrift
					} else if drift.Attribution == AttributionOriginUnverified && strings.Contains(drift.Reason, "comparison incomplete") {
						diag.StructurallyStable = false
						diag.Diagnosis = DiagnosisUntracked
					} else {
						diag.StructurallyStable = true
						diag.Diagnosis = DiagnosisStructurallyStable
					}
				} else {
					diag.Diagnosis = DiagnosisInitialRequest
					diag.Attribution = AttributionNone
				}
			}
		}
	}

	return req, nil, nil
}

// PostLLMHook observes provider responses and extracts cache usage metrics where available.
func PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (outResp *schemas.BifrostResponse, outErr *schemas.BifrostError, err error) {
	defer func() {
		if r := recover(); r != nil {
			// Fail-open: return original response and error untouched
			outResp = resp
			outErr = bifrostErr
			err = nil
		}
	}()

	if !isPluginEnabled() {
		return resp, bifrostErr, nil
	}

	var diag *RequestDiagnostic
	if ctx != nil {
		if val := ctx.Value(ctxDiagKey); val != nil {
			if d, ok := val.(*RequestDiagnostic); ok {
				diag = d
			}
		}
	}

	if diag == nil {
		return resp, bifrostErr, nil
	}

	// 1. Observe Outbound Provider-Bound Request if accessible via RawRequest
	if resp != nil && resp.GetExtraFields() != nil {
		rawReq := resp.GetExtraFields().RawRequest
		observable, prefixHMAC, toolsHMAC, pathHMACs, toolCount, markerCount, markerPaths := ParseOutboundPayload(rawReq)
		diag.Snapshot.OutboundObservable = observable
		diag.Snapshot.OutboundPrefixHMAC = prefixHMAC
		diag.Snapshot.OutboundToolsHMAC = toolsHMAC
		diag.Snapshot.OutboundPathHMACs = pathHMACs
		diag.Snapshot.OutboundToolCount = toolCount
		diag.Snapshot.OutboundCacheMarkerCount = markerCount
		diag.Snapshot.OutboundMarkerPaths = markerPaths
	}

	// 2. Detect within-request cache-control marker transformations
	diag.Transformation = DetectTransformation(diag.Snapshot, diag.Provider, diag.Model)

	// 3. Perform two-stage re-evaluation against previous snapshot if available
	if diag.CompositeKey != "" && diag.TrustedTenant {
		globalTrackerMu.RLock()
		tracker := globalTracker
		globalTrackerMu.RUnlock()

		if tracker != nil {
			if prev, exists := tracker.GetLatest(diag.CompositeKey); exists {
				drift := CompareSnapshots(prev, diag.Snapshot)
				diag.Drift = drift
				diag.Attribution = drift.Attribution
				if drift.Detected {
					diag.Diagnosis = DiagnosisConfirmedDrift
				}
			}
		}
	}

	isStreaming := false
	isStreamEnd := false
	if ctx != nil {
		if ctx.Value(schemas.BifrostContextKeyStreamAccumulatedUsage) != nil {
			isStreaming = true
		}
		if endVal := ctx.Value(schemas.BifrostContextKeyStreamEndIndicator); endVal != nil {
			if end, ok := endVal.(bool); ok && end {
				isStreamEnd = true
			}
		}
	}

	// 4. Extract and normalize provider usage metrics
	if resp != nil && resp.ChatResponse != nil && resp.ChatResponse.Usage != nil {
		usage := resp.ChatResponse.Usage
		diag.HasUsage = true
		diag.PromptTokens = usage.PromptTokens

		if usage.PromptTokensDetails != nil {
			diag.CachedReadTokens = usage.PromptTokensDetails.CachedReadTokens
			diag.CachedWriteTokens = usage.PromptTokensDetails.CachedWriteTokens

			// If provider only supplied non-cache details (e.g. AudioTokens > 0 or ImageTokens > 0)
			// with zero cached read/write tokens, the provider never supplied a cached-token metric.
			// Do not equate absent with explicit zero.
			hasExplicitCacheMetric := (diag.CachedReadTokens > 0 || diag.CachedWriteTokens > 0 || usage.PromptTokensDetails.CachedWriteTokenDetails != nil)
			if !hasExplicitCacheMetric && (usage.PromptTokensDetails.AudioTokens > 0 || usage.PromptTokensDetails.ImageTokens > 0) {
				diag.HasPromptDetails = false
			} else {
				diag.HasPromptDetails = true
			}
		}

		// Finalize diagnosis if structural comparison was stable
		if diag.StructurallyStable {
			if diag.HasPromptDetails {
				if diag.CachedReadTokens > 0 {
					diag.Diagnosis = DiagnosisStableHit
				} else {
					diag.Diagnosis = DiagnosisStableMiss
				}
			} else {
				// Provider usage reported, but prompt cache details are absent
				diag.Diagnosis = DiagnosisUsageUnavailable
			}
		}
	} else if bifrostErr != nil {
		// Provider returned an error: do not label failed requests as cache miss
		if diag.StructurallyStable || diag.Diagnosis == DiagnosisStableMiss {
			diag.Diagnosis = DiagnosisUntracked
		}
	} else if !diag.HasUsage && (!isStreaming || isStreamEnd) {
		// Usage metadata is absent on complete response: do not falsely label as stable_miss
		if diag.StructurallyStable || diag.Diagnosis == DiagnosisStableMiss {
			diag.Diagnosis = DiagnosisUsageUnavailable
		}
	}

	// 5. Build human-readable diagnostic findings upon turn completion or usage arrival
	if !isStreaming || isStreamEnd || diag.HasUsage {
		diag.Findings = BuildDiagnosticFindings(diag)
		if len(diag.Findings) > 0 {
			var primary *DiagnosticFinding
			for i := range diag.Findings {
				f := &diag.Findings[i]
				if primary == nil || (f.Severity == SeverityWarning && primary.Severity == SeverityInfo) || (f.Severity == SeverityError && primary.Severity != SeverityError) {
					primary = f
				}
			}
			diag.PrimaryFinding = primary

			activeConfigMu.RLock()
			cfg := activeConfig
			activeConfigMu.RUnlock()

			if cfg.DiagnosticLogging {
				emitPrivacySafeLogs(ctx, diag, cfg)
			}
		}
	}

	// 6. Record snapshot into session tracker upon successful turn only (at stream end for streaming, or for complete unary responses)
	if bifrostErr == nil && resp != nil && diag.CompositeKey != "" && diag.TrustedTenant && (!isStreaming || isStreamEnd) {
		globalTrackerMu.RLock()
		tracker := globalTracker
		globalTrackerMu.RUnlock()
		if tracker != nil {
			tracker.Record(diag.CompositeKey, diag.Snapshot)
		}
	}

	return resp, bifrostErr, nil
}

// HTTPTransportResponseHeadersHook sets non-sensitive diagnostic HTTP response headers.
// NEVER exposes raw HMAC fingerprints or prompt content.
func HTTPTransportResponseHeadersHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponseMetadata) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// Fail-open: do not fail request
			err = nil
		}
	}()

	if !isPluginEnabled() {
		return nil
	}

	if resp != nil {
		resp.SetHeader("X-Cacheguard-Status", "active")
		resp.SetHeader("X-Cacheguard-Observed", fmt.Sprintf("%d", requestCounter.Load()))

		if ctx != nil {
			if val := ctx.Value(ctxDiagKey); val != nil {
				if diag, ok := val.(*RequestDiagnostic); ok {
					resp.SetHeader("X-Cacheguard-Diagnosis", string(diag.Diagnosis))
					resp.SetHeader("X-Cacheguard-Attribution", string(diag.Attribution))

					if diag.DomainChanged {
						resp.SetHeader("X-Cacheguard-Domain-Changed", "true")
						// Privacy: Omit X-Cacheguard-Previous-Domain to prevent leaking provider/model information
					}

					activeConfigMu.RLock()
					exposePaths := activeConfig.ExposeSchemaPaths
					activeConfigMu.RUnlock()

					if diag.Drift.Detected {
						resp.SetHeader("X-Cacheguard-Drift-Component", diag.Drift.Component)
						if diag.Drift.Path != "" && exposePaths {
							resp.SetHeader("X-Cacheguard-Drift-Path", SanitizeFieldPath(diag.Drift.Path))
						}
						resp.SetHeader("X-Cacheguard-Drift-Type", diag.Drift.ChangeType)
						// Privacy: Omit raw client-derived drift reason strings from response headers
					}

					if diag.HasUsage {
						resp.SetHeader("X-Cacheguard-Prompt-Tokens", fmt.Sprintf("%d", diag.PromptTokens))
						if diag.HasPromptDetails {
							resp.SetHeader("X-Cacheguard-Cached-Tokens", fmt.Sprintf("%d", diag.CachedReadTokens))
						}
					}

					if diag.Transformation.Type != TransformationNone {
						resp.SetHeader("X-Cacheguard-Transformation", string(diag.Transformation.Type))
						resp.SetHeader("X-Cacheguard-Client-Markers", fmt.Sprintf("%d", diag.Transformation.ClientMarkers))
						resp.SetHeader("X-Cacheguard-Outbound-Markers", fmt.Sprintf("%d", diag.Transformation.OutboundMarkers))
						if diag.Transformation.Path != "" && exposePaths {
							resp.SetHeader("X-Cacheguard-Injected-Path", SanitizeFieldPath(diag.Transformation.Path))
						}
						if diag.Transformation.WarrantsReview {
							resp.SetHeader("X-Cacheguard-Review-Recommended", "true")
						}
					}

					if diag.PrimaryFinding != nil {
						resp.SetHeader("X-Cacheguard-Finding-Code", diag.PrimaryFinding.Code)
						resp.SetHeader("X-Cacheguard-Finding-Severity", string(diag.PrimaryFinding.Severity))
						resp.SetHeader("X-Cacheguard-Finding-Message", diag.PrimaryFinding.Message)
					}
				}
			}
		}
	}
	return nil
}

// emitPrivacySafeLogs emits structured, rate-limited operator logs.
// NEVER logs raw prompts, HMAC keys, client secrets, or full tenant IDs.
func emitPrivacySafeLogs(ctx *schemas.BifrostContext, diag *RequestDiagnostic, cfg PluginConfig) {
	if diag == nil || len(diag.Findings) == 0 {
		return
	}

	minSeverity := SeverityInfo
	if cfg.SeverityThreshold == "warning" {
		minSeverity = SeverityWarning
	} else if cfg.SeverityThreshold == "error" {
		minSeverity = SeverityError
	}

	for _, f := range diag.Findings {
		if minSeverity == SeverityWarning && f.Severity == SeverityInfo {
			continue
		}
		if minSeverity == SeverityError && f.Severity != SeverityError {
			continue
		}

		// Rate limit per trusted scope + code + path to prevent log flooding
		scopeKey := fmt.Sprintf("%s|%s|%s", diag.TenantID, f.Code, f.Path)
		globalRateLimiterMu.RLock()
		limiter := globalRateLimiter
		globalRateLimiterMu.RUnlock()

		if limiter != nil && !limiter.ShouldLog(scopeKey, time.Now()) {
			continue
		}

		// 1. Structured log into BifrostContext
		if ctx != nil {
			logMsg := fmt.Sprintf("code=%s severity=%s component=%s path=%s msg=%q action=%q",
				f.Code, f.Severity, f.Component, f.Path, f.Message, f.SuggestedAction)
			level := schemas.LogLevelInfo
			if f.Severity == SeverityWarning {
				level = schemas.LogLevelWarn
			} else if f.Severity == SeverityError {
				level = schemas.LogLevelError
			}
			ctx.Log(level, logMsg)
		}

		// 2. Structured log to Operator Stdout/Stderr (bounded, sanitized, zero raw prompt)
		cleanPath := f.Path
		if cleanPath == "" {
			cleanPath = "n/a"
		}
		fmt.Printf("[CacheGuard] %s [%s] code=%s component=%s path=%s msg=%q action=%q\n",
			strings.ToUpper(string(f.Severity)),
			f.Confidence,
			f.Code,
			f.Component,
			cleanPath,
			f.Message,
			f.SuggestedAction,
		)
	}
}

// GetObservedRequestCount returns the total number of observed requests.
func GetObservedRequestCount() int64 {
	return requestCounter.Load()
}

// GetRequestDiagnostic retrieves the RequestDiagnostic from the request context.
func GetRequestDiagnostic(ctx *schemas.BifrostContext) (RequestDiagnostic, bool) {
	if ctx == nil {
		return RequestDiagnostic{}, false
	}
	if val := ctx.Value(ctxDiagKey); val != nil {
		if diag, ok := val.(*RequestDiagnostic); ok && diag != nil {
			return *diag, true
		}
		if diag, ok := val.(RequestDiagnostic); ok {
			return diag, true
		}
	}
	return RequestDiagnostic{}, false
}

// IsInitialized returns whether Init was called.
func IsInitialized() bool {
	return isInitialized.Load()
}

func main() {
	fmt.Println("Bifrost CacheGuard plugin entrypoint")
}
