package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	basecollector "github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

var fixtureNow = time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)

func TestCollectExactSessionDeduplicatesAndNormalizesUsage(t *testing.T) {
	configDir := filepath.Join("testdata", "config")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	collector := newTestCollector(t, WithNow(func() time.Time { return fixtureNow }))
	snapshot, err := collector.Collect(context.Background(), basecollector.Target{
		PaneID:      "pane-1",
		WorkspaceID: "workspace-1",
		Harness:     "claude-code",
		SessionID:   "session-main",
		// An exact session ID must win even when CWD points at other sessions.
		CWD:   "/workspace/ambiguous",
		State: "idle",
	})
	if err != nil {
		t.Fatal(err)
	}

	if snapshot.SessionID != "session-main" {
		t.Fatalf("SessionID = %q, want session-main", snapshot.SessionID)
	}
	if snapshot.Confidence != usage.ConfidenceExact {
		t.Fatalf("Confidence = %q, want exact", snapshot.Confidence)
	}
	if snapshot.Source != sourceName {
		t.Fatalf("Source = %q, want %q", snapshot.Source, sourceName)
	}
	if strings.Contains(snapshot.Source, "project-alpha") || strings.Contains(snapshot.Source, configDir) {
		t.Fatalf("Source leaks a transcript path: %q", snapshot.Source)
	}
	if snapshot.Model != "claude-sonnet-4-5-20250929" {
		t.Fatalf("Model = %q", snapshot.Model)
	}
	if snapshot.HarnessVersion != "2.1.1" {
		t.Fatalf("HarnessVersion = %q", snapshot.HarnessVersion)
	}
	if snapshot.Provider != "" {
		t.Fatalf("Provider = %q, want unknown without transcript evidence", snapshot.Provider)
	}
	if snapshot.Context != nil {
		t.Fatalf("Context = %#v, want nil without an exact limit", snapshot.Context)
	}

	want := usage.Tokens{
		FreshInput: 150,
		CacheRead:  300,
		CacheWrite: 32,
		Output:     20,
		Reasoning:  5,
		Total:      502,
	}
	if snapshot.Tokens != want {
		t.Fatalf("Tokens = %#v, want %#v", snapshot.Tokens, want)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("snapshot validation: %v", err)
	}
	if snapshot.CollectedAt != fixtureNow {
		t.Fatalf("CollectedAt = %v, want %v", snapshot.CollectedAt, fixtureNow)
	}
}

