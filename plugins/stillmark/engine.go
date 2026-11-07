package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

const (
	domainPrefixPrompt     = "stillmark:prompt:"
	domainPrefixMasked     = "stillmark:prompt-masked:"
	domainPrefixTool       = "stillmark:tool:"
	domainPrefixSchemaPath = "stillmark:schema-path:"

	DefaultMaxSessions = 10000
	DefaultMaxEntries  = 10
	DefaultSessionTTL  = 30 * time.Minute

	headerSessionID = "x-stillmark-session-id"
)

// AttributionType classifies the verified origin of request differences across requests.
type AttributionType string

const (
	AttributionNone                 AttributionType = "none"
	AttributionClientInputChanged   AttributionType = "client_input_changed"
	AttributionGatewayOutputChanged AttributionType = "gateway_output_changed"
	AttributionOriginUnverified     AttributionType = "origin_unverified"
)

// TransformationType classifies within-request transformations performed on cache-control markers.
type TransformationType string

const (
	TransformationNone                 TransformationType = "none"
	TransformationCacheControlInjected TransformationType = "cache_control_injected"
	TransformationCacheControlClamped  TransformationType = "cache_control_clamped"
	TransformationMultiMarkerObserved  TransformationType = "multi_marker_observed"
)

// TransformationFinding reports verified within-request alterations to cache-control breakpoints.
type TransformationFinding struct {
	Type            TransformationType `json:"type"`
	Component       string             `json:"component"`          // e.g. "messages[0].content[0].cache_control" or "cache_control"
	Path            string             `json:"path"`               // sanitized JSON path
	ClientMarkers   int                `json:"client_markers"`     // count of client-supplied markers
	OutboundMarkers int                `json:"outbound_markers"`   // count of provider-bound markers
	BifrostAdded    bool               `json:"bifrost_added"`      // true if gateway added markers
	Provider        string             `json:"provider,omitempty"` // provider context
	Model           string             `json:"model,omitempty"`    // model context
	WarrantsReview  bool               `json:"warrants_review"`    // true if configuration warrants review
	DiagnosticMode  bool               `json:"diagnostic_mode"`    // true if observed via diagnostic outbound capture
	Summary         string             `json:"summary"`            // concise evidence-based description
}

// DiagnosticSeverity indicates the operational impact of a finding.
type DiagnosticSeverity string

const (
	SeverityInfo    DiagnosticSeverity = "info"
	SeverityWarning DiagnosticSeverity = "warning"
	SeverityError   DiagnosticSeverity = "error"
)

// EvidenceSource identifies the origin of the observation.
type EvidenceSource string

const (
	EvidenceClientInput    EvidenceSource = "client_input"
	EvidenceProviderOutput EvidenceSource = "provider_output"
	EvidenceProviderUsage  EvidenceSource = "provider_usage"
	EvidenceGatewayState   EvidenceSource = "gateway_state"
)

// ConfidenceLevel reports the certainty of the diagnosis based on active observation.
type ConfidenceLevel string

const (
	ConfidenceConfirmed ConfidenceLevel = "confirmed"
	ConfidenceObserved  ConfidenceLevel = "observed"
	ConfidenceInferred  ConfidenceLevel = "inferred"
)

// Stable diagnostic codes.
const (
	CodeToolSchemaChanged               = "SM001"
	CodeGatewayBreakpointInjected       = "SM002"
	CodeStableMissUnknownCause          = "SM003"
	CodeOutboundVerificationUnavailable = "SM004"
	CodeProviderOrModelChanged          = "SM005"
	CodeMultiMarkerConfigObserved       = "SM006"
	CodeGatewayBreakpointClamped        = "SM007"
	CodeSystemPromptChanged             = "SM008"
	CodeVolatileValueInPrefix           = "SM009"
	CodeToolsReordered                  = "SM010"
	CodeToolSerializationChanged        = "SM011"
)

// DiagnosticFinding represents a human-readable, privacy-safe operator finding.
// Free of sensitive prompt content, user secrets, or unhashed identifiers.
type DiagnosticFinding struct {
	Code               string             `json:"code"`                // e.g. "SM001"
	Severity           DiagnosticSeverity `json:"severity"`            // "info", "warning", "error"
	Component          string             `json:"component"`           // "tools", "cache_control", "system_prompt", etc.
	Path               string             `json:"path,omitempty"`      // Sanitized JSON path
	EvidenceSource     EvidenceSource     `json:"evidence_source"`     // "client_input", "provider_output", etc.
	Confidence         ConfidenceLevel    `json:"confidence"`          // "confirmed", "observed", "inferred"
	Message            string             `json:"message"`             // Plain-English explanation
	SuggestedAction    string             `json:"suggested_action"`    // Actionable operator guidance
	OutboundObservable bool               `json:"outbound_observable"` // Whether provider-bound payload was inspected
}

// PluginConfig defines operator configuration for StillMark.
type PluginConfig struct {
	Enabled               bool   `json:"enabled"`
	DiagnosticLogging     bool   `json:"diagnostic_logging"`
	SeverityThreshold     string `json:"severity_threshold"`      // "info", "warning", "error" (default: "info")
	MaxSessions           int    `json:"max_sessions"`            // default: 10000
	MaxEntriesPerSession  int    `json:"max_entries_per_session"` // default: 10
	SessionTTL            string `json:"session_ttl"`             // duration string e.g. "30m" (default: "30m")
	RawOutboundInspection bool   `json:"raw_outbound_inspection"` // default: false (DIAGNOSTIC-ONLY)
	ExposeSchemaPaths     bool   `json:"expose_schema_paths"`     // default: false (opt-in to expose detailed schema paths in client response headers)

	// Reference prices (USD per million input tokens) used only to estimate the cost of lost cache reuse.
	// Defaults are a reference point; set them to your provider's published prices.
	InputPricePerMTok       float64 `json:"input_price_per_mtok"`        // default: 3.00
	CachedInputPricePerMTok float64 `json:"cached_input_price_per_mtok"` // default: 0.30

	// AutoSession tracks requests without an X-Stillmark-Session-Id header, grouped by
	// authenticated tenant + provider + model, so no client changes are needed. default: true
	AutoSession bool `json:"auto_session"`
	// ReportAddr serves the operator report (GET /stillmark/report) on this loopback address,
	// e.g. "127.0.0.1:8099". Empty disables it. Non-loopback addresses are refused. default: ""
	ReportAddr string `json:"report_addr"`
}

func DefaultPluginConfig() PluginConfig {
	return PluginConfig{
		Enabled:               true,
		DiagnosticLogging:     true,
		SeverityThreshold:     "info",
		MaxSessions:           DefaultMaxSessions,
		MaxEntriesPerSession:  DefaultMaxEntries,
		SessionTTL:            "30m",
		RawOutboundInspection: false,
		ExposeSchemaPaths:     false,

		InputPricePerMTok:       DefaultInputPricePerMTok,
		CachedInputPricePerMTok: DefaultCachedInputPricePerMTok,
		AutoSession:             true,
	}
}

const (
	DefaultInputPricePerMTok       = 3.00
	DefaultCachedInputPricePerMTok = 0.30
)

