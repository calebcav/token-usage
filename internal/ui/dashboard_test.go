package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/calebcav/token-usage/internal/app"
	"github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

func TestViewRendersUsageAndInclusiveReasoning(t *testing.T) {
	tokens, _ := usage.NewTokens(1_000, 2_000, 500, 750, 250)
	snapshot := usage.Snapshot{
		Harness:   "codex",
		PaneID:    "w1:p1",
		SessionID: "session-1",
		Model:     "gpt-5.6",
		State:     "idle",
		Tokens:    tokens,
		Context:   &usage.ContextWindow{Used: 4_250, Limit: 10_000},
		Quota: &usage.QuotaSnapshot{Windows: []usage.QuotaWindow{
			{Label: "5h", UsedPercent: 42},
			{Label: "7d", UsedPercent: 73},
		}},
		Confidence:  usage.ConfidenceExact,
		CollectedAt: time.Now(),
	}
	m := model{width: 110, results: []app.Result{{Target: collector.Target{PaneID: "w1:p1"}, Snapshot: &snapshot}}}
	content := m.View().Content
	for _, want := range []string{"TOKEN USAGE", "SPENT  1.8k", "CODEX", "gpt-5.6", "processed 4.2k", "43%", "━", "5.8k left", "ACCOUNT LIMITS", "5h 42% used", "7d 73% used", "reasoning 250 (inside output)", "match exact"} {
		if !strings.Contains(strings.ToUpper(content), strings.ToUpper(want)) {
			t.Fatalf("view missing %q:\n%s", want, content)
		}
	}
}

func TestSubtitleMarksCachedSnapshots(t *testing.T) {
	m := model{
		lastRefresh: time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC),
		results:     []app.Result{{FromCache: true}},
	}
	if subtitle := m.subtitle(); !strings.Contains(subtitle, "cached ≤15s") {
		t.Fatalf("subtitle = %q, want cache TTL marker", subtitle)
	}
}

func TestContextBarSegmentsClampToAvailableWidth(t *testing.T) {
	tests := []struct {
		name         string
		percent      float64
		width        int
		wantFilled   int
		wantUnfilled int
	}{
		{name: "negative", percent: -1, width: 10, wantFilled: 0, wantUnfilled: 10},
		{name: "empty", percent: 0, width: 10, wantFilled: 0, wantUnfilled: 10},
		{name: "quarter", percent: 25, width: 10, wantFilled: 3, wantUnfilled: 7},
		{name: "warning", percent: 70, width: 10, wantFilled: 7, wantUnfilled: 3},
		{name: "critical", percent: 90, width: 10, wantFilled: 9, wantUnfilled: 1},
		{name: "full", percent: 100, width: 10, wantFilled: 10, wantUnfilled: 0},
		{name: "over", percent: 125, width: 10, wantFilled: 10, wantUnfilled: 0},
		{name: "no width", percent: 50, width: 0, wantFilled: 0, wantUnfilled: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filled, unfilled := barSegments(test.percent, test.width)
			if filled != test.wantFilled || unfilled != test.wantUnfilled {
				t.Fatalf("barSegments(%v, %d) = (%d, %d), want (%d, %d)",
					test.percent, test.width, filled, unfilled, test.wantFilled, test.wantUnfilled)
			}
		})
	}
}

func TestContextBarHasExactVisibleWidth(t *testing.T) {
	window := &usage.ContextWindow{Used: 42, Limit: 100}
	for _, width := range []int{1, 7, 24} {
		if got := lipgloss.Width(renderContextBar(window, width)); got != width {
			t.Fatalf("renderContextBar width = %d, want %d", got, width)
		}
	}
	if got := renderContextBar(nil, 10); got != "" {
		t.Fatalf("nil context bar = %q, want empty", got)
	}
}

func TestContextSeverityThresholds(t *testing.T) {
	tests := []struct {
		percent float64
		want    contextSeverity
	}{
		{percent: 69.9, want: contextHealthy},
		{percent: 70, want: contextWarning},
		{percent: 89.9, want: contextWarning},
		{percent: 90, want: contextCritical},
		{percent: 125, want: contextCritical},
	}
	for _, test := range tests {
		if got := contextSeverityFor(test.percent); got != test.want {
			t.Fatalf("contextSeverityFor(%v) = %v, want %v", test.percent, got, test.want)
		}
	}
}

func TestViewRendersCompactContextBar(t *testing.T) {
	tokens, _ := usage.NewTokens(100, 200, 0, 50, 0)
	snapshot := usage.Snapshot{
		Harness:    "codex",
		PaneID:     "w1:p1",
		SessionID:  "session-1",
		Model:      "gpt-5.6",
		Tokens:     tokens,
		Context:    &usage.ContextWindow{Used: 75, Limit: 100},
		Confidence: usage.ConfidenceExact,
	}
	content := (model{width: 64, results: []app.Result{{Snapshot: &snapshot}}}).View().Content
	for _, want := range []string{"━", "75%", "spent 150", "processed 350"} {
		if !strings.Contains(content, want) {
			t.Fatalf("compact context view missing %q:\n%s", want, content)
		}
	}
}

