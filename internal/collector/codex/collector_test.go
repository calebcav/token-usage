package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	basecollector "github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const (
	primarySession = "11111111-1111-4111-8111-111111111111"
	minimalSession = "22222222-2222-4222-8222-222222222222"
)

var collectedAt = time.Date(2026, time.August, 30, 12, 30, 0, 0, time.UTC)

func TestCollectorImplementsContract(t *testing.T) {
	var _ basecollector.Collector = New(Config{Home: fixtureHome(t)})
}

func TestCollectUsesFinalCumulativeEvent(t *testing.T) {
	item := New(Config{Home: fixtureHome(t), Now: func() time.Time { return collectedAt }})
	snapshot, err := item.Collect(context.Background(), basecollector.Target{
		PaneID:      "pane-fixture",
		WorkspaceID: "workspace-fixture",
		Harness:     "codex-cli",
		SessionID:   primarySession,
		CWD:         "/a/different/path",
		State:       "idle",
	})
	if err != nil {
		t.Fatal(err)
	}

	if snapshot.Confidence != usage.ConfidenceExact {
		t.Fatalf("Confidence = %q, want exact", snapshot.Confidence)
	}
	if snapshot.SessionID != primarySession {
		t.Fatalf("SessionID = %q, want fixture ID", snapshot.SessionID)
	}
	if snapshot.HarnessVersion != "0.fixture.1" || snapshot.Model != "gpt-fixture" || snapshot.Provider != "openai" {
		t.Fatalf("metadata = version %q, model %q, provider %q", snapshot.HarnessVersion, snapshot.Model, snapshot.Provider)
	}
	wantTokens := usage.Tokens{
		FreshInput: 150,
		CacheRead:  80,
		CacheWrite: 20,
		Output:     50,
		Reasoning:  15,
		Total:      300,
	}
	if snapshot.Tokens != wantTokens {
		t.Fatalf("Tokens = %#v, want %#v", snapshot.Tokens, wantTokens)
	}
	if snapshot.Context == nil || *snapshot.Context != (usage.ContextWindow{Used: 150, Limit: 400}) {
		t.Fatalf("Context = %#v, want 150/400", snapshot.Context)
	}
	if snapshot.Source != "codex-rollout" {
		t.Fatalf("Source = %q, want safe source kind", snapshot.Source)
	}
	if !snapshot.CollectedAt.Equal(collectedAt) {
		t.Fatalf("CollectedAt = %v, want %v", snapshot.CollectedAt, collectedAt)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("snapshot validation: %v", err)
	}
}

func TestCollectToleratesMissingNewerFields(t *testing.T) {
	item := New(Config{Home: fixtureHome(t), Now: func() time.Time { return collectedAt }})
	snapshot, err := item.Collect(context.Background(), basecollector.Target{
		Harness:   "codex",
		SessionID: minimalSession,
	})
	if err != nil {
		t.Fatal(err)
	}

	want := usage.Tokens{FreshInput: 7, Output: 3, Total: 10}
	if snapshot.Tokens != want {
		t.Fatalf("Tokens = %#v, want %#v", snapshot.Tokens, want)
	}
	if snapshot.Context == nil || *snapshot.Context != (usage.ContextWindow{Used: 6, Limit: 100}) {
		t.Fatalf("Context = %#v, want derived 6/100", snapshot.Context)
	}
	if snapshot.Model != "" || snapshot.Provider != "" {
		t.Fatalf("optional metadata = model %q, provider %q; want empty", snapshot.Model, snapshot.Provider)
	}
}

func TestExactSessionIDDoesNotFallBackToCWD(t *testing.T) {
	item := New(Config{
		Home:                    fixtureHome(t),
		AllowCWDRecencyFallback: true,
		Now:                     func() time.Time { return collectedAt },
	})
	_, err := item.Collect(context.Background(), basecollector.Target{
		Harness:   "codex",
		SessionID: "99999999-9999-4999-8999-999999999999",
		CWD:       "/fixtures/project",
	})
	if !errors.Is(err, basecollector.ErrSessionNotFound) {
		t.Fatalf("error = %v, want ErrSessionNotFound", err)
	}
}

func TestCWDRecencyFallbackRequiresOptIn(t *testing.T) {
	item := New(Config{Home: fixtureHome(t), Now: func() time.Time { return collectedAt }})
	_, err := item.Collect(context.Background(), basecollector.Target{
		Harness: "codex",
		CWD:     "/fixtures/project",
	})
	if !errors.Is(err, basecollector.ErrSessionNotFound) {
		t.Fatalf("error = %v, want ErrSessionNotFound", err)
	}
}