// EstimateExtraCostPer1k returns the estimated extra USD per 1,000 requests when tokensLost
// input tokens are billed at the full input price instead of the cached price.
func EstimateExtraCostPer1k(tokensLost int, inputPrice, cachedPrice float64) float64 {
	if tokensLost <= 0 || inputPrice <= cachedPrice {
		return 0
	}
	return float64(tokensLost) * (inputPrice - cachedPrice) / 1e6 * 1000
}

var (
	sessionIDRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,64}$`)

	processKeyMu sync.RWMutex
	processKey   []byte

	globalTrackerMu sync.RWMutex
	globalTracker   *SessionTracker

	globalDomainTrackerMu sync.RWMutex
	globalDomainTracker   *SessionDomainTracker
)

type domainEntry struct {
	val        string
	lastActive time.Time
}

// SessionDomainTracker tracks the provider and model used in a session to identify
// expected provider or model switches across requests.
// Implements bounded size and TTL-based eviction consistent with SessionTracker.
type SessionDomainTracker struct {
	mu          sync.Mutex
	maxSessions int
	ttl         time.Duration
	domains     map[string]domainEntry // tenantID|sessionID -> {val: "provider|model", lastActive}
}

func NewSessionDomainTracker(maxSessions int, ttl time.Duration) *SessionDomainTracker {
	if maxSessions <= 0 {
		maxSessions = DefaultMaxSessions
	} else if maxSessions > 100000 {
		maxSessions = 100000
	} else if maxSessions < 10 {
		maxSessions = 10
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &SessionDomainTracker{
		maxSessions: maxSessions,
		ttl:         ttl,
		domains:     make(map[string]domainEntry),
	}
}

func (s *SessionDomainTracker) CheckAndRecord(tenantID, sessionID, provider, model string) (bool, string) {
	if s == nil || sessionID == "" {
		return false, ""
	}
	key := tenantID + "|" + sessionID
	val := provider + "|" + model
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, exists := s.domains[key]
	if exists {
		if now.Sub(existing.lastActive) > s.ttl {
			delete(s.domains, key)
			exists = false
		}
	}

	if !exists {
		// Evict oldest if reaching capacity
		if len(s.domains) >= s.maxSessions {
			var oldestKey string
			var oldestTime time.Time
			for k, v := range s.domains {
				if oldestKey == "" || v.lastActive.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.lastActive
				}
			}
			if oldestKey != "" {
				delete(s.domains, oldestKey)
			}
		}
		s.domains[key] = domainEntry{val: val, lastActive: now}
		return false, ""
	}

	s.domains[key] = domainEntry{val: val, lastActive: now}
	if existing.val != val {
		return true, existing.val
	}
	return false, existing.val
}

func (s *SessionDomainTracker) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.domains = make(map[string]domainEntry)
}

func (s *SessionDomainTracker) Count() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.domains)
}

// InitEngine initializes process-local cryptographic key, session tracker, and domain tracker.
func InitEngine(maxSessions, maxEntries int, ttl time.Duration) error {
	processKeyMu.Lock()
	defer processKeyMu.Unlock()

	processKey = make([]byte, 32)
	if _, err := rand.Read(processKey); err != nil {
		return fmt.Errorf("failed to generate process-local HMAC key: %w", err)
	}

	globalTrackerMu.Lock()
	globalTracker = NewSessionTracker(maxSessions, maxEntries, ttl)
	globalTrackerMu.Unlock()

	globalDomainTrackerMu.Lock()
	globalDomainTracker = NewSessionDomainTracker(maxSessions, ttl)
	globalDomainTrackerMu.Unlock()

	return nil
}

// CleanupEngine clears process-local keys, session memory, and domain tracking.
func CleanupEngine() {
	processKeyMu.Lock()
	if processKey != nil {
		for i := range processKey {
			processKey[i] = 0
		}
		processKey = nil
	}
	processKeyMu.Unlock()

	globalTrackerMu.Lock()
	if globalTracker != nil {
		globalTracker.Clear()
		globalTracker = nil
	}
	globalTrackerMu.Unlock()

	globalDomainTrackerMu.Lock()
	if globalDomainTracker != nil {
		globalDomainTracker.Clear()
		globalDomainTracker = nil
	}
	globalDomainTrackerMu.Unlock()
}

// computeHMAC generates domain-separated keyed hashes using the process-local secret.
// This prevents offline rainbow-table precomputation on short, predictable prompts.
// Thread-safe: holds read lock during HMAC initialization and calculation to prevent
// race conditions with CleanupEngine.
func computeHMAC(domain string, data []byte) string {
	processKeyMu.RLock()
	defer processKeyMu.RUnlock()

	if len(processKey) == 0 || len(data) == 0 {
		return ""
	}

	mac := hmac.New(sha256.New, processKey)
	mac.Write([]byte(domain))
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// ValidateSessionID checks length (8-64) and character set [a-zA-Z0-9_-].
func ValidateSessionID(sessionID string) bool {
	return sessionIDRegex.MatchString(sessionID)
}

// ExtractTrustedTenant extracts the authenticated tenant identity from BifrostContext.
// Never trusts an unverified client tenant header.
func ExtractTrustedTenant(ctx *schemas.BifrostContext) (string, bool) {
	if ctx == nil {
		return "", false
	}
	if custID, ok := ctx.Value(schemas.BifrostContextKeyGovernanceCustomerID).(string); ok && custID != "" {
		return "customer:" + custID, true
	}
	if vkID, ok := ctx.Value(schemas.BifrostContextKeyGovernanceVirtualKeyID).(string); ok && vkID != "" {
		return "virtual_key:" + vkID, true
	}
	return "", false
}

// RequestSnapshot stores bounded, non-sensitive structural metadata for a request.
// NEVER stores raw prompts, parameter values, or authorization secrets.
type RequestSnapshot struct {
	Timestamp time.Time

	// Stage A: Client Input (observed in PreLLMHook)
	PrefixHMAC             string
	PrefixMaskedHMAC       string // prefix HMAC with volatile values masked
	ToolsHMAC              string
	PathHMACs              map[string]string // schema_path -> HMAC(leaf_val)
	HasSystemPrompt        bool
	ToolCount              int
	ClientCacheMarkerCount int
	ClientMarkerPaths      []string
	ComparisonIncomplete   bool // Indicates schema recursion, path count, or leaf bounds were reached

	// Stage B: Outbound Provider-Bound Request (observed in PostLLMHook via RawRequest when enabled)
	OutboundObservable       bool
	OutboundPrefixHMAC       string
	OutboundToolsHMAC        string
	OutboundPathHMACs        map[string]string // schema_path -> HMAC(leaf_val)
	OutboundToolCount        int
	OutboundCacheMarkerCount int
	OutboundMarkerPaths      []string

	// Byte-exact fingerprints of the top-level "tools" array (key-order-sensitive)
	ClientToolsRawHMAC   string
	OutboundToolsRawHMAC string

	// Provider usage recorded with the snapshot (counts only), used to measure cache loss on drift
	CachedReadTokens int
	PromptTokens     int
}

const (
	maxSchemaRecursionDepth = 16
	maxFlattenedPaths       = 512
	maxLeafValueLength      = 2048
)

// ExtractPrefixSnapshot deterministically captures the static prefix that is cache-eligible.
// In multi-turn chat conversations, later turns (messages[1], messages[2], ...) are appended.
// 1. If explicit System or Developer message(s) exist, all of them are captured in order as the primary prefix.
// 2. Supports multimodal content blocks (text, image, audio metadata).
// 3. If an explicit cache_control marker is present on any message, that message is part of the prefix.
// 4. Otherwise, the initial turn messages[0] serves as the baseline prefix.
func ExtractPrefixSnapshot(chatReq *schemas.BifrostChatRequest) (string, bool) {
	raw, hasSys := extractPrefixBytes(chatReq)
	if raw == nil {
		return "", false
	}
	return computeHMAC(domainPrefixPrompt, raw), hasSys
}

// ExtractPrefixFingerprints returns the prefix HMAC plus a second HMAC computed after masking
// volatile values (dates, times, UUIDs, epoch timestamps, long hex IDs). If two requests differ
// in the first HMAC but match in the masked one, the only change was a per-request dynamic value.
// Raw prompt bytes are never stored or returned.
func ExtractPrefixFingerprints(chatReq *schemas.BifrostChatRequest) (prefixHMAC, maskedHMAC string, hasSys bool) {
	raw, hasSys := extractPrefixBytes(chatReq)
	if raw == nil {
		return "", "", false
	}
	return computeHMAC(domainPrefixPrompt, raw), computeHMAC(domainPrefixMasked, MaskVolatileValues(raw)), hasSys
}

var volatilePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`),
	regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}(?:[T ]\d{1,2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?)?`),
	regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:\s?[AaPp][Mm])?\b`),
	regexp.MustCompile(`(?i)\b(?:jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?\s+\d{1,2}(?:st|nd|rd|th)?,?\s+\d{4}\b`),
	regexp.MustCompile(`\b1[5-9]\d{8}(?:\d{3})?\b`),
	regexp.MustCompile(`\b[0-9a-fA-F]{16,}\b`),
}

