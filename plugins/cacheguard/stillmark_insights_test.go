package main

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

func initKey(t *testing.T) {
	t.Helper()
	if err := InitEngine(100, 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(CleanupEngine)
}

func sysReq(system string) *schemas.BifrostChatRequest {
	return &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &system}},
	}}
}

func TestMaskVolatileValues(t *testing.T) {
	cases := map[string]string{
		"Now: 2026-11-07 22:41:03. Be nice.":             "Now: <v>. Be nice.",
		"Now: 2026-11-07T22:41:03Z":                      "Now: <v>",
		"req 3f2b8c1e-9d4a-4b7e-a1c2-0e9f8d7c6b5a ok":    "req <v> ok",
		"Today is November 7, 2026.":                     "Today is <v>.",
		"ts=1762535463 done":                             "ts=<v> done",
		"Rule 12: refund window of 14 days, order 0042.": "Rule 12: refund window of 14 days, order 0042.",
	}
	for in, want := range cases {
		if got := string(MaskVolatileValues([]byte(in))); got != want {
			t.Errorf("MaskVolatileValues(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVolatileOnlySystemPromptDriftIsCG009(t *testing.T) {
	initKey(t)
	p1, m1, _ := ExtractPrefixFingerprints(sysReq("Current time: 2026-11-07 22:41:03\nYou are a support agent."))
	p2, m2, _ := ExtractPrefixFingerprints(sysReq("Current time: 2026-11-07 22:41:19\nYou are a support agent."))
	if p1 == p2 {
		t.Fatal("expected raw prefix HMACs to differ")
	}
	if m1 != m2 {
		t.Fatal("expected masked prefix HMACs to match")
	}
	drift := CompareSnapshots(RequestSnapshot{PrefixHMAC: p1, PrefixMaskedHMAC: m1, HasSystemPrompt: true},
		RequestSnapshot{PrefixHMAC: p2, PrefixMaskedHMAC: m2, HasSystemPrompt: true})
	if !drift.Detected || drift.Component != "system_prompt" || !drift.VolatileOnly {
		t.Fatalf("expected volatile-only system prompt drift, got %+v", drift)
	}
	f := BuildDiagnosticFindings(&RequestDiagnostic{Drift: drift})
	if len(f) != 1 || f[0].Code != CodeVolatileValueInPrefix || f[0].Severity != SeverityWarning {
		t.Fatalf("expected one CG009 warning, got %+v", f)
	}
}

func TestRealSystemPromptEditIsCG008(t *testing.T) {
	initKey(t)
	p1, m1, _ := ExtractPrefixFingerprints(sysReq("You are a support agent."))
	p2, m2, _ := ExtractPrefixFingerprints(sysReq("You are a helpful support agent."))
	drift := CompareSnapshots(RequestSnapshot{PrefixHMAC: p1, PrefixMaskedHMAC: m1, HasSystemPrompt: true},
		RequestSnapshot{PrefixHMAC: p2, PrefixMaskedHMAC: m2, HasSystemPrompt: true})
	if drift.VolatileOnly {
		t.Fatal("a real wording change must not be classified as volatile-only")
	}
	f := BuildDiagnosticFindings(&RequestDiagnostic{Drift: drift})
	if len(f) != 1 || f[0].Code != CodeSystemPromptChanged {
		t.Fatalf("expected one CG008 finding, got %+v", f)
	}
}

func TestEstimateExtraCostPer1k(t *testing.T) {
	// 2,318 tokens lost at $3.00 vs $0.30 per MTok = $6.2586 per 1,000 requests
	got := EstimateExtraCostPer1k(2318, 3.00, 0.30)
	if got < 6.258 || got > 6.259 {
		t.Fatalf("got %.4f", got)
	}
	if EstimateExtraCostPer1k(0, 3, 0.3) != 0 || EstimateExtraCostPer1k(100, 0.3, 3) != 0 {
		t.Fatal("expected zero for no loss or inverted prices")
	}
}

func TestPriceConfigValidation(t *testing.T) {
	cfg, err := ValidateAndApplyConfig(map[string]any{"input_price_per_mtok": -1, "cached_input_price_per_mtok": 99})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InputPricePerMTok != DefaultInputPricePerMTok || cfg.CachedInputPricePerMTok > cfg.InputPricePerMTok {
		t.Fatalf("invalid prices not corrected: %+v", cfg)
	}
}

func strp(s string) *string { return &s }

func tools(names ...string) []schemas.ChatTool {
	out := make([]schemas.ChatTool, 0, len(names))
	for _, n := range names {
		out = append(out, schemas.ChatTool{Type: schemas.ChatToolTypeFunction, Function: &schemas.ChatToolFunction{Name: n, Description: strp("does " + n)}})
	}
	return out
}

func TestToolReorderIsCG010(t *testing.T) {
	initKey(t)
	h1, p1, _ := FlattenToolSchemasWithStatus(tools("get_order", "refund", "search"))
	h2, p2, _ := FlattenToolSchemasWithStatus(tools("search", "get_order", "refund"))
	drift := CompareSnapshots(RequestSnapshot{ToolsHMAC: h1, PathHMACs: p1, ToolCount: 3},
		RequestSnapshot{ToolsHMAC: h2, PathHMACs: p2, ToolCount: 3})
	if !drift.Detected || drift.ChangeType != "reordered" {
		t.Fatalf("expected reorder drift, got %+v", drift)
	}
	f := BuildDiagnosticFindings(&RequestDiagnostic{Drift: drift})
	if len(f) != 1 || f[0].Code != CodeToolsReordered {
		t.Fatalf("expected one CG010, got %+v", f)
	}
}

func TestToolEditIsNotReorder(t *testing.T) {
	initKey(t)
	_, p1, _ := FlattenToolSchemasWithStatus(tools("get_order", "refund"))
	_, p2, _ := FlattenToolSchemasWithStatus(tools("refund", "get_orders"))
	if IsToolReorder(p1, p2, 2) {
		t.Fatal("a renamed tool must not be reported as a pure reorder")
	}
	if IsToolReorder(p1, p1, 2) {
		t.Fatal("identical order is not a reorder")
	}
}

func TestAutoSessionPrefersMatchingWorkload(t *testing.T) {
	tr := NewSessionTracker(10, 10, time.Minute)
	appA := RequestSnapshot{PrefixHMAC: "a1", PrefixMaskedHMAC: "A", ToolsHMAC: "TA"}
	appB := RequestSnapshot{PrefixHMAC: "b1", PrefixMaskedHMAC: "B", ToolsHMAC: "TB"}
	tr.Record("k", appA)
	tr.Record("k", appB)
	got, ok := tr.GetBestMatch("k", RequestSnapshot{PrefixMaskedHMAC: "A", ToolsHMAC: "TA"})
	if !ok || got.PrefixHMAC != "a1" {
		t.Fatalf("expected app A baseline, got %+v", got)
	}
	got, _ = tr.GetBestMatch("k", RequestSnapshot{PrefixMaskedHMAC: "Z", ToolsHMAC: "TZ"})
	if got.PrefixHMAC != "b1" {
		t.Fatal("expected fallback to latest snapshot")
	}
}

func TestReportAggregatesCostAndHidesTenant(t *testing.T) {
	initKey(t)
	r := NewInsightReport()
	d := &RequestDiagnostic{TenantID: "virtual_key:vk-secret-name", Model: "m", HasUsage: true, HasPromptDetails: true,
		PromptTokens: 2000, CachedReadTokens: 0, CacheTokensLost: 2000,
		Drift:    DriftResult{Detected: true, Component: "system_prompt"},
		Findings: []DiagnosticFinding{{Code: CodeVolatileValueInPrefix, Component: "system_prompt", Path: "messages[0].content"}}}
	r.Record(d, 3.0, 0.3)
	r.Record(d, 3.0, 0.3)
	snap := r.Snapshot(3.0, 0.3)
	issues := snap["top_issues"].([]reportIssue)
	if len(issues) != 1 || issues[0].Occurrences != 2 || issues[0].TokensLost != 4000 {
		t.Fatalf("unexpected issues: %+v", issues)
	}
	if issues[0].Tenant == "" || issues[0].Tenant == d.TenantID {
		t.Fatal("tenant must be a hashed label, never the raw ID")
	}
	if tot := snap["totals"].(reportTotals); tot.EstExtraUSD < 0.0107 || tot.EstExtraUSD > 0.0109 {
		t.Fatalf("expected ~$0.0108 extra, got %+v", tot)
	}
}

func TestReportOnlyBindsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:8099": true, "localhost:8099": true, "[::1]:8099": true,
		"0.0.0.0:8099": false, "10.0.0.5:8099": false, ":8099": false} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestRawToolsHMACIsKeyOrderSensitive(t *testing.T) {
	initKey(t)
	a := RawToolsHMAC([]byte(`{"model":"m","tools":[{"type":"function","function":{"name":"x","parameters":{"type":"object","properties":{}}}}]}`))
	b := RawToolsHMAC([]byte(`{"tools":[{"type":"function","function":{"name":"x","parameters":{"properties":{},"type":"object"}}}],"model":"m"}`))
	c := RawToolsHMAC([]byte("{ \"tools\" : [ {\"type\":\"function\",\"function\":{\"name\":\"x\",\"parameters\":{\"type\":\"object\",\"properties\":{}}}} ] }"))
	if a == "" || a == b {
		t.Fatal("key order change must change the byte fingerprint")
	}
	if a != c {
		t.Fatal("whitespace-only differences must not change the byte fingerprint")
	}
	if RawToolsHMAC([]byte(`{"model":"m"}`)) != "" || RawToolsHMAC([]byte(`not json`)) != "" {
		t.Fatal("expected empty fingerprint without tools")
	}
}

func TestToolBytesAttribution(t *testing.T) {
	base := RequestSnapshot{OutboundObservable: true, PrefixHMAC: "p", OutboundPrefixHMAC: "p", ToolsHMAC: "t", OutboundToolsHMAC: "t"}
	prev, curr := base, base
	prev.ClientToolsRawHMAC, curr.ClientToolsRawHMAC = "c1", "c1"
	prev.OutboundToolsRawHMAC, curr.OutboundToolsRawHMAC = "o1", "o2"

	drift := CompareSnapshots(prev, curr)
	if !drift.Detected || drift.ChangeType != "serialization" || drift.Attribution != AttributionGatewayOutputChanged {
		t.Fatalf("expected gateway serialization drift, got %+v", drift)
	}
	f := BuildDiagnosticFindings(&RequestDiagnostic{Drift: drift, Snapshot: curr})
	if len(f) != 1 || f[0].Code != CodeToolSerializationChanged || f[0].EvidenceSource != EvidenceGatewayState {
		t.Fatalf("expected gateway CG011, got %+v", f)
	}

	curr.ClientToolsRawHMAC = "c2"
	if d := CompareSnapshots(prev, curr); d.Attribution != AttributionClientInputChanged {
		t.Fatalf("client byte change must be attributed to the client, got %+v", d)
	}
	curr.ClientToolsRawHMAC, prev.ClientToolsRawHMAC = "", ""
	if d := CompareSnapshots(prev, curr); d.Attribution != AttributionOriginUnverified {
		t.Fatalf("unknown client bytes must be unverified, got %+v", d)
	}
	curr.OutboundToolsRawHMAC = "o1"
	if d := CompareSnapshots(prev, curr); d.Detected {
		t.Fatalf("identical bytes must not drift, got %+v", d)
	}
}
