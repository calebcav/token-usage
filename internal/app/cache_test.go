package app

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

func TestFileSnapshotCacheRoundTripAndExpiry(t *testing.T) {
	now := time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)
	cache := &FileSnapshotCache{Directory: t.TempDir(), Now: func() time.Time { return now }}
	target := collector.Target{
		PaneID:      "w1:p1",
		WorkspaceID: "w1",
		Harness:     "codex",
		SessionID:   "session-1",
		CWD:         "/private/workspace-name",
		Confidence:  usage.ConfidenceExact,
	}
	snapshot := cachedTestSnapshot(now)

	if err := cache.Store(target, snapshot); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	loaded, ok := cache.Load(target, 15*time.Second)
	if !ok {
		t.Fatal("Load() missed fresh entry")
	}
	if loaded.SessionID != snapshot.SessionID || loaded.Tokens != snapshot.Tokens {
		t.Fatalf("Load() = %+v, want %+v", loaded, snapshot)
	}

	path, _, ok := cache.path(target)
	if !ok {
		t.Fatal("cache path is unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if permission := info.Mode().Perm(); permission != 0o600 {
		t.Fatalf("cache permissions = %o, want 600", permission)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), target.CWD) {
		t.Fatalf("cache persisted target cwd: %s", data)
	}

	now = now.Add(15 * time.Second)
	if _, ok := cache.Load(target, 15*time.Second); ok {
		t.Fatal("Load() returned entry at TTL boundary")
	}
}

func TestFileSnapshotCacheRejectsChangedSessionAndResetQuota(t *testing.T) {
	now := time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)
	cache := &FileSnapshotCache{Directory: t.TempDir(), Now: func() time.Time { return now }}
	target := collector.Target{PaneID: "w1:p1", WorkspaceID: "w1", Harness: "codex", SessionID: "session-1"}
	snapshot := cachedTestSnapshot(now)
	reset := now.Add(5 * time.Second)
	snapshot.Quota = &usage.QuotaSnapshot{
		Windows:     []usage.QuotaWindow{{Label: "5h", UsedPercent: 42, ResetsAt: &reset}},
		Source:      "fixture-quota",
		CollectedAt: now,
	}
	if err := cache.Store(target, snapshot); err != nil {
		t.Fatal(err)
	}

	changed := target
	changed.SessionID = "session-2"
	if _, ok := cache.Load(changed, time.Minute); ok {
		t.Fatal("Load() returned snapshot for a different exact session")
	}

	if err := cache.Store(target, snapshot); err != nil {
		t.Fatal(err)
	}
	now = reset
	if _, ok := cache.Load(target, time.Minute); ok {
		t.Fatal("Load() returned snapshot after its quota reset")
	}
}

func TestFileSnapshotCacheDoesNotExtendQuotaSourceTTL(t *testing.T) {
	now := time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)
	cache := &FileSnapshotCache{Directory: t.TempDir(), Now: func() time.Time { return now }}
	target := collector.Target{PaneID: "w1:p1", Harness: "codex", SessionID: "session-1"}
	snapshot := cachedTestSnapshot(now)
	snapshot.Quota = &usage.QuotaSnapshot{
		Windows:     []usage.QuotaWindow{{Label: "5h", UsedPercent: 42}},
		Source:      "codex-app-server",
		CollectedAt: now.Add(-59 * time.Second),
	}
	if err := cache.Store(target, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Load(target, 15*time.Second); !ok {
		t.Fatal("Load() missed quota before its source TTL")
	}

	now = now.Add(time.Second)
	if _, ok := cache.Load(target, 15*time.Second); ok {
		t.Fatal("Load() extended quota beyond its source TTL")
	}
}

func TestFileSnapshotCacheTreatsMalformedEntryAsMiss(t *testing.T) {
	cache := &FileSnapshotCache{Directory: t.TempDir()}
	target := collector.Target{PaneID: "w1:p1", Harness: "codex", SessionID: "session-1"}
	path, _, ok := cache.path(target)
	if !ok {
		t.Fatal("cache path is unavailable")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"stored_at":"not-a-time"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Load(target, time.Minute); ok {
		t.Fatal("Load() returned malformed entry")
	}
}

func cachedTestSnapshot(now time.Time) usage.Snapshot {
	tokens, _ := usage.NewTokens(100, 20, 0, 30, 5)
	return usage.Snapshot{
		SchemaVersion: usage.SchemaVersion,
		PaneID:        "w1:p1",
		WorkspaceID:   "w1",
		Harness:       "codex",
		SessionID:     "session-1",
		Model:         "gpt-test",
		Tokens:        tokens,
		Source:        "fixture",
		Confidence:    usage.ConfidenceExact,
		CollectedAt:   now,
	}
}