// MaskVolatileValues replaces values that typically change on every request with a fixed token.
func MaskVolatileValues(raw []byte) []byte {
	out := raw
	for _, re := range volatilePatterns {
		out = re.ReplaceAll(out, []byte("<v>"))
	}
	return out
}

func extractPrefixBytes(chatReq *schemas.BifrostChatRequest) ([]byte, bool) {
	if chatReq == nil || len(chatReq.Input) == 0 {
		return nil, false
	}

	// 1. Check for explicit system/developer instruction(s) - capture ALL of them in order
	var sysBuf bytes.Buffer
	hasSys := false
	for _, msg := range chatReq.Input {
		if msg.Role == schemas.ChatMessageRoleSystem || msg.Role == schemas.ChatMessageRoleDeveloper {
			hasSys = true
			sysBuf.WriteString(string(msg.Role))
			sysBuf.WriteString(":")
			if msg.Content != nil {
				if msg.Content.ContentStr != nil {
					sysBuf.WriteString(*msg.Content.ContentStr)
				}
				for _, block := range msg.Content.ContentBlocks {
					if block.Type != "" {
						sysBuf.WriteString(string(block.Type))
						sysBuf.WriteString(":")
					}
					if block.Text != nil {
						sysBuf.WriteString(*block.Text)
					}
					if block.ImageURLStruct != nil {
						sysBuf.WriteString(block.ImageURLStruct.URL)
					}
				}
			}
			sysBuf.WriteString(";")
		}
	}
	if hasSys {
		return sysBuf.Bytes(), true
	}

	// 2. Check for message with explicit cache-control marker
	for _, msg := range chatReq.Input {
		if msg.Content != nil {
			for _, block := range msg.Content.ContentBlocks {
				if block.CacheControl != nil || block.PromptCacheBreakpoint != nil || block.CachePoint != nil {
					var b bytes.Buffer
					if block.Text != nil {
						b.WriteString(*block.Text)
					}
					if block.ImageURLStruct != nil {
						b.WriteString(block.ImageURLStruct.URL)
					}
					if b.Len() > 0 {
						return b.Bytes(), true
					}
				}
			}
		}
	}

	// 3. Baseline first message (e.g. initial prompt in conversations without system prompt)
	first := chatReq.Input[0]
	if first.Content != nil {
		if first.Content.ContentStr != nil {
			return []byte(*first.Content.ContentStr), false
		}
		if len(first.Content.ContentBlocks) > 0 {
			var b bytes.Buffer
			for _, block := range first.Content.ContentBlocks {
				if block.Text != nil {
					b.WriteString(*block.Text)
				}
				if block.ImageURLStruct != nil {
					b.WriteString(block.ImageURLStruct.URL)
				}
			}
			if b.Len() > 0 {
				return b.Bytes(), false
			}
		}
	}

	return nil, false
}

// CanonicalSchemaFlattener recursively extracts JSON schema paths and computes
// keyed leaf HMACs without storing the raw schema values.
// Enforces maxSchemaRecursionDepth and maxFlattenedPaths to prevent CPU/memory exhaustion.
func FlattenToolSchemas(tools []schemas.ChatTool) (string, map[string]string) {
	hmac, paths, _ := FlattenToolSchemasWithStatus(tools)
	return hmac, paths
}