func TestCWDRecencyFallbackIsEstimatedWhenUnambiguous(t *testing.T) {
	item := New(Config{
		Home:                    fixtureHome(t),
		AllowCWDRecencyFallback: true,
		RecencyWindow:           24 * time.Hour,
		Now:                     time.Now,
	})
	snapshot, err := item.Collect(context.Background(), basecollector.Target{
		Harness: "codex",
		CWD:     "/fixtures/project/.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Confidence != usage.ConfidenceEstimated || snapshot.SessionID != primarySession {
		t.Fatalf("fallback = confidence %q, session %q", snapshot.Confidence, snapshot.SessionID)
	}
}

func TestCWDRecencyFallbackRejectsAmbiguity(t *testing.T) {
	home := t.TempDir()
	writeMetadataOnlyRollout(t, home, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "/fixtures/shared", collectedAt.Add(-10*time.Minute))
	writeMetadataOnlyRollout(t, home, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "/fixtures/shared", collectedAt.Add(-20*time.Minute))

	item := New(Config{
		Home:                    home,
		AllowCWDRecencyFallback: true,
		RecencyWindow:           time.Hour,
		Now:                     func() time.Time { return collectedAt },
	})
	_, err := item.Collect(context.Background(), basecollector.Target{Harness: "codex", CWD: "/fixtures/shared"})
	if !errors.Is(err, basecollector.ErrAmbiguous) {
		t.Fatalf("error = %v, want ErrAmbiguous", err)
	}
}

func TestCWDRecencyFallbackIgnoresChildRolloutsButExactIDCanResolveThem(t *testing.T) {
	home := t.TempDir()
	rootID := "12121212-1212-4212-8212-121212121212"
	childByParentID := "34343434-3434-4434-8434-343434343434"
	childByForkID := "56565656-5656-4656-8656-565656565656"
	writeMetadataOnlyRollout(t, home, rootID, "/fixtures/delegated", collectedAt.Add(-10*time.Minute))
	writeChildMetadataOnlyRollout(t, home, childByParentID, "/fixtures/delegated", collectedAt.Add(-5*time.Minute), "parent_thread_id", rootID)
	writeChildMetadataOnlyRollout(t, home, childByForkID, "/fixtures/delegated", collectedAt.Add(-time.Minute), "forked_from_id", rootID)

	paths, err := listRollouts(context.Background(), filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	item := New(Config{
		Home:                    home,
		AllowCWDRecencyFallback: true,
		RecencyWindow:           time.Hour,
		Now:                     func() time.Time { return collectedAt },
	})
	resolved, confidence, err := item.resolve(context.Background(), paths, basecollector.Target{CWD: "/fixtures/delegated"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.metadata.ID != rootID || confidence != usage.ConfidenceEstimated {
		t.Fatalf("fallback resolved %q (%q), want root %q estimated", resolved.metadata.ID, confidence, rootID)
	}
	resolved, confidence, err = item.resolve(context.Background(), paths, basecollector.Target{SessionID: childByParentID})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.metadata.ID != childByParentID || confidence != usage.ConfidenceExact {
		t.Fatalf("exact resolved %q (%q), want child %q exact", resolved.metadata.ID, confidence, childByParentID)
	}
}

func TestExactResolutionRejectsDuplicateSessionID(t *testing.T) {
	home := t.TempDir()
	id := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	writeMetadataOnlyRolloutAt(t, home, "one", id, "/fixtures/one", collectedAt)
	writeMetadataOnlyRolloutAt(t, home, "two", id, "/fixtures/two", collectedAt)

	item := New(Config{Home: home, Now: func() time.Time { return collectedAt }})
	_, err := item.Collect(context.Background(), basecollector.Target{Harness: "codex", SessionID: id})
	if !errors.Is(err, basecollector.ErrAmbiguous) {
		t.Fatalf("error = %v, want ErrAmbiguous", err)
	}
}

func TestCWDRecencyFallbackRejectsStaleCandidate(t *testing.T) {
	home := t.TempDir()
	writeMetadataOnlyRollout(t, home, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "/fixtures/project", collectedAt.Add(-30*time.Minute))
	item := New(Config{
		Home:                    home,
		AllowCWDRecencyFallback: true,
		RecencyWindow:           15 * time.Minute,
		Now:                     func() time.Time { return collectedAt },
	})
	_, err := item.Collect(context.Background(), basecollector.Target{Harness: "codex", CWD: "/fixtures/project"})
	if !errors.Is(err, basecollector.ErrSessionNotFound) {
		t.Fatalf("error = %v, want ErrSessionNotFound", err)
	}
}

func TestCollectRejectsUnsupportedHarness(t *testing.T) {
	item := New(Config{Home: fixtureHome(t)})
	_, err := item.Collect(context.Background(), basecollector.Target{Harness: "claude", SessionID: primarySession})
	if !errors.Is(err, basecollector.ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestCollectHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	item := New(Config{Home: fixtureHome(t)})
	_, err := item.Collect(ctx, basecollector.Target{Harness: "codex", SessionID: primarySession})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestFilesystemErrorsDoNotExposeSessionPaths(t *testing.T) {
	home := t.TempDir()
	privatePath := filepath.Join(home, "private-project-name")
	if err := os.MkdirAll(privatePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privatePath, "sessions"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	item := New(Config{Home: privatePath})
	_, err := item.Collect(context.Background(), basecollector.Target{Harness: "codex", SessionID: primarySession})
	if err == nil {
		t.Fatal("Collect() error = nil")
	}
	if strings.Contains(err.Error(), privatePath) {
		t.Fatalf("Collect() leaked private path: %v", err)
	}

	_, err = readRollout(context.Background(), filepath.Join(privatePath, "secret-rollout.jsonl"))
	if err == nil || strings.Contains(err.Error(), privatePath) {
		t.Fatalf("readRollout() error = %v; path must remain private", err)
	}
}

func TestNormalizeTokenUsageRejectsMalformedCounters(t *testing.T) {
	number := func(value uint64) *uint64 { return &value }
	tests := map[string]rawTokenUsage{
		"cache read underflow": {
			Input: number(4), CacheRead: number(5),
		},
		"combined cache underflow": {
			Input: number(5), CacheRead: number(3), CacheWrite: number(3),
		},
		"reasoning is not an additive category": {
			Output: number(3), Reasoning: number(4),
		},
		"inconsistent reported total": {
			Input: number(4), Output: number(3), Total: number(8),
		},
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeTokenUsage(input); err == nil {
				t.Fatal("expected malformed counter error")
			}
		})
	}
}

func TestReadRolloutRejectsMalformedNumericInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-fixture.jsonl")
	contents := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":-1,"output_tokens":2}}}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readRollout(context.Background(), path)
	if !errors.Is(err, basecollector.ErrMalformed) {
		t.Fatalf("error = %v, want ErrMalformed", err)
	}
}

func TestReadRolloutSkipsIncompleteTokenEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-fixture.jsonl")
	contents := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":null}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := readRollout(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.tokens.Total != 10 {
		t.Fatalf("Total = %d, want earlier complete cumulative value 10", parsed.tokens.Total)
	}
}

func TestContextUsesExplicitTotalWhenLastBreakdownIsEstimated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-fixture.jsonl")
	contents := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10},"last_token_usage":{"input_tokens":0,"output_tokens":0,"total_tokens":77},"model_context_window":100}}}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := readRollout(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.context == nil || parsed.context.Used != 77 || parsed.context.Limit != 100 {
		t.Fatalf("Context = %#v, want 77/100", parsed.context)
	}
}

