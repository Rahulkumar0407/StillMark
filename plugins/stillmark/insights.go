package main

// insights.go: zero-config session tracking, tool reorder detection, and the
// operator report (aggregated cache-breakers by tokens and estimated cost).
// Like the rest of the plugin, nothing here stores prompt text, tool text or raw tenant IDs.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	domainPrefixToolBytes = "stillmark:tool-bytes:"
	maxRawBodyForBytes    = 4 << 20 // skip byte fingerprinting for bodies over 4 MiB

	autoSessionID      = "auto"
	domainPrefixTenant = "stillmark:tenant:"
	maxReportIssues    = 500
)

// ---------------------------------------------------------------------------
// Zero-config sessions
// ---------------------------------------------------------------------------

// GetBestMatch returns the snapshot a new request should be compared against.
// Explicit sessions use the latest snapshot. Auto sessions (no session header) may mix
// several workloads under one virtual key, so we prefer the most recent snapshot that shares
// the request's masked system prompt or its tool set, and fall back to the latest one.
func (st *SessionTracker) GetBestMatch(key string, curr RequestSnapshot) (RequestSnapshot, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()

	data, exists := st.sessions[key]
	if !exists || len(data.Snapshots) == 0 {
		return RequestSnapshot{}, false
	}
	if time.Since(data.LastActive) > st.ttl {
		delete(st.sessions, key)
		return RequestSnapshot{}, false
	}
	for i := len(data.Snapshots) - 1; i >= 0; i-- {
		s := data.Snapshots[i]
		if (s.PrefixMaskedHMAC != "" && s.PrefixMaskedHMAC == curr.PrefixMaskedHMAC) ||
			(s.ToolsHMAC != "" && s.ToolsHMAC == curr.ToolsHMAC) {
			return s, true
		}
	}
	return data.Snapshots[len(data.Snapshots)-1], true
}

// previousSnapshot picks the comparison baseline for a request according to its session mode.
func previousSnapshot(tracker *SessionTracker, diag *RequestDiagnostic) (RequestSnapshot, bool) {
	if diag.AutoSession {
		return tracker.GetBestMatch(diag.CompositeKey, diag.Snapshot)
	}
	return tracker.GetLatest(diag.CompositeKey)
}

// ---------------------------------------------------------------------------
// Tool reorder detection
// ---------------------------------------------------------------------------

// toolFingerprints groups flattened schema paths by tool index and returns one
// order-independent fingerprint per tool, in request order.
func toolFingerprints(paths map[string]string, count int) []string {
	parts := make([][]string, count)
	for p, h := range paths {
		if !strings.HasPrefix(p, "tools[") {
			continue
		}
		end := strings.Index(p, "]")
		if end < 0 {
			continue
		}
		idx, err := strconv.Atoi(p[6:end])
		if err != nil || idx < 0 || idx >= count {
			continue
		}
		parts[idx] = append(parts[idx], p[end+1:]+"="+h)
	}
	out := make([]string, count)
	for i, ps := range parts {
		sort.Strings(ps)
		out[i] = strings.Join(ps, ";")
	}
	return out
}

