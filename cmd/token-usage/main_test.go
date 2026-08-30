package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/calebcav/token-usage/internal/app"
	"github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

func TestParseEvent(t *testing.T) {
	paneID, status := parseEvent(`{"event":"pane.agent_status_changed","data":{"type":"pane_agent_status_changed","pane_id":"w1:p3","agent_status":"idle"}}`)
	if paneID != "w1:p3" || status != "idle" {
		t.Fatalf("parseEvent() = %q, %q", paneID, status)
	}
}

func TestPrintStatusIncludesUnavailableRows(t *testing.T) {
	tokens, _ := usage.NewTokens(100, 20, 0, 30, 5)
	results := []app.Result{
		{Target: collector.Target{Harness: "codex", PaneID: "w1:p1"}, Snapshot: &usage.Snapshot{
			Harness: "codex", PaneID: "w1:p1", Model: "gpt-5", Tokens: tokens, Confidence: usage.ConfidenceExact,
			Quota: &usage.QuotaSnapshot{Windows: []usage.QuotaWindow{{Label: "5h", UsedPercent: 42}}},
		}},
		{Target: collector.Target{Harness: "aider", PaneID: "w1:p2"}, Error: "unsupported harness"},
	}
	var output bytes.Buffer
	printStatus(&output, results)
	for _, want := range []string{"HARNESS", "SPENT", "LIMIT", "codex", "gpt-5", "130", "5h 42%", "aider", "unsupported harness"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("status output missing %q:\n%s", want, output.String())
		}
	}
}

func TestPrintStatusSanitizesUnavailableRows(t *testing.T) {
	results := []app.Result{{
		Target: collector.Target{Harness: "aider\x1b[2J", PaneID: "w1:p2\nforged", State: "idle\rworking"},
		Error:  "failed\x1b[31m\nforged",
	}}
	var output bytes.Buffer
	printStatus(&output, results)
	if strings.ContainsAny(output.String(), "\x1b\r") || strings.Contains(output.String(), "\nforged") {
		t.Fatalf("status output contains terminal controls or a forged line:\n%q", output.String())
	}
}

func TestSetupMentionsSidebarAndContract(t *testing.T) {
	var output bytes.Buffer
	printSetup(&output, "/tmp/token-usage/collectors.json", "/tmp/Token Usage/token-usage")
	if !strings.Contains(output.String(), "$usage") || !strings.Contains(output.String(), "$limit") || !strings.Contains(output.String(), "claude-statusline") || !strings.Contains(output.String(), "collectors.json") {
		t.Fatalf("setup output is incomplete:\n%s", output.String())
	}
	if !strings.Contains(output.String(), `"command": "'/tmp/Token Usage/token-usage' claude-statusline"`) {
		t.Fatalf("setup output does not safely quote the executable:\n%s", output.String())
	}
}