// FlattenToolSchemasWithStatus extracts JSON schema paths and computes keyed leaf HMACs,
// tracking whether any content or depth limits were reached (incomplete=true).
func FlattenToolSchemasWithStatus(tools []schemas.ChatTool) (string, map[string]string, bool) {
	pathHMACs := make(map[string]string)
	if len(tools) == 0 {
		return "", pathHMACs, false
	}

	var incomplete bool
	for i, tool := range tools {
		if len(pathHMACs) >= maxFlattenedPaths {
			incomplete = true
			break
		}
		basePath := fmt.Sprintf("tools[%d]", i)
		if tool.Function != nil {
			funcPath := basePath + ".function"
			if tool.Function.Name != "" {
				namePath := funcPath + ".name"
				pathHMACs[namePath] = computeHMAC(domainPrefixTool, []byte(tool.Function.Name))
			}
			if tool.Function.Description != nil && *tool.Function.Description != "" {
				descPath := funcPath + ".description"
				desc := *tool.Function.Description
				if len(desc) > maxLeafValueLength {
					desc = desc[:maxLeafValueLength]
					incomplete = true
				}
				pathHMACs[descPath] = computeHMAC(domainPrefixTool, []byte(desc))
			}
			if tool.Function.Parameters != nil {
				// Marshal parameters to JSON to walk canonical JSON structure
				if paramBytes, err := json.Marshal(tool.Function.Parameters); err == nil {
					var rawMap map[string]any
					if err := json.Unmarshal(paramBytes, &rawMap); err == nil {
						if inc := flattenJSONMap(funcPath+".parameters", rawMap, pathHMACs, 0); inc {
							incomplete = true
						}
					}
				}
			}
		}
	}

	// Compute overall tools HMAC from sorted paths
	var sortedKeys []string
	for k := range pathHMACs {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	var combined strings.Builder
	for _, k := range sortedKeys {
		combined.WriteString(k)
		combined.WriteString("=")
		combined.WriteString(pathHMACs[k])
		combined.WriteString(";")
	}

	overallHMAC := computeHMAC(domainPrefixTool, []byte(combined.String()))
	return overallHMAC, pathHMACs, incomplete
}

func flattenJSONMap(prefix string, m map[string]any, dest map[string]string, depth int) bool {
	var incomplete bool
	if depth >= maxSchemaRecursionDepth {
		return true
	}
	if len(dest) >= maxFlattenedPaths {
		return true
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if len(dest) >= maxFlattenedPaths {
			incomplete = true
			break
		}
		path := prefix + "." + k
		val := m[k]
		switch v := val.(type) {
		case map[string]any:
			if flattenJSONMap(path, v, dest, depth+1) {
				incomplete = true
			}
		case []any:
			if flattenJSONArray(path, v, dest, depth+1) {
				incomplete = true
			}
		default:
			valStr := fmt.Sprintf("%v", v)
			if len(valStr) > maxLeafValueLength {
				valStr = valStr[:maxLeafValueLength]
				incomplete = true
			}
			dest[path] = computeHMAC(domainPrefixSchemaPath, []byte(valStr))
		}
	}
	return incomplete
}

func flattenJSONArray(prefix string, arr []any, dest map[string]string, depth int) bool {
	var incomplete bool
	if depth >= maxSchemaRecursionDepth {
		return true
	}
	if len(dest) >= maxFlattenedPaths {
		return true
	}
	for i, item := range arr {
		if len(dest) >= maxFlattenedPaths {
			incomplete = true
			break
		}
		path := fmt.Sprintf("%s[%d]", prefix, i)
		switch v := item.(type) {
		case map[string]any:
			if flattenJSONMap(path, v, dest, depth+1) {
				incomplete = true
			}
		case []any:
			if flattenJSONArray(path, v, dest, depth+1) {
				incomplete = true
			}
		default:
			valStr := fmt.Sprintf("%v", v)
			if len(valStr) > maxLeafValueLength {
				valStr = valStr[:maxLeafValueLength]
				incomplete = true
			}
			dest[path] = computeHMAC(domainPrefixSchemaPath, []byte(valStr))
		}
	}
	return incomplete
}

// DriftResult reports the exact component and field-level difference between requests,
// along with verified attribution (client_input_changed, gateway_output_changed, or origin_unverified).
type DriftResult struct {
	Detected     bool
	Attribution  AttributionType // client_input_changed, gateway_output_changed, origin_unverified, or none
	Component    string          // "system_prompt", "tools", or "none"
	Path         string          // e.g. "tools[0].function.parameters.properties.node_id.type"
	ChangeType   string          // "added", "removed", "modified", or "none"
	Confirmed    bool            // true if cryptographically confirmed difference
	VolatileOnly bool            // true if the only system-prompt change was a dynamic value (timestamp, ID)
	Reason       string
}

// CountChatCacheMarkers scans the client-supplied chat request for explicit cache-control markers.
// Returns marker count and sanitized JSON paths.
func CountChatCacheMarkers(chatReq *schemas.BifrostChatRequest) (int, []string) {
	if chatReq == nil {
		return 0, nil
	}
	var paths []string

	for i, msg := range chatReq.Input {
		if msg.Content != nil {
			for j, block := range msg.Content.ContentBlocks {
				if block.CacheControl != nil {
					paths = append(paths, fmt.Sprintf("messages[%d].content[%d].cache_control", i, j))
				}
				if block.PromptCacheBreakpoint != nil {
					paths = append(paths, fmt.Sprintf("messages[%d].content[%d].prompt_cache_breakpoint", i, j))
				}
				if block.CachePoint != nil {
					paths = append(paths, fmt.Sprintf("messages[%d].content[%d].cache_point", i, j))
				}
			}
		}
		if msg.ChatAssistantMessage != nil {
			for j, tc := range msg.ChatAssistantMessage.ToolCalls {
				if tc.CacheControl != nil {
					paths = append(paths, fmt.Sprintf("messages[%d].tool_calls[%d].cache_control", i, j))
				}
			}
		}
	}

	if chatReq.Params != nil {
		if chatReq.Params.CacheControl != nil {
			paths = append(paths, "cache_control")
		}
		for i, tool := range chatReq.Params.Tools {
			if tool.CacheControl != nil {
				paths = append(paths, fmt.Sprintf("tools[%d].cache_control", i))
			}
		}
	}

	return len(paths), paths
}

// ExtractOutboundCacheMarkers extracts sanitized JSON paths where cache_control,
// prompt_cache_breakpoint, or cache_point markers are present in the provider-bound payload.
// NEVER stores or exposes prompt strings, values, or credentials.
func ExtractOutboundCacheMarkers(rawBytes []byte) (int, []string) {
	if len(bytes.TrimSpace(rawBytes)) == 0 {
		return 0, nil
	}
	var rawData any
	if err := json.Unmarshal(rawBytes, &rawData); err != nil {
		return 0, nil
	}

	var paths []string
	walkJSONForMarkers("", rawData, &paths, 0)
	return len(paths), paths
}

func walkJSONForMarkers(currentPath string, node any, paths *[]string, depth int) {
	if depth >= maxSchemaRecursionDepth || len(*paths) >= 64 {
		return
	}
	switch v := node.(type) {
	case map[string]any:
		for k, val := range v {
			if len(*paths) >= 64 {
				return
			}
			childPath := k
			if currentPath != "" {
				childPath = currentPath + "." + k
			}
			if k == "cache_control" && val != nil {
				*paths = append(*paths, childPath)
			} else if k == "prompt_cache_breakpoint" && val != nil {
				if b, ok := val.(bool); !ok || b {
					*paths = append(*paths, childPath)
				}
			} else if k == "cache_point" && val != nil {
				*paths = append(*paths, childPath)
			}
			walkJSONForMarkers(childPath, val, paths, depth+1)
		}
	case []any:
		for i, item := range v {
			if len(*paths) >= 64 {
				return
			}
			childPath := fmt.Sprintf("%s[%d]", currentPath, i)
			walkJSONForMarkers(childPath, item, paths, depth+1)
		}
	}
}

// DetectTransformation evaluates within-request cache-control marker transformations.
// Uses evidence-based terminology: distinguishes confirmed gateway injection, clamping,
// and potential operator review scenarios (such as issue #9416).
func DetectTransformation(snap RequestSnapshot, provider, model string) TransformationFinding {
	// Diagnostic mode: outbound capture was observable
	if snap.OutboundObservable {
		if snap.ClientCacheMarkerCount == 0 && snap.OutboundCacheMarkerCount > 0 {
			firstPath := ""
			if len(snap.OutboundMarkerPaths) > 0 {
				firstPath = snap.OutboundMarkerPaths[0]
			}
			return TransformationFinding{
				Type:            TransformationCacheControlInjected,
				Component:       "cache_control",
				Path:            firstPath,
				ClientMarkers:   0,
				OutboundMarkers: snap.OutboundCacheMarkerCount,
				BifrostAdded:    true,
				Provider:        provider,
				Model:           model,
				WarrantsReview:  snap.OutboundCacheMarkerCount > 4, // Provider ceiling typically 4
				DiagnosticMode:  true,
				Summary:         fmt.Sprintf("Bifrost prompt cache auto-injection inserted %d breakpoint(s)", snap.OutboundCacheMarkerCount),
			}
		}

		if snap.ClientCacheMarkerCount > snap.OutboundCacheMarkerCount {
			firstPath := ""
			if len(snap.ClientMarkerPaths) > 0 {
				firstPath = snap.ClientMarkerPaths[0]
			}
			return TransformationFinding{
				Type:            TransformationCacheControlClamped,
				Component:       "cache_control",
				Path:            firstPath,
				ClientMarkers:   snap.ClientCacheMarkerCount,
				OutboundMarkers: snap.OutboundCacheMarkerCount,
				BifrostAdded:    false,
				Provider:        provider,
				Model:           model,
				WarrantsReview:  false,
				DiagnosticMode:  true,
				Summary:         fmt.Sprintf("Bifrost clamped cache breakpoints from %d client markers to %d outbound markers", snap.ClientCacheMarkerCount, snap.OutboundCacheMarkerCount),
			}
		}
	}

	// OpenRouter prompt-caching check:
	// OpenRouter official documentation confirms multiple cache-control breakpoints are allowed for Gemini,
	// with only the final breakpoint used as the prompt-cache prefix.
	// We classify this as an informative configuration observation (SM006), not a proven billing bug.
	if snap.ClientCacheMarkerCount > 1 && strings.EqualFold(provider, "openrouter") {
		firstPath := ""
		if len(snap.ClientMarkerPaths) > 0 {
			firstPath = SanitizeFieldPath(snap.ClientMarkerPaths[0])
		}
		summaryMsg := fmt.Sprintf("Multiple client-supplied cache-control markers (%d) observed for OpenRouter request. For Gemini models, OpenRouter uses only the final marker as the prompt-cache prefix.", snap.ClientCacheMarkerCount)
		return TransformationFinding{
			Type:            TransformationMultiMarkerObserved,
			Component:       "cache_control",
			Path:            firstPath,
			ClientMarkers:   snap.ClientCacheMarkerCount,
			OutboundMarkers: snap.OutboundCacheMarkerCount,
			BifrostAdded:    false,
			Provider:        provider,
			Model:           model,
			WarrantsReview:  false, // Permitted by OpenRouter; not a proven defect
			DiagnosticMode:  snap.OutboundObservable,
			Summary:         summaryMsg,
		}
	}

	return TransformationFinding{
		Type:            TransformationNone,
		Component:       "none",
		ClientMarkers:   snap.ClientCacheMarkerCount,
		OutboundMarkers: snap.OutboundCacheMarkerCount,
		BifrostAdded:    false,
		Provider:        provider,
		Model:           model,
		WarrantsReview:  false,
		DiagnosticMode:  snap.OutboundObservable,
		Summary:         "No cache-control marker transformation detected",
	}
}

// ParseOutboundPayload extracts cache-sensitive structures and marker locations from the captured
// raw provider payload. It calculates keyed HMACs and NEVER stores raw bytes, prompts, or secrets.
func ParseOutboundPayload(raw any) (bool, string, string, map[string]string, int, int, []string) {
	if raw == nil {
		return false, "", "", nil, 0, 0, nil
	}

	var rawBytes []byte
	switch v := raw.(type) {
	case json.RawMessage:
		rawBytes = v
	case []byte:
		rawBytes = v
	case string:
		rawBytes = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return false, "", "", nil, 0, 0, nil
		}
		rawBytes = b
	}

	if len(bytes.TrimSpace(rawBytes)) == 0 {
		return false, "", "", nil, 0, 0, nil
	}

	markerCount, markerPaths := ExtractOutboundCacheMarkers(rawBytes)

	var payload struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		System json.RawMessage   `json:"system"` // Anthropic Messages format: top-level system prompt
		Tools  []json.RawMessage `json:"tools"`  // any provider format (OpenAI function / Anthropic input_schema)
	}

	if err := json.Unmarshal(rawBytes, &payload); err != nil {
		return true, "", "", nil, 0, markerCount, markerPaths
	}

	var prefixHMAC string
	for _, msg := range payload.Messages {
		if msg.Role == "system" || msg.Role == "developer" {
			var strContent string
			if err := json.Unmarshal(msg.Content, &strContent); err == nil && strContent != "" {
				prefixHMAC = computeHMAC(domainPrefixPrompt, []byte(strContent))
			} else if len(msg.Content) > 0 {
				prefixHMAC = computeHMAC(domainPrefixPrompt, msg.Content)
			}
			break
		}
	}
	if prefixHMAC == "" && len(payload.System) > 0 && string(payload.System) != "null" {
		var strSystem string
		if err := json.Unmarshal(payload.System, &strSystem); err == nil && strSystem != "" {
			prefixHMAC = computeHMAC(domainPrefixPrompt, []byte(strSystem))
		} else {
			prefixHMAC = computeHMAC(domainPrefixPrompt, payload.System)
		}
	}

	var toolsHMAC string
	var pathHMACs map[string]string
	if len(payload.Tools) > 0 {
		toolsHMAC, pathHMACs = FlattenOutboundTools(payload.Tools)
	}

	return true, prefixHMAC, toolsHMAC, pathHMACs, len(payload.Tools), markerCount, markerPaths
}

