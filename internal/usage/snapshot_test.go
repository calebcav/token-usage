package usage

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNewTokensUsesNonOverlappingTotal(t *testing.T) {
	tokens, err := NewTokens(10, 20, 30, 40, 12)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.Total != 100 {
		t.Fatalf("Total = %d, want 100", tokens.Total)
	}
	if tokens.Spent() != 50 {
		t.Fatalf("Spent() = %d, want 50", tokens.Spent())
	}
	if got := (Snapshot{Tokens: tokens}).CompactUsage(); got != "Σ 50" {
		t.Fatalf("CompactUsage() = %q, want %q", got, "Σ 50")
	}
}

func TestNewTokensRejectsReasoningAboveOutput(t *testing.T) {
	if _, err := NewTokens(0, 0, 0, 3, 4); err == nil {
		t.Fatal("expected reasoning validation error")
	}
}

func TestNewTokensRejectsOverflow(t *testing.T) {
	if _, err := NewTokens(math.MaxUint64, 1, 0, 0, 0); err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestSnapshotValidate(t *testing.T) {
	tokens, err := NewTokens(10, 20, 0, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		Harness:       "codex",
		SessionID:     "session-1",
		Tokens:        tokens,
		Source:        "codex-rollout",
		Confidence:    ConfidenceExact,
		CollectedAt:   time.Now(),
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotQuotaValidationAndCompactLimit(t *testing.T) {
	tokens, _ := NewTokens(10, 0, 0, 4, 0)
	reset := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		Harness:       "codex",
		SessionID:     "session-1",
		Tokens:        tokens,
		Quota: &QuotaSnapshot{
			Windows: []QuotaWindow{
				{Label: "5h", UsedPercent: 42, ResetsAt: &reset},
				{Label: "7d", UsedPercent: 73},
			},
			Source:      "codex-app-server",
			CollectedAt: time.Now(),
		},
		Source:      "codex-rollout",
		Confidence:  ConfidenceExact,
		CollectedAt: time.Now(),
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := snapshot.CompactLimit(); got != "7d 73%" {
		t.Fatalf("CompactLimit() = %q, want %q", got, "7d 73%")
	}
	snapshot.Quota.Windows[1].Label = "5h"
	if err := snapshot.Validate(); err == nil {
		t.Fatal("Validate() accepted duplicate quota labels")
	}
}

func TestSnapshotRejectsInvalidQuotaProvenanceAndReset(t *testing.T) {
	tokens, _ := NewTokens(1, 0, 0, 1, 0)
	invalidReset := time.Date(10_000, time.January, 1, 0, 0, 0, 0, time.UTC)
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		Harness:       "codex",
		SessionID:     "session-1",
		Tokens:        tokens,
		Quota: &QuotaSnapshot{
			Windows:     []QuotaWindow{{Label: "5h", UsedPercent: 42}},
			CollectedAt: time.Now(),
		},
		Source:      "codex-rollout",
		Confidence:  ConfidenceExact,
		CollectedAt: time.Now(),
	}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("Validate() accepted an empty quota source")
	}
	snapshot.Quota.Source = "codex-app-server"
	snapshot.Quota.Windows[0].ResetsAt = &invalidReset
	if err := snapshot.Validate(); err == nil {
		t.Fatal("Validate() accepted a non-JSON reset timestamp")
	}
}

func TestSnapshotRejectsSourcePath(t *testing.T) {
	tokens, err := NewTokens(1, 0, 0, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		Harness:       "codex",
		SessionID:     "session",
		Tokens:        tokens,
		Source:        "/Users/someone/private/transcript.jsonl",
		Confidence:    ConfidenceExact,
		CollectedAt:   time.Now(),
	}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want unsafe source rejection")
	}
}

func TestSnapshotRejectsUnsafeDisplayText(t *testing.T) {
	tokens, _ := NewTokens(1, 0, 0, 1, 0)
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		Harness:       "codex",
		SessionID:     "session",
		Model:         "safe\x1b[2Junsafe",
		Tokens:        tokens,
		Source:        "codex-rollout",
		Confidence:    ConfidenceExact,
		CollectedAt:   time.Now(),
	}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want control-character rejection")
	}
	if got := SanitizeText(" bad\x1b[31m\nvalue ", 10); strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\n') || utf8.RuneCountInString(got) > 10 {
		t.Fatalf("SanitizeText() = %q", got)
	}
}

func TestFormatCount(t *testing.T) {
	tests := map[uint64]string{
		999:       "999",
		1_000:     "1.0k",
		12_345:    "12.3k",
		123_456:   "123k",
		1_250_000: "1.2m",
	}
	for input, want := range tests {
		if got := FormatCount(input); got != want {
			t.Errorf("FormatCount(%d) = %q, want %q", input, got, want)
		}
	}
}
