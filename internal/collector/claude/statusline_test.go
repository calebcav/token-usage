package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	basecollector "github.com/calebcav/token-usage/internal/collector"
)

func TestCaptureStatusLinePersistsNormalizedStateAndLoadsIt(t *testing.T) {
	stateDir := t.TempDir()
	sessionID := "../../private/session-id"
	reset5h := fixtureNow.Add(4 * time.Hour).Unix()
	reset7d := fixtureNow.Add(6 * 24 * time.Hour).Unix()
	resetSpend := fixtureNow.Add(20 * 24 * time.Hour).Unix()
	payload := fmt.Sprintf(`{
      "session_id": %q,
      "cwd": "/private/workspace",
      "transcript_path": "/private/transcript.jsonl",
      "model": {"id":"secret-model","display_name":"Secret Model"},
      "prompt": "super-secret-prompt",
      "context_window": {
        "context_window_size": 200000,
        "used_percentage": 99,
        "current_usage": {
          "input_tokens": 8500,
          "output_tokens": 1200,
          "cache_creation_input_tokens": 5000,
          "cache_read_input_tokens": 2000
        }
      },
      "rate_limits": {
        "five_hour": {"used_percentage":23.5,"resets_at":%d},
        "seven_day": {"used_percentage":41.2,"resets_at":%d},
        "spend_limit": {"used_percentage":162.8,"resets_at":%d}
      }
    }`, sessionID, reset5h, reset7d, resetSpend)

	var output bytes.Buffer
	err := CaptureStatusLineWithOptions(strings.NewReader(payload), &output,
		WithCaptureStateDir(stateDir),
		WithCaptureNow(func() time.Time { return fixtureNow }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "ctx 8% | 5h 24% | 7d 41% | spend 163%\n"; got != want {
		t.Fatalf("status line = %q, want %q", got, want)
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("state entries = %d, want 1", len(entries))
	}
	wantName := statusSessionKey(sessionID) + ".json"
	if entries[0].Name() != wantName || strings.Contains(entries[0].Name(), "session") || strings.Contains(entries[0].Name(), "..") {
		t.Fatalf("state filename = %q, want safe hash %q", entries[0].Name(), wantName)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("state permissions = %o, want 600", permissions)
	}
	encoded, err := os.ReadFile(filepath.Join(stateDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{sessionID, "/private/workspace", "/private/transcript.jsonl", "secret-model", "Secret Model", "super-secret-prompt", "cwd", "transcript_path", "model", "prompt"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("normalized state persisted forbidden content %q: %s", forbidden, encoded)
		}
	}

	collector := New(WithStatusDir(stateDir))
	contextWindow, quota := collector.loadStatusState(sessionID, fixtureNow)
	if contextWindow == nil || contextWindow.Used != 15500 || contextWindow.Limit != 200000 {
		t.Fatalf("loaded context = %#v, want 15500/200000", contextWindow)
	}
	if quota == nil || quota.Source != statusLineSource || quota.CollectedAt != fixtureNow || len(quota.Windows) != 3 {
		t.Fatalf("loaded quota = %#v", quota)
	}
}

func TestCaptureStatusLineDerivesContextFromPercentage(t *testing.T) {
	stateDir := t.TempDir()
	payload := `{
      "session_id":"percentage-session",
      "context_window":{"context_window_size":200000,"used_percentage":12.5,"current_usage":null}
    }`
	var output bytes.Buffer
	if err := CaptureStatusLineWithOptions(strings.NewReader(payload), &output,
		WithCaptureStateDir(stateDir),
		WithCaptureNow(func() time.Time { return fixtureNow }),
	); err != nil {
		t.Fatal(err)
	}
	contextWindow, quota := New(WithStatusDir(stateDir)).loadStatusState("percentage-session", fixtureNow)
	if contextWindow == nil || contextWindow.Used != 25000 || contextWindow.Limit != 200000 {
		t.Fatalf("derived context = %#v, want 25000/200000", contextWindow)
	}
	if quota != nil {
		t.Fatalf("quota = %#v, want nil", quota)
	}
}

func TestLoadStatusStateFiltersResetQuotaAndExpiresState(t *testing.T) {
	stateDir := t.TempDir()
	payload := fmt.Sprintf(`{
      "session_id":"reset-session",
      "context_window":{"context_window_size":100000,"current_usage":{"input_tokens":10000,"output_tokens":90000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}},
      "rate_limits":{
        "five_hour":{"used_percentage":90,"resets_at":%d},
        "seven_day":{"used_percentage":30,"resets_at":%d},
        "spend_limit":{"used_percentage":10,"resets_at":%d}
      }
    }`, fixtureNow.Add(-time.Second).Unix(), fixtureNow.Add(time.Hour).Unix(), fixtureNow.Unix())
	if err := CaptureStatusLineWithOptions(strings.NewReader(payload), &bytes.Buffer{},
		WithCaptureStateDir(stateDir),
		WithCaptureNow(func() time.Time { return fixtureNow.Add(-time.Minute) }),
	); err != nil {
		t.Fatal(err)
	}

	contextWindow, quota := New(WithStatusDir(stateDir)).loadStatusState("reset-session", fixtureNow)
	if contextWindow == nil || contextWindow.Used != 10000 {
		t.Fatalf("context = %#v, output tokens must not count toward occupancy", contextWindow)
	}
	if quota == nil || len(quota.Windows) != 1 || quota.Windows[0].Label != "7d" {
		t.Fatalf("active quota = %#v, want only 7d", quota)
	}
	contextWindow, quota = New(WithStatusDir(stateDir)).loadStatusState("reset-session", fixtureNow.Add(2*time.Hour))
	if contextWindow == nil || contextWindow.Used != 10000 {
		t.Fatalf("idle context = %#v, want retained context", contextWindow)
	}
	if quota != nil {
		t.Fatalf("expired quota = %#v, want nil", quota)
	}
}

func TestLoadStatusStateRejectsContextOlderThanTranscript(t *testing.T) {
	stateDir := t.TempDir()
	payload := fmt.Sprintf(`{
		"session_id":"advanced-session",
		"context_window":{"context_window_size":100,"used_percentage":10},
		"rate_limits":{"five_hour":{"used_percentage":25,"resets_at":%d}}
	}`, fixtureNow.Add(time.Hour).Unix())
	if err := CaptureStatusLineWithOptions(strings.NewReader(payload), &bytes.Buffer{},
		WithCaptureStateDir(stateDir),
		WithCaptureNow(func() time.Time { return fixtureNow }),
	); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statusStatePath(stateDir, "advanced-session"))
	if err != nil {
		t.Fatal(err)
	}
	contextWindow, quota := New(WithStatusDir(stateDir)).loadStatusState("advanced-session", fixtureNow, info.ModTime().Add(time.Second))
	if contextWindow != nil || quota == nil || quota.Windows[0].UsedPercent != 25 {
		t.Fatalf("state older than transcript = context %#v, quota %#v; want only fresh account quota", contextWindow, quota)
	}
}

func TestCaptureStatusLineRejectsMalformedAndUnsafeNumbers(t *testing.T) {
	tests := map[string]string{
		"malformed JSON":     `{"session_id":`,
		"trailing input":     `{"session_id":"session"} false`,
		"missing session":    `{"context_window":{"context_window_size":100}}`,
		"negative tokens":    `{"session_id":"session","context_window":{"context_window_size":100,"current_usage":{"input_tokens":-1}}}`,
		"token overflow":     `{"session_id":"session","context_window":{"context_window_size":18446744073709551615,"current_usage":{"input_tokens":18446744073709551615,"cache_read_input_tokens":1}}}`,
		"context over limit": `{"session_id":"session","context_window":{"context_window_size":100,"current_usage":{"input_tokens":101}}}`,
		"context percentage": `{"session_id":"session","context_window":{"context_window_size":100,"used_percentage":100.1}}`,
		"quota percentage":   `{"session_id":"session","rate_limits":{"five_hour":{"used_percentage":101}}}`,
		"spend percentage":   `{"session_id":"session","rate_limits":{"spend_limit":{"used_percentage":1000}}}`,
		"reset overflow":     `{"session_id":"session","rate_limits":{"five_hour":{"used_percentage":10,"resets_at":253402300800}}}`,
		"oversized":          `{"session_id":"session","padding":"` + strings.Repeat("x", maxStatusLineJSON) + `"}`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			err := CaptureStatusLineWithOptions(strings.NewReader(payload), &bytes.Buffer{}, WithCaptureStateDir(stateDir))
			if err == nil {
				t.Fatal("CaptureStatusLineWithOptions() error = nil")
			}
			entries, readErr := os.ReadDir(stateDir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("capture wrote %d state files for malformed input", len(entries))
			}
		})
	}
}