// compareStage checks a single comparison stage (client input or outbound request)
// for system prompt and tool schema differences.
func compareStage(
	prevPrefixHMAC, currPrefixHMAC string, prevHasSys, currHasSys bool,
	prevToolsHMAC, currToolsHMAC string, prevToolCount, currToolCount int,
	prevPaths, currPaths map[string]string,
) DriftResult {
	// 1. Compare System/Developer instructions
	if prevPrefixHMAC != currPrefixHMAC {
		changeType := "modified"
		if !prevHasSys && currHasSys {
			changeType = "added"
		} else if prevHasSys && !currHasSys {
			changeType = "removed"
		}
		return DriftResult{
			Detected:   true,
			Component:  "system_prompt",
			Path:       "messages[0].content",
			ChangeType: changeType,
			Confirmed:  true,
			Reason:     "System prompt content changed",
		}
	}

	// 2. Compare Tools definitions and nested schemas
	if prevToolsHMAC != currToolsHMAC {
		if prevToolCount != currToolCount {
			changeType := "added"
			if currToolCount < prevToolCount {
				changeType = "removed"
			}
			return DriftResult{
				Detected:   true,
				Component:  "tools",
				Path:       "tools",
				ChangeType: changeType,
				Confirmed:  true,
				Reason:     fmt.Sprintf("Tool count changed from %d to %d", prevToolCount, currToolCount),
			}
		}

		if IsToolReorder(prevPaths, currPaths, currToolCount) {
			return DriftResult{
				Detected:   true,
				Component:  "tools",
				Path:       "tools",
				ChangeType: "reordered",
				Confirmed:  true,
				Reason:     "Same tool definitions sent in a different order",
			}
		}

		diffPath, changeType := findFirstPathDiff(prevPaths, currPaths)
		return DriftResult{
			Detected:   true,
			Component:  "tools",
			Path:       diffPath,
			ChangeType: changeType,
			Confirmed:  true,
			Reason:     fmt.Sprintf("Tool schema definition modified at %s", diffPath),
		}
	}

	return DriftResult{
		Detected:   false,
		Component:  "none",
		Path:       "",
		ChangeType: "none",
		Confirmed:  false,
		Reason:     "Request structure is identical across cache-sensitive components",
	}
}