func TestReadRolloutIgnoresTruncatedTrailingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-fixture.jsonl")
	contents := strings.Join([]string{
		`{"type":"turn_context","payload":{"model":"gpt-fixture"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":12,"cached_input_tokens":2,"output_tokens":3,"total_tokens":15}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count"`,
	}, "\n")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := readRollout(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.tokens.Total != 15 || parsed.model != "gpt-fixture" {
		t.Fatalf("parsed = total %d, model %q; want 15 and gpt-fixture", parsed.tokens.Total, parsed.model)
	}
}

func TestCollectUsesCODEXHomeEnvironment(t *testing.T) {
	t.Setenv("CODEX_HOME", fixtureHome(t))
	item := New(Config{Now: func() time.Time { return collectedAt }})
	snapshot, err := item.Collect(context.Background(), basecollector.Target{
		Harness:   "codex",
		SessionID: "  " + minimalSession + "  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SessionID != minimalSession {
		t.Fatalf("SessionID = %q, want trimmed exact ID", snapshot.SessionID)
	}
}

func fixtureHome(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "home"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func writeMetadataOnlyRollout(t *testing.T, home, id, cwd string, started time.Time) {
	t.Helper()
	writeMetadataOnlyRolloutAt(t, home, id, id, cwd, started)
}

func writeMetadataOnlyRolloutAt(t *testing.T, home, name, id, cwd string, started time.Time) {
	t.Helper()
	directory := filepath.Join(home, "sessions", "2026", "08", "30", name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "rollout-fixture-"+name+".jsonl")
	line := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"timestamp":%q,"cwd":%q}}`, id, started.Format(time.RFC3339Nano), cwd)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, started, started); err != nil {
		t.Fatal(err)
	}
}

func writeChildMetadataOnlyRollout(t *testing.T, home, id, cwd string, started time.Time, parentField, parentID string) {
	t.Helper()
	directory := filepath.Join(home, "sessions", "2026", "08", "30", id)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "rollout-fixture-"+id+".jsonl")
	line := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"timestamp":%q,"cwd":%q,%q:%q}}`,
		id, started.Format(time.RFC3339Nano), cwd, parentField, parentID)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, started, started); err != nil {
		t.Fatal(err)
	}
}