// IsToolReorder reports whether two tool lists contain exactly the same tools in a different order.
func IsToolReorder(prevPaths, currPaths map[string]string, count int) bool {
	if count < 2 {
		return false
	}
	a, b := toolFingerprints(prevPaths, count), toolFingerprints(currPaths, count)
	sameOrder := true
	for i := range a {
		if a[i] != b[i] {
			sameOrder = false
			break
		}
	}
	if sameOrder {
		return false
	}
	sa, sb := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Operator report
// ---------------------------------------------------------------------------

type reportIssue struct {
	Code            string             `json:"code"`
	Severity        DiagnosticSeverity `json:"severity"`
	Component       string             `json:"component"`
	Path            string             `json:"path,omitempty"`
	Tenant          string             `json:"tenant"` // keyed hash label, never the raw tenant ID
	Model           string             `json:"model"`
	Occurrences     int                `json:"occurrences"`
	TokensLost      int                `json:"tokens_lost"`
	EstExtraUSD     float64            `json:"est_extra_usd"`
	LastSeen        time.Time          `json:"last_seen"`
	Message         string             `json:"message"`
	SuggestedAction string             `json:"suggested_action"`
}

type reportTotals struct {
	Requests      int     `json:"requests_with_usage"`
	PromptTokens  int     `json:"prompt_tokens"`
	CachedTokens  int     `json:"cached_tokens"`
	CacheHitRatio float64 `json:"cache_hit_ratio"`
	DriftEvents   int     `json:"drift_events"`
	TokensLost    int     `json:"tokens_lost"`
	EstExtraUSD   float64 `json:"est_extra_usd"`
}

// InsightReport aggregates findings across requests. Bounded in memory.
type InsightReport struct {
	mu     sync.Mutex
	since  time.Time
	totals reportTotals
	issues map[string]*reportIssue
}

func NewInsightReport() *InsightReport {
	return &InsightReport{since: time.Now().UTC(), issues: make(map[string]*reportIssue)}
}

var (
	globalReportMu sync.RWMutex
	globalReport   *InsightReport

	reportServerMu sync.Mutex
	reportServer   *http.Server
)

func tenantLabel(tenantID string) string {
	if tenantID == "" {
		return "untrusted"
	}
	h := computeHMAC(domainPrefixTenant, []byte(tenantID))
	if len(h) > 12 {
		h = h[:12]
	}
	return "t_" + h
}

// Record adds one completed request to the report. Safe to call once per request.
func (r *InsightReport) Record(diag *RequestDiagnostic, inputPrice, cachedPrice float64) {
	if r == nil || diag == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if diag.HasUsage && diag.HasPromptDetails {
		r.totals.Requests++
		r.totals.PromptTokens += diag.PromptTokens
		r.totals.CachedTokens += diag.CachedReadTokens
		if r.totals.PromptTokens > 0 {
			r.totals.CacheHitRatio = float64(r.totals.CachedTokens) / float64(r.totals.PromptTokens)
		}
	}
	extraUSD := 0.0
	if diag.CacheTokensLost > 0 {
		extraUSD = EstimateExtraCostPer1k(diag.CacheTokensLost, inputPrice, cachedPrice) / 1000
	}
	if diag.Drift.Detected {
		r.totals.DriftEvents++
		r.totals.TokensLost += diag.CacheTokensLost
		r.totals.EstExtraUSD += extraUSD
	}

	tenant := tenantLabel(diag.TenantID)
	for _, f := range diag.Findings {
		if f.Code == CodeOutboundVerificationUnavailable {
			continue // operational notice, not a cache-breaker
		}
		key := strings.Join([]string{f.Code, f.Component, f.Path, tenant, diag.Model}, "|")
		is, ok := r.issues[key]
		if !ok {
			if len(r.issues) >= maxReportIssues {
				r.evictOldestLocked()
			}
			is = &reportIssue{Code: f.Code, Severity: f.Severity, Component: f.Component, Path: f.Path,
				Tenant: tenant, Model: diag.Model, Message: f.Message, SuggestedAction: f.SuggestedAction}
			r.issues[key] = is
		}
		is.Occurrences++
		is.LastSeen = time.Now().UTC()
		if diag.Drift.Detected && f.Component == diag.Drift.Component {
			is.TokensLost += diag.CacheTokensLost
			is.EstExtraUSD += extraUSD
		}
	}
}

func (r *InsightReport) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, v := range r.issues {
		if oldestKey == "" || v.LastSeen.Before(oldest) {
			oldestKey, oldest = k, v.LastSeen
		}
	}
	delete(r.issues, oldestKey)
}

// Snapshot returns the report as a JSON-serializable value, issues sorted by tokens lost.
func (r *InsightReport) Snapshot(inputPrice, cachedPrice float64) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	issues := make([]reportIssue, 0, len(r.issues))
	for _, v := range r.issues {
		issues = append(issues, *v)
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].TokensLost != issues[j].TokensLost {
			return issues[i].TokensLost > issues[j].TokensLost
		}
		return issues[i].Occurrences > issues[j].Occurrences
	})
	return map[string]any{
		"generated_at": time.Now().UTC(),
		"since":        r.since,
		"totals":       r.totals,
		"price_reference_usd_per_mtok": map[string]float64{
			"input": inputPrice, "cached_input": cachedPrice,
		},
		"note":       "Costs are estimates from reference prices, not provider bills.",
		"top_issues": issues,
	}
}