// CompareSnapshots performs two-stage deterministic hierarchical diffing between two snapshots.
// Stage 1: Compares Client Input received by Bifrost.
// Stage 2: Compares Outbound Provider-Bound request if observable.
func CompareSnapshots(prev, curr RequestSnapshot) DriftResult {
	// Stage 1: Check Client-Originated Changes
	clientDrift := compareStage(
		prev.PrefixHMAC, curr.PrefixHMAC, prev.HasSystemPrompt, curr.HasSystemPrompt,
		prev.ToolsHMAC, curr.ToolsHMAC, prev.ToolCount, curr.ToolCount,
		prev.PathHMACs, curr.PathHMACs,
	)

	if clientDrift.Detected {
		clientDrift.Attribution = AttributionClientInputChanged
		clientDrift.Reason = "Client input changed: " + clientDrift.Reason
		if clientDrift.Component == "system_prompt" && prev.PrefixMaskedHMAC != "" && prev.PrefixMaskedHMAC == curr.PrefixMaskedHMAC {
			clientDrift.VolatileOnly = true
		}
		return clientDrift
	}

	// If no difference was detected within bounded fingerprint limits, but truncation occurred:
	// DO NOT mark requests as confirmed stable! Return a conservative incomplete result.
	if prev.ComparisonIncomplete || curr.ComparisonIncomplete {
		return DriftResult{
			Detected:    false,
			Attribution: AttributionOriginUnverified,
			Component:   "tools",
			Path:        "",
			ChangeType:  "none",
			Confirmed:   false,
			Reason:      "Request schema or content exceeded fingerprint limits (comparison incomplete); structural stability cannot be confirmed",
		}
	}

	// Stage 2: Client Input is identical across cache-sensitive components.
	// Check if Outbound Provider-Bound Request is observable.
	if !prev.OutboundObservable || !curr.OutboundObservable {
		return DriftResult{
			Detected:    false,
			Attribution: AttributionOriginUnverified,
			Component:   "none",
			Path:        "",
			ChangeType:  "none",
			Confirmed:   false,
			Reason:      "Client input is identical, but outbound provider request is unobservable (raw capture disabled)",
		}
	}

	// Both outbound requests are observable: compare outbound provider-bound content
	outboundDrift := compareStage(
		prev.OutboundPrefixHMAC, curr.OutboundPrefixHMAC, prev.HasSystemPrompt, curr.HasSystemPrompt,
		prev.OutboundToolsHMAC, curr.OutboundToolsHMAC, prev.OutboundToolCount, curr.OutboundToolCount,
		prev.OutboundPathHMACs, curr.OutboundPathHMACs,
	)

	if outboundDrift.Detected {
		outboundDrift.Attribution = AttributionGatewayOutputChanged
		outboundDrift.Reason = "Gateway transformation changed outbound request: " + outboundDrift.Reason
		return outboundDrift
	}

	// Stage 3: content is identical, but are the exact provider-bound bytes identical?
	if byteDrift, changed := compareToolBytes(prev, curr); changed {
		return byteDrift
	}

	return DriftResult{
		Detected:    false,
		Attribution: AttributionNone,
		Component:   "none",
		Path:        "",
		ChangeType:  "none",
		Confirmed:   false,
		Reason:      "Request structure is identical across client input and outbound provider request",
	}
}

// FlattenOutboundTools fingerprints provider-bound tools in any provider format by flattening each
// tool object generically (e.g. tools[0].function.parameters... or tools[0].input_schema...).
// Outbound snapshots are only compared with other outbound snapshots, so the path layout only
// needs to be consistent with itself.
func FlattenOutboundTools(tools []json.RawMessage) (string, map[string]string) {
	pathHMACs := make(map[string]string)
	for i, raw := range tools {
		if len(pathHMACs) >= maxFlattenedPaths {
			break
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		flattenJSONMap(fmt.Sprintf("tools[%d]", i), m, pathHMACs, 0)
	}
	if len(pathHMACs) == 0 {
		return "", pathHMACs
	}
	keys := make([]string, 0, len(pathHMACs))
	for k := range pathHMACs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var combined strings.Builder
	for _, k := range keys {
		combined.WriteString(k)
		combined.WriteString("=")
		combined.WriteString(pathHMACs[k])
		combined.WriteString(";")
	}
	return computeHMAC(domainPrefixTool, []byte(combined.String())), pathHMACs
}

// findFirstPathDiff finds the first alphabetical path that was added, removed, or modified.
func findFirstPathDiff(prev, curr map[string]string) (string, string) {
	allKeys := make(map[string]struct{})
	for k := range prev {
		allKeys[k] = struct{}{}
	}
	for k := range curr {
		allKeys[k] = struct{}{}
	}

	var sortedKeys []string
	for k := range allKeys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	for _, k := range sortedKeys {
		vPrev, inPrev := prev[k]
		vCurr, inCurr := curr[k]

		if inPrev && !inCurr {
			return k, "removed"
		}
		if !inPrev && inCurr {
			return k, "added"
		}
		if vPrev != vCurr {
			return k, "modified"
		}
	}

	return "tools", "modified"
}

// DiagnosisCategory represents the normalized diagnosis status.
type DiagnosisCategory string

const (
	DiagnosisConfirmedDrift     DiagnosisCategory = "confirmed_drift"
	DiagnosisStableMiss         DiagnosisCategory = "stable_miss"
	DiagnosisStableHit          DiagnosisCategory = "stable_hit"
	DiagnosisInitialRequest     DiagnosisCategory = "initial_request"
	DiagnosisUntracked          DiagnosisCategory = "untracked"
	DiagnosisRefused            DiagnosisCategory = "refused"
	DiagnosisStructurallyStable DiagnosisCategory = "structurally_stable"
	DiagnosisUsageUnavailable   DiagnosisCategory = "usage_unavailable"
)

// SessionData holds the history entries for a single session.
type SessionData struct {
	LastActive time.Time
	Snapshots  []RequestSnapshot
}

// SessionTracker is a thread-safe, memory-bounded LRU in-memory store.
type SessionTracker struct {
	mu          sync.Mutex
	maxSessions int
	maxEntries  int
	ttl         time.Duration
	sessions    map[string]*SessionData
}

func NewSessionTracker(maxSessions, maxEntries int, ttl time.Duration) *SessionTracker {
	if maxSessions <= 0 {
		maxSessions = DefaultMaxSessions
	}
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &SessionTracker{
		maxSessions: maxSessions,
		maxEntries:  maxEntries,
		ttl:         ttl,
		sessions:    make(map[string]*SessionData),
	}
}

func (st *SessionTracker) GetLatest(key string) (RequestSnapshot, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()

	data, exists := st.sessions[key]
	if !exists {
		return RequestSnapshot{}, false
	}

	// Check TTL
	if time.Since(data.LastActive) > st.ttl {
		delete(st.sessions, key)
		return RequestSnapshot{}, false
	}

	if len(data.Snapshots) == 0 {
		return RequestSnapshot{}, false
	}

	return data.Snapshots[len(data.Snapshots)-1], true
}

func (st *SessionTracker) Record(key string, snap RequestSnapshot) {
	st.mu.Lock()
	defer st.mu.Unlock()

	data, exists := st.sessions[key]
	if !exists {
		// Evict oldest if capacity exceeded
		if len(st.sessions) >= st.maxSessions {
			st.evictOldest()
		}
		data = &SessionData{
			LastActive: time.Now(),
			Snapshots:  make([]RequestSnapshot, 0, st.maxEntries),
		}
		st.sessions[key] = data
	}

	data.LastActive = time.Now()
	if len(data.Snapshots) >= st.maxEntries {
		// Shift ring buffer
		copy(data.Snapshots, data.Snapshots[1:])
		data.Snapshots[len(data.Snapshots)-1] = snap
	} else {
		data.Snapshots = append(data.Snapshots, snap)
	}
}

func (st *SessionTracker) evictOldest() {
	var oldestKey string
	var oldestTime time.Time

	for k, v := range st.sessions {
		if oldestKey == "" || v.LastActive.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.LastActive
		}
	}

	if oldestKey != "" {
		delete(st.sessions, oldestKey)
	}
}