func TestCollectorEnrichesExactResolvedSessionAndIgnoresBadState(t *testing.T) {
	projectsDir, transcriptPath := makeTranscriptPath(t, "enriched-session")
	if err := os.WriteFile(transcriptPath, []byte(transcriptRecord("enriched-session", "message", 2, 3)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	payload := fmt.Sprintf(`{
      "session_id":"enriched-session",
      "context_window":{"context_window_size":200000,"current_usage":{"input_tokens":10000,"output_tokens":5000,"cache_creation_input_tokens":2000,"cache_read_input_tokens":3000}},
      "rate_limits":{"five_hour":{"used_percentage":45,"resets_at":%d},"seven_day":{"used_percentage":60,"resets_at":%d}}
    }`, fixtureNow.Add(-time.Second).Unix(), fixtureNow.Add(time.Hour).Unix())
	if err := CaptureStatusLineWithOptions(strings.NewReader(payload), &bytes.Buffer{},
		WithCaptureStateDir(stateDir),
		WithCaptureNow(func() time.Time { return fixtureNow.Add(-time.Minute) }),
	); err != nil {
		t.Fatal(err)
	}

	collector := New(
		WithProjectsDir(projectsDir),
		WithStatusDir(stateDir),
		WithNow(func() time.Time { return fixtureNow }),
	)
	snapshot, err := collector.Collect(context.Background(), basecollector.Target{SessionID: "enriched-session"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Context == nil || snapshot.Context.Used != 15000 || snapshot.Context.Limit != 200000 {
		t.Fatalf("snapshot context = %#v", snapshot.Context)
	}
	if snapshot.Quota == nil || snapshot.Quota.Source != statusLineSource || len(snapshot.Quota.Windows) != 1 || snapshot.Quota.Windows[0].Label != "7d" {
		t.Fatalf("snapshot quota = %#v", snapshot.Quota)
	}
	if snapshot.CollectedAt != fixtureNow {
		t.Fatalf("CollectedAt = %v, want %v", snapshot.CollectedAt, fixtureNow)
	}

	badSession := "bad-state-session"
	badProjectsDir, badTranscriptPath := makeTranscriptPath(t, badSession)
	if err := os.WriteFile(badTranscriptPath, []byte(transcriptRecord(badSession, "message", 4, 5)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusStatePath(stateDir, badSession), []byte(`{"broken"`), 0o600); err != nil {
		t.Fatal(err)
	}
	badCollector := New(WithProjectsDir(badProjectsDir), WithStatusDir(stateDir), WithNow(func() time.Time { return fixtureNow }))
	badSnapshot, err := badCollector.Collect(context.Background(), basecollector.Target{SessionID: badSession})
	if err != nil {
		t.Fatalf("malformed state failed token collection: %v", err)
	}
	if badSnapshot.Tokens.Total != 9 || badSnapshot.Context != nil || badSnapshot.Quota != nil {
		t.Fatalf("snapshot with malformed state = %#v", badSnapshot)
	}
}

func TestLoadStatusStateIgnoresMalformedQuotaWithoutDiscardingContext(t *testing.T) {
	stateDir := t.TempDir()
	sessionID := "malformed-quota"
	contextJSON, err := json.Marshal(struct {
		Used  uint64 `json:"used"`
		Limit uint64 `json:"limit"`
	}{Used: 10, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	state := statusState{
		Version:     statusStateVersion,
		SessionKey:  statusSessionKey(sessionID),
		CollectedAt: fixtureNow,
		Context:     contextJSON,
		Quota:       json.RawMessage(`{"windows":[{"label":"5h","used_percent":1000}],"source":"claude-statusline","collected_at":"2026-08-30T12:00:00Z"}`),
	}
	if err := writeStatusState(stateDir, state); err != nil {
		t.Fatal(err)
	}
	contextWindow, quota := New(WithStatusDir(stateDir)).loadStatusState(sessionID, fixtureNow)
	if contextWindow == nil || contextWindow.Used != 10 || contextWindow.Limit != 100 {
		t.Fatalf("context = %#v, want valid context", contextWindow)
	}
	if quota != nil {
		t.Fatalf("quota = %#v, want malformed quota ignored", quota)
	}
}