func TestViewExplainsMissingContextWithoutFabricatingZeroPercent(t *testing.T) {
	tokens, _ := usage.NewTokens(100, 0, 0, 50, 0)
	snapshot := usage.Snapshot{
		Harness:    "opencode",
		PaneID:     "w1:p1",
		SessionID:  "session-1",
		Tokens:     tokens,
		Confidence: usage.ConfidenceExact,
	}
	content := (model{width: 64, results: []app.Result{{Snapshot: &snapshot}}}).View().Content
	if !strings.Contains(content, "not reported") {
		t.Fatalf("missing context/limit view lacks explanation:\n%s", content)
	}
	if strings.Contains(content, "0%") {
		t.Fatalf("missing-context view fabricated a zero-percent context:\n%s", content)
	}
}

func TestContextUsageLineReportsOverage(t *testing.T) {
	window := &usage.ContextWindow{Used: 105_000, Limit: 100_000}
	if got := contextUsageLine(window); got != "105k used  •  5.0k over  •  100k limit" {
		t.Fatalf("contextUsageLine() = %q", got)
	}
	if got := displayPercent(window.Percent()); got != "105%" {
		t.Fatalf("displayPercent() = %q, want 105%%", got)
	}
}

func TestPanelUsesRequestedOuterWidth(t *testing.T) {
	window := &usage.ContextWindow{Used: 75, Limit: 100}
	for _, width := range []int{40, 56, 60} {
		content := "CONTEXT\n" + renderContextBar(window, min(52, panelInnerWidth(width)))
		rendered := renderPanel(content, width)
		for lineNumber, line := range strings.Split(rendered, "\n") {
			if got := lipgloss.Width(line); got != width {
				t.Fatalf("width %d, line %d rendered at %d cells:\n%s", width, lineNumber, got, rendered)
			}
		}
		if got := lipgloss.Height(rendered); got != 4 {
			t.Fatalf("width %d panel height = %d, want 4", width, got)
		}
	}
}

func TestViewFitsMeasuredTerminalHeight(t *testing.T) {
	tokens, _ := usage.NewTokens(100, 200, 0, 50, 0)
	results := make([]app.Result, 8)
	for index := range results {
		snapshot := usage.Snapshot{
			Harness:    "codex",
			PaneID:     fmt.Sprintf("w1:p%d", index),
			SessionID:  fmt.Sprintf("session-%d", index),
			Model:      "a-model-name-that-needs-clipping",
			State:      "working",
			Tokens:     tokens,
			Context:    &usage.ContextWindow{Used: 75, Limit: 100},
			Confidence: usage.ConfidenceEstimated,
		}
		results[index] = app.Result{Snapshot: &snapshot}
	}
	for _, width := range []int{30, 50, 96} {
		const height = 30
		content := (model{width: width, height: height, selected: 5, results: results}).View().Content
		if got := lipgloss.Height(content); got > height {
			t.Fatalf("%dx%d view rendered %d rows:\n%s", width, height, got, content)
		}
		for lineNumber, line := range strings.Split(content, "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("%dx%d view line %d rendered %d cells:\n%s", width, height, lineNumber, got, content)
			}
		}
	}
}

func TestCompactCardMarksEstimatedMatches(t *testing.T) {
	tokens, _ := usage.NewTokens(100, 0, 0, 50, 0)
	snapshot := usage.Snapshot{
		Harness:    "opencode",
		PaneID:     "w1:p1",
		SessionID:  "session-1",
		State:      "idle",
		Tokens:     tokens,
		Confidence: usage.ConfidenceEstimated,
	}
	content := (model{width: 64, results: []app.Result{{Snapshot: &snapshot}}}).View().Content
	for _, want := range []string{"spent 150", "idle·≈", "ctx not reported", "limit not reported"} {
		if !strings.Contains(content, want) {
			t.Fatalf("compact estimated card lacks %q:\n%s", want, content)
		}
	}
}

func TestViewRendersCompactErrorCard(t *testing.T) {
	m := model{width: 60, results: []app.Result{{
		Target: collector.Target{PaneID: "w1:p2", Harness: "unknown"},
		Error:  "unsupported harness",
	}}}
	content := m.View().Content
	if !strings.Contains(content, "UNKNOWN") || !strings.Contains(content, "unsupported harness") {
		t.Fatalf("compact error card missing content:\n%s", content)
	}
}

func TestClip(t *testing.T) {
	if got := clip("abcdefgh", 5); got != "abcd…" {
		t.Fatalf("clip() = %q", got)
	}
	if got := clip("ab界cd", 5); got != "ab界…" {
		t.Fatalf("wide-character clip() = %q", got)
	}
	if got := lipgloss.Width(clip("e\u0301界xyz", 4)); got != 4 {
		t.Fatalf("combining-character clip width = %d, want 4", got)
	}
}

func TestViewKeepsSelectedSessionVisibleWithinTerminalHeight(t *testing.T) {
	results := make([]app.Result, 20)
	for index := range results {
		results[index] = app.Result{
			Target: collector.Target{PaneID: fmt.Sprintf("pane-%02d", index), Harness: "codex"},
			Error:  "usage unavailable",
		}
	}
	m := model{width: 110, height: 20, selected: 15, results: results}
	content := m.View().Content
	if !strings.Contains(content, "pane-15") || !strings.Contains(content, "sessions ") || !strings.Contains(content, "of 20") {
		t.Fatalf("height-bounded view does not keep the selected window visible:\n%s", content)
	}
	if strings.Contains(content, "pane-00") {
		t.Fatalf("height-bounded view rendered off-window rows:\n%s", content)
	}
}