func (st *SessionTracker) Clear() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sessions = make(map[string]*SessionData)
}

func (st *SessionTracker) Count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.sessions)
}

// SanitizeFieldPath strips control and unsafe characters, bounding the path to 128 characters.
func SanitizeFieldPath(path string) string {
	if path == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range path {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '.' || r == '[' || r == ']' || r == '-' {
			b.WriteRune(r)
		}
	}
	clean := b.String()
	if len(clean) > 128 {
		clean = clean[:125] + "..."
	}
	return clean
}

// FindingRateLimiter prevents log flooding from repeated identical warnings within a trusted scope.
type FindingRateLimiter struct {
	mu         sync.Mutex
	cooldown   time.Duration
	maxEntries int
	entries    map[string]time.Time
}

func NewFindingRateLimiter(cooldown time.Duration, maxEntries int) *FindingRateLimiter {
	if cooldown <= 0 {
		cooldown = 15 * time.Second
	}
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	return &FindingRateLimiter{
		cooldown:   cooldown,
		maxEntries: maxEntries,
		entries:    make(map[string]time.Time),
	}
}

// ShouldLog returns true if the event should be logged.
// If recently logged within the cooldown window, returns false.
func (rl *FindingRateLimiter) ShouldLog(scopeKey string, now time.Time) bool {
	if rl == nil || scopeKey == "" {
		return true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	last, exists := rl.entries[scopeKey]
	if exists && now.Sub(last) < rl.cooldown {
		return false
	}

	if len(rl.entries) >= rl.maxEntries {
		rl.evictOldest()
	}

	rl.entries[scopeKey] = now
	return true
}

func (rl *FindingRateLimiter) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	for k, t := range rl.entries {
		if oldestKey == "" || t.Before(oldestTime) {
			oldestKey = k
			oldestTime = t
		}
	}
	if oldestKey != "" {
		delete(rl.entries, oldestKey)
	}
}