func recordReport(diag *RequestDiagnostic) {
	if diag == nil || diag.reported {
		return
	}
	globalReportMu.RLock()
	r := globalReport
	globalReportMu.RUnlock()
	if r == nil {
		return
	}
	activeConfigMu.RLock()
	in, cached := activeConfig.InputPricePerMTok, activeConfig.CachedInputPricePerMTok
	activeConfigMu.RUnlock()
	r.Record(diag, in, cached)
	diag.reported = true
}

// isLoopbackAddr only allows the report to bind to the local machine.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func reportHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	globalReportMu.RLock()
	r := globalReport
	globalReportMu.RUnlock()
	if r == nil {
		http.Error(w, "report unavailable", http.StatusServiceUnavailable)
		return
	}
	activeConfigMu.RLock()
	in, cached := activeConfig.InputPricePerMTok, activeConfig.CachedInputPricePerMTok
	activeConfigMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r.Snapshot(in, cached))
}

// startReportServer starts the operator report on a loopback address. Fail-open: errors are logged only.
func startReportServer(addr string) {
	if addr == "" {
		return
	}
	if !isLoopbackAddr(addr) {
		fmt.Printf("[StillMark] report_addr %q refused: only loopback addresses are allowed\n", addr)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/stillmark/report", reportHandler)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("[StillMark] report server not started: %v\n", err)
		return
	}
	reportServerMu.Lock()
	reportServer = srv
	reportServerMu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	fmt.Printf("[StillMark] operator report at http://%s/stillmark/report\n", addr)
}

func stopReportServer() {
	reportServerMu.Lock()
	srv := reportServer
	reportServer = nil
	reportServerMu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// ---------------------------------------------------------------------------
// Byte-exact tool serialization (SM011)
// ---------------------------------------------------------------------------

// rawPayloadBytes converts a captured request payload into bytes without re-encoding when possible.
func rawPayloadBytes(raw any) []byte {
	switch v := raw.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return v
	case []byte:
		return v
	case string:
		return []byte(v)
	default:
		return nil // already decoded: original key order is lost, so byte comparison is impossible
	}
}

// RawToolsHMAC fingerprints the exact bytes of the top-level "tools" array (whitespace-insensitive,
// key-order-sensitive). Providers cache exact bytes, so two semantically identical tool lists with
// different key order produce different fingerprints here but identical schema-path fingerprints.
func RawToolsHMAC(body []byte) string {
	if len(body) == 0 || len(body) > maxRawBodyForBytes {
		return ""
	}
	var top struct {
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &top); err != nil || len(top.Tools) == 0 || string(top.Tools) == "null" {
		return ""
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, top.Tools); err != nil {
		return ""
	}
	return computeHMAC(domainPrefixToolBytes, compact.Bytes())
}

// compareToolBytes runs after semantic comparison found no change. It reports a serialization-only
// change in the provider-bound tools and attributes it to the client or the gateway.
func compareToolBytes(prev, curr RequestSnapshot) (DriftResult, bool) {
	if prev.OutboundToolsRawHMAC == "" || curr.OutboundToolsRawHMAC == "" || prev.OutboundToolsRawHMAC == curr.OutboundToolsRawHMAC {
		return DriftResult{}, false
	}
	attr := AttributionOriginUnverified
	if prev.ClientToolsRawHMAC != "" && curr.ClientToolsRawHMAC != "" {
		if prev.ClientToolsRawHMAC != curr.ClientToolsRawHMAC {
			attr = AttributionClientInputChanged
		} else {
			attr = AttributionGatewayOutputChanged
		}
	}
	return DriftResult{
		Detected:    true,
		Attribution: attr,
		Component:   "tools",
		Path:        "tools",
		ChangeType:  "serialization",
		Confirmed:   true,
		Reason:      "Provider-bound tool JSON bytes changed although tool content is identical (key order)",
	}, true
}