func TestCollectIgnoresSyntheticModel(t *testing.T) {
	const synthetic = `{"type":"assistant","sessionId":"session-synthetic","timestamp":"2026-08-30T12:00:00Z","isApiErrorMessage":true,"error":"server_error","message":{"id":"error-1","role":"assistant","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	real := transcriptRecord("session-synthetic", "message-1", 2, 3)
	recovered := strings.ReplaceAll(transcriptRecord("session-synthetic", "message-2", 4, 5), "claude-test", "claude-new")
	for _, test := range []struct {
		name      string
		initial   string
		appended  string
		wantModel string
		wantTotal uint64
	}{
		{name: "initial parse", initial: real + "\n" + synthetic, wantModel: "claude-test", wantTotal: 5},
		{name: "appended error", initial: real, appended: synthetic, wantModel: "claude-test", wantTotal: 5},
		{name: "only synthetic", initial: synthetic},
		{name: "model change after error", initial: real + "\n" + synthetic, appended: recovered, wantModel: "claude-new", wantTotal: 14},
	} {
		t.Run(test.name, func(t *testing.T) {
			projectsDir, path := makeTranscriptPath(t, "session-synthetic")
			if err := os.WriteFile(path, []byte(test.initial+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			collector := newTestCollector(t, WithProjectsDir(projectsDir))
			target := basecollector.Target{Harness: "claude", SessionID: "session-synthetic"}
			if test.appended != "" {
				if _, err := collector.Collect(context.Background(), target); err != nil {
					t.Fatal(err)
				}
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteString(test.appended + "\n")
				closeErr := file.Close()
				if err := errors.Join(writeErr, closeErr); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := collector.Collect(context.Background(), target)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Model != test.wantModel {
				t.Errorf("Model = %q, want %q", snapshot.Model, test.wantModel)
			}
			if snapshot.Tokens.Total != test.wantTotal {
				t.Errorf("Total = %d, want %d", snapshot.Tokens.Total, test.wantTotal)
			}
		})
	}
}

func TestCollectToleratesMissingCacheAndReasoningFields(t *testing.T) {
	collector := newTestCollector(t,
		WithConfigDir(filepath.Join("testdata", "config")),
		WithNow(func() time.Time { return fixtureNow }),
	)
	snapshot, err := collector.Collect(context.Background(), basecollector.Target{
		SessionID: "session-missing-fields",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := usage.Tokens{FreshInput: 11, Output: 7, Total: 18}
	if snapshot.Tokens != want {
		t.Fatalf("Tokens = %#v, want %#v", snapshot.Tokens, want)
	}
}

func TestCollectRejectsAmbiguousExactSessionWithoutLeakingPaths(t *testing.T) {
	projectsDir := filepath.Join("testdata", "config", "projects")
	collector := newTestCollector(t, WithProjectsDir(projectsDir))
	_, err := collector.Collect(context.Background(), basecollector.Target{SessionID: "duplicate-session"})
	if !errors.Is(err, basecollector.ErrAmbiguous) {
		t.Fatalf("error = %v, want ErrAmbiguous", err)
	}
	if strings.Contains(err.Error(), projectsDir) || strings.Contains(err.Error(), "duplicate-a") {
		t.Fatalf("error leaks a transcript path: %v", err)
	}
}

func TestCollectFallsBackToOnlyRecentTranscriptForCWD(t *testing.T) {
	collector := newTestCollector(t,
		WithProjectsDir(filepath.Join("testdata", "config", "projects")),
		WithNow(func() time.Time { return fixtureNow }),
		WithFallbackWindow(2*time.Hour),
	)
	snapshot, err := collector.Collect(context.Background(), basecollector.Target{
		CWD: "/workspace/fallback",
	})
	if err != nil {
		t.Fatal(err)
	}

	if snapshot.SessionID != "fallback-recent" {
		t.Fatalf("SessionID = %q, want fallback-recent", snapshot.SessionID)
	}
	if snapshot.Confidence != usage.ConfidenceEstimated {
		t.Fatalf("Confidence = %q, want estimated", snapshot.Confidence)
	}
	if snapshot.Tokens.Total != 60 {
		t.Fatalf("Total = %d, want 60", snapshot.Tokens.Total)
	}
}

func TestCollectCWDFallbackIgnoresNestedSubagentTranscripts(t *testing.T) {
	projectsDir, rootPath := makeTranscriptPath(t, "root-session")
	rootRecord := transcriptRecordWithCWD("root-session", "root-message", "/workspace/with-subagent", fixtureNow.Add(-time.Minute), 2, 3)
	if err := os.WriteFile(rootPath, []byte(rootRecord+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subagentsDir := filepath.Join(filepath.Dir(rootPath), "root-session", "subagents")
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subagentRecord := transcriptRecordWithCWD("child-session", "child-message", "/workspace/with-subagent", fixtureNow, 20, 30)
	if err := os.WriteFile(filepath.Join(subagentsDir, "agent-child.jsonl"), []byte(subagentRecord+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	collector := newTestCollector(t,
		WithProjectsDir(projectsDir),
		WithNow(func() time.Time { return fixtureNow }),
	)
	snapshot, err := collector.Collect(context.Background(), basecollector.Target{CWD: "/workspace/with-subagent"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SessionID != "root-session" || snapshot.Tokens.Total != 5 || snapshot.Confidence != usage.ConfidenceEstimated {
		t.Fatalf("fallback snapshot = %+v, want root session only", snapshot)
	}
}

func TestCollectRejectsAmbiguousCWDFallback(t *testing.T) {
	collector := newTestCollector(t,
		WithProjectsDir(filepath.Join("testdata", "config", "projects")),
		WithNow(func() time.Time { return fixtureNow }),
	)
	_, err := collector.Collect(context.Background(), basecollector.Target{
		CWD: "/workspace/ambiguous",
	})
	if !errors.Is(err, basecollector.ErrAmbiguous) {
		t.Fatalf("error = %v, want ErrAmbiguous", err)
	}
}

func TestCollectNeverFallsBackWhenSessionIDWasProvided(t *testing.T) {
	collector := newTestCollector(t,
		WithProjectsDir(filepath.Join("testdata", "config", "projects")),
		WithNow(func() time.Time { return fixtureNow }),
	)
	_, err := collector.Collect(context.Background(), basecollector.Target{
		SessionID: "does-not-exist",
		CWD:       "/workspace/fallback",
	})
	if !errors.Is(err, basecollector.ErrSessionNotFound) {
		t.Fatalf("error = %v, want ErrSessionNotFound", err)
	}
}

func TestCollectRequiresAbsoluteCWDForFallback(t *testing.T) {
	collector := newTestCollector(t, WithProjectsDir(filepath.Join("testdata", "config", "projects")))
	_, err := collector.Collect(context.Background(), basecollector.Target{CWD: "workspace/fallback"})
	if !errors.Is(err, basecollector.ErrSessionNotFound) {
		t.Fatalf("error = %v, want ErrSessionNotFound", err)
	}
}

func TestCollectHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	collector := newTestCollector(t, WithProjectsDir(filepath.Join("testdata", "config", "projects")))
	_, err := collector.Collect(ctx, basecollector.Target{SessionID: "session-main"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestHarnesses(t *testing.T) {
	collector := New()
	harnesses := collector.Harnesses()
	if len(harnesses) != 1 || harnesses[0] != "claude" {
		t.Fatalf("Harnesses = %v", harnesses)
	}
}

func TestDecodeUsageSumsFallbackIterationsWithoutDoubleCountingTopLevel(t *testing.T) {
	data := json.RawMessage(`{
      "input_tokens":4,
      "cache_read_input_tokens":20,
      "cache_creation_input_tokens":5,
      "output_tokens":6,
      "iterations":[
        {"type":"message","input_tokens":2,"cache_read_input_tokens":10,"cache_creation_input_tokens":3,"output_tokens":1},
        {"type":"fallback_message","input_tokens":4,"cache_read_input_tokens":20,"cache_creation_input_tokens":5,"output_tokens":6,"reasoning_tokens":2}
      ]
    }`)
	got, err := decodeUsage(data)
	if err != nil {
		t.Fatal(err)
	}
	want := messageUsage{freshInput: 6, cacheRead: 30, cacheWrite: 8, output: 7, reasoning: 2}
	if got != want {
		t.Fatalf("decodeUsage() = %+v, want %+v", got, want)
	}
}

func TestReadLinesRejectsMalformedTerminatedTail(t *testing.T) {
	input := "{\"ok\":true}\n{malformed}\n"
	err := readLines(context.Background(), strings.NewReader(input), func(line []byte, _ int) error {
		var value map[string]any
		if json.Unmarshal(line, &value) != nil {
			return errMalformedLine
		}
		return nil
	})
	if !errors.Is(err, errMalformedLine) {
		t.Fatalf("readLines() error = %v, want errMalformedLine", err)
	}
}

func TestReadLinesToleratesOnlyUnterminatedPartialTail(t *testing.T) {
	input := "{\"ok\":true}\n{\"partial\""
	err := readLines(context.Background(), strings.NewReader(input), func(line []byte, _ int) error {
		var value map[string]any
		if json.Unmarshal(line, &value) != nil {
			return errMalformedLine
		}
		return nil
	})
	if err != nil {
		t.Fatalf("readLines() error = %v", err)
	}
}

func TestReadLinesCapsRecordSize(t *testing.T) {
	input := strings.Repeat("x", maxJSONLine+1)
	err := readLines(context.Background(), strings.NewReader(input), func([]byte, int) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatalf("readLines() error = %v, want bounded-record error", err)
	}
}

func TestCollectIncrementallyParsesAppendedTranscriptRecords(t *testing.T) {
	projectsDir := t.TempDir()
	projectDir := filepath.Join(projectsDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projectDir, "session-cache.jsonl")
	initial := `{"type":"assistant","sessionId":"session-cache","uuid":"record-1","message":{"id":"message-1","role":"assistant","model":"claude-test","usage":{"input_tokens":2,"output_tokens":3}}}` + "\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	collector := newTestCollector(t,
		WithProjectsDir(projectsDir),
		WithNow(func() time.Time { return fixtureNow }),
	)
	target := basecollector.Target{Harness: "claude", SessionID: "session-cache"}
	first, err := collector.Collect(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if first.Tokens.Total != 5 {
		t.Fatalf("first total = %d, want 5", first.Tokens.Total)
	}
	cachedBefore, ok := collector.cachedTranscript(path)
	if !ok || cachedBefore.offset != int64(len(initial)) || cachedBefore.records != 1 {
		t.Fatalf("initial cache = %#v, want one complete record at offset %d", cachedBefore, len(initial))
	}

	appended := `{"type":"assistant","sessionId":"session-cache","uuid":"record-2","message":{"id":"message-2","role":"assistant","model":"claude-test","usage":{"input_tokens":4,"cache_read_input_tokens":5,"output_tokens":6,"reasoning_tokens":1}}}` + "\n"
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(appended); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := collector.Collect(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if second.Tokens.Total != 20 {
		t.Fatalf("second total = %d, want 20", second.Tokens.Total)
	}
	cachedAfter, ok := collector.cachedTranscript(path)
	if !ok || cachedAfter.offset != int64(len(initial)+len(appended)) || cachedAfter.records != 2 {
		t.Fatalf("appended cache = %#v, want two complete records at offset %d", cachedAfter, len(initial)+len(appended))
	}

	duplicate := `{"type":"assistant","sessionId":"session-cache","uuid":"record-3","message":{"id":"message-2","role":"assistant","model":"claude-test","usage":{"input_tokens":6,"cache_read_input_tokens":3,"output_tokens":8,"reasoning_tokens":2}}}` + "\n"
	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(duplicate); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := collector.Collect(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	// The repeated message ID is one logical response. Per-field maxima merge
	// its cumulative updates, so the second message contributes 6+5+8, not
	// both records and not the lower cache counter from the newer record.
	if third.Tokens.Total != 24 {
		t.Fatalf("third total = %d, want 24", third.Tokens.Total)
	}
}

func TestCollectCheckpointsCompletePrefixBeforePartialTail(t *testing.T) {
	projectsDir, path := makeTranscriptPath(t, "session-partial")
	firstRecord := transcriptRecord("session-partial", "message-1", 2, 3)
	secondRecord := transcriptRecord("session-partial", "message-2", 4, 5)
	cut := len(secondRecord) / 2
	if err := os.WriteFile(path, []byte(firstRecord+"\n"+secondRecord[:cut]), 0o600); err != nil {
		t.Fatal(err)
	}

	collector := newTestCollector(t, WithProjectsDir(projectsDir))
	target := basecollector.Target{Harness: "claude", SessionID: "session-partial"}
	first, err := collector.Collect(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if first.Tokens.Total != 5 {
		t.Fatalf("first total = %d, want 5", first.Tokens.Total)
	}
	cached, ok := collector.cachedTranscript(path)
	if !ok || cached.offset != int64(len(firstRecord)+1) || cached.records != 1 {
		t.Fatalf("partial-tail cache = %#v, want complete prefix checkpoint", cached)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(secondRecord[cut:] + "\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := collector.Collect(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if second.Tokens.Total != 14 {
		t.Fatalf("second total = %d, want 14", second.Tokens.Total)
	}
}

func TestCollectInvalidatesCacheWhenTranscriptChangesGeneration(t *testing.T) {
	largePadding := strings.Repeat(" ", 5<<10) + "\n"
	tests := []struct {
		name    string
		initial string
		rewrite string
		replace bool
		want    uint64
	}{
		{
			name:    "shorter truncation",
			initial: transcriptRecord("session-generation", "message-1", 2, 3) + "\n" + transcriptRecord("session-generation", "message-2", 4, 5) + "\n",
			rewrite: transcriptRecord("session-generation", "message-3", 7, 1) + "\n",
			want:    8,
		},
		{
			name:    "same-size atomic replacement",
			initial: transcriptRecord("session-generation", "message-1", 2, 3) + "\n",
			rewrite: transcriptRecord("session-generation", "message-2", 8, 3) + "\n",
			replace: true,
			want:    11,
		},
		{
			name:    "larger truncate and regrow",
			initial: transcriptRecord("session-generation", "message-1", 2, 3) + "\n",
			rewrite: transcriptRecord("session-generation", "message-2", 8, 3) + "\n" + transcriptRecord("session-generation", "message-3", 4, 5) + "\n",
			want:    20,
		},
		{
			name:    "larger rewrite changes only middle of cached prefix",
			initial: largePadding + transcriptRecord("session-generation", "message-1", 2, 3) + "\n" + largePadding,
			rewrite: largePadding + transcriptRecord("session-generation", "message-1", 8, 3) + "\n" + largePadding + transcriptRecord("session-generation", "message-2", 1, 1) + "\n",
			want:    13,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projectsDir, path := makeTranscriptPath(t, "session-generation")
			if err := os.WriteFile(path, []byte(test.initial), 0o600); err != nil {
				t.Fatal(err)
			}
			collector := newTestCollector(t, WithProjectsDir(projectsDir))
			target := basecollector.Target{Harness: "claude", SessionID: "session-generation"}
			if _, err := collector.Collect(context.Background(), target); err != nil {
				t.Fatal(err)
			}

			if test.replace {
				replacement := path + ".replacement"
				if len(test.initial) != len(test.rewrite) {
					t.Fatalf("replacement fixture lengths differ: %d and %d", len(test.initial), len(test.rewrite))
				}
				if err := os.WriteFile(replacement, []byte(test.rewrite), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(test.rewrite), 0o600); err != nil {
				t.Fatal(err)
			}

			snapshot, err := collector.Collect(context.Background(), target)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Tokens.Total != test.want {
				t.Fatalf("total after rewrite = %d, want %d", snapshot.Tokens.Total, test.want)
			}
		})
	}
}

func TestCollectSerializesConcurrentReadsOfGrowingTranscript(t *testing.T) {
	projectsDir, path := makeTranscriptPath(t, "session-concurrent")
	initial := transcriptRecord("session-concurrent", "message-0", 1, 1) + "\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := newTestCollector(t, WithProjectsDir(projectsDir))
	target := basecollector.Target{Harness: "claude", SessionID: "session-concurrent"}
	if _, err := collector.Collect(context.Background(), target); err != nil {
		t.Fatal(err)
	}

	const appendedRecords = 12
	start := make(chan struct{})
	errorsSeen := make(chan error, 8)
	var workers sync.WaitGroup
	workers.Add(5)
	go func() {
		defer workers.Done()
		<-start
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			errorsSeen <- err
			return
		}
		defer file.Close()
		for index := 1; index <= appendedRecords; index++ {
			record := transcriptRecord("session-concurrent", fmt.Sprintf("message-%d", index), 1, 1)
			cut := len(record) / 2
			if _, err := file.WriteString(record[:cut]); err != nil {
				errorsSeen <- err
				return
			}
			time.Sleep(time.Millisecond)
			if _, err := file.WriteString(record[cut:] + "\n"); err != nil {
				errorsSeen <- err
				return
			}
		}
	}()
	for range 4 {
		go func() {
			defer workers.Done()
			<-start
			for range appendedRecords {
				if _, err := collector.Collect(context.Background(), target); err != nil {
					errorsSeen <- err
					return
				}
			}
		}()
	}
	close(start)
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent collection: %v", err)
	}
	if t.Failed() {
		return
	}
	final, err := collector.Collect(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	want := uint64((appendedRecords + 1) * 2)
	if final.Tokens.Total != want {
		t.Fatalf("final total = %d, want %d", final.Tokens.Total, want)
	}
}

func makeTranscriptPath(t *testing.T, sessionID string) (projectsDir, path string) {
	t.Helper()
	projectsDir = t.TempDir()
	projectDir := filepath.Join(projectsDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return projectsDir, filepath.Join(projectDir, sessionID+".jsonl")
}

func newTestCollector(t *testing.T, options ...Option) *Collector {
	t.Helper()
	return New(append(options, WithStatusDir(t.TempDir()))...)
}

func transcriptRecord(sessionID, messageID string, input, output uint64) string {
	return fmt.Sprintf(`{"type":"assistant","sessionId":%q,"uuid":%q,"message":{"id":%q,"role":"assistant","model":"claude-test","usage":{"input_tokens":%d,"output_tokens":%d}}}`,
		sessionID, messageID, messageID, input, output)
}

func transcriptRecordWithCWD(sessionID, messageID, cwd string, timestamp time.Time, input, output uint64) string {
	return fmt.Sprintf(`{"type":"assistant","sessionId":%q,"uuid":%q,"cwd":%q,"timestamp":%q,"message":{"id":%q,"role":"assistant","model":"claude-test","usage":{"input_tokens":%d,"output_tokens":%d}}}`,
		sessionID, messageID, cwd, timestamp.Format(time.RFC3339Nano), messageID, input, output)
}