func (rl *FindingRateLimiter) Clear() {
	if rl == nil {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.entries = make(map[string]time.Time)
}

// BuildDiagnosticFindings translates observed request states, drift, usage, and transformations
// into human-readable, privacy-safe diagnostic findings.
func BuildDiagnosticFindings(diag *RequestDiagnostic) []DiagnosticFinding {
	if diag == nil {
		return nil
	}

	var findings []DiagnosticFinding
	outboundObservable := diag.Snapshot.OutboundObservable

	// 1. Tool Schema Changed (SM001)
	if diag.Drift.Detected && diag.Drift.Component == "tools" && diag.Drift.ChangeType == "reordered" {
		source := EvidenceClientInput
		if diag.Drift.Attribution == AttributionGatewayOutputChanged {
			source = EvidenceGatewayState
		}
		findings = append(findings, DiagnosticFinding{
			Code:               CodeToolsReordered,
			Severity:           SeverityWarning,
			Component:          "tools",
			Path:               "tools",
			EvidenceSource:     source,
			Confidence:         ConfidenceConfirmed,
			Message:            "The same tools were sent in a different order. Tool order is part of the cached prompt, so reordering breaks cache reuse even though nothing else changed.",
			SuggestedAction:    "Sort tools deterministically (e.g. by name) before sending, especially tools discovered from MCP servers.",
			OutboundObservable: outboundObservable,
		})
	} else if diag.Drift.Detected && diag.Drift.Component == "tools" && diag.Drift.ChangeType == "serialization" {
		f := DiagnosticFinding{
			Code:               CodeToolSerializationChanged,
			Severity:           SeverityWarning,
			Component:          "tools",
			Path:               "tools",
			EvidenceSource:     EvidenceProviderOutput,
			Confidence:         ConfidenceConfirmed,
			Message:            "The tools sent to the provider had the same content but different JSON bytes (key order) than the previous request. Providers cache exact bytes, so this breaks cache reuse.",
			SuggestedAction:    "Find which side serializes tool schemas non-deterministically and make key order stable.",
			OutboundObservable: outboundObservable,
		}
		switch diag.Drift.Attribution {
		case AttributionGatewayOutputChanged:
			f.EvidenceSource = EvidenceGatewayState
			f.Message = "Bifrost sent the same tools to the provider with different JSON bytes (key order) than the previous request, although the client sent identical bytes. Providers cache exact bytes, so this breaks cache reuse."
			f.SuggestedAction = "This points to non-deterministic serialization in the gateway. Report it to the Bifrost maintainers with this finding and your Bifrost version."
		case AttributionClientInputChanged:
			f.EvidenceSource = EvidenceClientInput
			f.Message = "Your client sent the same tools with a different JSON key order than the previous request, and it reached the provider that way. Providers cache exact bytes, so this breaks cache reuse."
			f.SuggestedAction = "Serialize tool schemas with a stable key order in the client (e.g. sort keys)."
		}
		findings = append(findings, f)
	} else if diag.Drift.Detected && diag.Drift.Component == "tools" {
		source := EvidenceClientInput
		if diag.Drift.Attribution == AttributionGatewayOutputChanged {
			source = EvidenceGatewayState
		}
		cleanPath := SanitizeFieldPath(diag.Drift.Path)
		findings = append(findings, DiagnosticFinding{
			Code:               CodeToolSchemaChanged,
			Severity:           SeverityWarning,
			Component:          "tools",
			Path:               cleanPath,
			EvidenceSource:     source,
			Confidence:         ConfidenceConfirmed,
			Message:            "Tool parameter structure changed within this session. This may reduce provider prompt-cache reuse.",
			SuggestedAction:    "Keep shared tool definitions consistent across requests when possible.",
			OutboundObservable: outboundObservable,
		})
	}

	// 1b. System prompt changed (SM008), or changed only by a dynamic value (SM009)
	if diag.Drift.Detected && diag.Drift.Component == "system_prompt" {
		source := EvidenceClientInput
		if diag.Drift.Attribution == AttributionGatewayOutputChanged {
			source = EvidenceGatewayState
		}
		f := DiagnosticFinding{
			Code:               CodeSystemPromptChanged,
			Severity:           SeverityWarning,
			Component:          "system_prompt",
			Path:               SanitizeFieldPath(diag.Drift.Path),
			EvidenceSource:     source,
			Confidence:         ConfidenceConfirmed,
			Message:            "System prompt changed within this session. Providers cache from the start of the prompt, so a change here invalidates the cached prefix after the change point.",
			SuggestedAction:    "Keep the system prompt identical across requests; put per-request content at the end of the conversation.",
			OutboundObservable: outboundObservable,
		}
		if diag.Drift.VolatileOnly && source == EvidenceClientInput {
			f.Code = CodeVolatileValueInPrefix
			f.Message = "The only change to the system prompt was a value that changes on every request (date, time, timestamp or ID). This breaks prompt-cache reuse on every call."
			f.SuggestedAction = "Move the dynamic value out of the system prompt into the latest user message, or round it (e.g. date only)."
		}
		findings = append(findings, f)
	}

	// 2. Gateway Added a Cache Breakpoint (SM002)
	if diag.Transformation.Type == TransformationCacheControlInjected {
		cleanPath := SanitizeFieldPath(diag.Transformation.Path)
		findings = append(findings, DiagnosticFinding{
			Code:               CodeGatewayBreakpointInjected,
			Severity:           SeverityInfo,
			Component:          "cache_control",
			Path:               cleanPath,
			EvidenceSource:     EvidenceGatewayState,
			Confidence:         ConfidenceConfirmed,
			Message:            "Bifrost inserted a cache-control marker before forwarding the request.",
			SuggestedAction:    "This may be expected. Review provider prompt-cache settings if cache usage is unexpected.",
			OutboundObservable: outboundObservable,
		})
	}

	// 3. Gateway Clamped Cache Breakpoints (SM007)
	if diag.Transformation.Type == TransformationCacheControlClamped {
		cleanPath := SanitizeFieldPath(diag.Transformation.Path)
		findings = append(findings, DiagnosticFinding{
			Code:               CodeGatewayBreakpointClamped,
			Severity:           SeverityInfo,
			Component:          "cache_control",
			Path:               cleanPath,
			EvidenceSource:     EvidenceGatewayState,
			Confidence:         ConfidenceConfirmed,
			Message:            "Bifrost reduced the number of cache-control markers before forwarding to the provider.",
			SuggestedAction:    "Ensure client markers do not exceed provider-supported breakpoint limits.",
			OutboundObservable: outboundObservable,
		})
	}

	// 4. Multi-marker configuration observed (SM006)
	if diag.Transformation.Type == TransformationMultiMarkerObserved {
		cleanPath := SanitizeFieldPath(diag.Transformation.Path)
		findings = append(findings, DiagnosticFinding{
			Code:               CodeMultiMarkerConfigObserved,
			Severity:           SeverityInfo,
			Component:          "cache_control",
			Path:               cleanPath,
			EvidenceSource:     EvidenceClientInput,
			Confidence:         ConfidenceObserved,
			Message:            "Multiple cache-control breakpoints observed on OpenRouter request. OpenRouter uses only the final breakpoint for Gemini models.",
			SuggestedAction:    "Verify that the final breakpoint corresponds to your intended static prompt prefix.",
			OutboundObservable: outboundObservable,
		})
	}

	// 5. Stable request, no reported cached tokens (SM003)
	// Requires both usage metadata and explicit prompt token details.
	// Because Bifrost's Go schema ChatPromptTokensDetails normalizes absent cached read tokens
	// to zero (int), an explicit zero cannot be distinguished with certainty from an omitted metric.
	if diag.Diagnosis == DiagnosisStableMiss && diag.HasUsage && diag.HasPromptDetails {
		findings = append(findings, DiagnosticFinding{
			Code:               CodeStableMissUnknownCause,
			Severity:           SeverityInfo,
			Component:          "provider_usage",
			Path:               "",
			EvidenceSource:     EvidenceProviderUsage,
			Confidence:         ConfidenceObserved,
			Message:            "Zero cached read tokens observed for this structurally stable request prefix. Upstream schemas normalize omitted cache metrics to zero, so this may represent an explicit cache miss, cache ineligibility, or a provider omitting read metrics.",
			SuggestedAction:    "Check provider cache eligibility, minimum prompt length, TTL, routing, and relevant caching configuration.",
			OutboundObservable: outboundObservable,
		})
	}

	// 6. Provider or model changed within session (SM005)
	if diag.DomainChanged {
		findings = append(findings, DiagnosticFinding{
			Code:               CodeProviderOrModelChanged,
			Severity:           SeverityInfo,
			Component:          "session",
			Path:               "",
			EvidenceSource:     EvidenceClientInput,
			Confidence:         ConfidenceConfirmed,
			Message:            "Cache history was isolated because this request used a different provider or model.",
			SuggestedAction:    "Ensure requests within the same cache session target the same provider and model.",
			OutboundObservable: outboundObservable,
		})
	}

	// 7. Outbound verification unavailable (SM004)
	// Only emitted when relevant to an unverified attribution (diag.Attribution == AttributionOriginUnverified)
	// to avoid noisy warnings on routine requests where outbound verification was not required.
	if diag.SessionID != "" && !outboundObservable && diag.Attribution == AttributionOriginUnverified {
		findings = append(findings, DiagnosticFinding{
			Code:               CodeOutboundVerificationUnavailable,
			Severity:           SeverityInfo,
			Component:          "gateway_state",
			Path:               "",
			EvidenceSource:     EvidenceGatewayState,
			Confidence:         ConfidenceObserved,
			Message:            "StillMark cannot determine whether provider-bound content changed because outbound inspection is disabled.",
			SuggestedAction:    "Use the isolated diagnostic workflow with synthetic requests to inspect outbound transformations.",
			OutboundObservable: false,
		})
	}

	return findings
}

// ValidateAndApplyConfig parses and validates plugin configuration with strict fail-open defaults.
func ValidateAndApplyConfig(raw any) (PluginConfig, error) {
	cfg := DefaultPluginConfig()
	if raw == nil {
		return cfg, nil
	}

	switch v := raw.(type) {
	case PluginConfig:
		cfg = v
	case *PluginConfig:
		if v != nil {
			cfg = *v
		}
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid config format, using defaults: %w", err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("failed to parse config JSON, using defaults: %w", err)
		}
	}

	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = DefaultMaxSessions
	} else if cfg.MaxSessions > 100000 {
		cfg.MaxSessions = 100000
	} else if cfg.MaxSessions < 10 {
		cfg.MaxSessions = 10
	}
	if cfg.MaxEntriesPerSession <= 0 {
		cfg.MaxEntriesPerSession = DefaultMaxEntries
	} else if cfg.MaxEntriesPerSession > 100 {
		cfg.MaxEntriesPerSession = 100
	}
	if _, err := time.ParseDuration(cfg.SessionTTL); err != nil {
		cfg.SessionTTL = "30m"
	}
	if cfg.InputPricePerMTok <= 0 || cfg.InputPricePerMTok > 1000 {
		cfg.InputPricePerMTok = DefaultInputPricePerMTok
	}
	if cfg.CachedInputPricePerMTok < 0 || cfg.CachedInputPricePerMTok > cfg.InputPricePerMTok {
		cfg.CachedInputPricePerMTok = cfg.InputPricePerMTok * 0.1
	}
	switch strings.ToLower(cfg.SeverityThreshold) {
	case "info", "warning", "error":
		cfg.SeverityThreshold = strings.ToLower(cfg.SeverityThreshold)
	default:
		cfg.SeverityThreshold = "info"
	}

	return cfg, nil
}
