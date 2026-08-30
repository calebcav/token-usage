package opencode

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	basecollector "github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const testSessionSchema = `
CREATE TABLE "session" (
	id TEXT PRIMARY KEY,
	directory TEXT NOT NULL,
	parent_id TEXT,
	model TEXT,
	tokens_input INTEGER NOT NULL,
	tokens_output INTEGER NOT NULL,
	tokens_reasoning INTEGER NOT NULL,
	tokens_cache_read INTEGER NOT NULL,
	tokens_cache_write INTEGER NOT NULL,
	time_updated INTEGER NOT NULL
)`

type testSession struct {
	id         string
	directory  string
	parentID   any
	model      any
	input      int64
	output     int64
	reasoning  int64
	cacheRead  int64
	cacheWrite int64
	updatedAt  int64
}

func TestCollectExactSessionNormalizesAdditiveCounters(t *testing.T) {
	path := createTestDB(t, testSessionSchema)
	insertTestSession(t, path, testSession{
		id:         "session-test-001",
		directory:  "/workspace/sanitized-project",
		model:      `{"providerID":"openai","modelID":"gpt-test"}`,
		input:      100,
		output:     20,
		reasoning:  7,
		cacheRead:  40,
		cacheWrite: 3,
		updatedAt:  1_700_000_000,
	})

	collectedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	collector := New(Config{DBPath: path})
	collector.now = func() time.Time { return collectedAt }
	snapshot, err := collector.Collect(context.Background(), basecollector.Target{
		PaneID:      "pane-test",
		WorkspaceID: "workspace-test",
		SessionID:   "session-test-001",
		CWD:         "/workspace/another-project",
		State:       "idle",
	})
	if err != nil {
		t.Fatal(err)
	}

	if snapshot.Harness != "opencode" || snapshot.SessionID != "session-test-001" {
		t.Fatalf("identity = %q/%q", snapshot.Harness, snapshot.SessionID)
	}
	if snapshot.Provider != "openai" || snapshot.Model != "gpt-test" {
		t.Fatalf("model = %q/%q", snapshot.Provider, snapshot.Model)
	}
	if snapshot.PaneID != "pane-test" || snapshot.WorkspaceID != "workspace-test" || snapshot.State != "idle" {
		t.Fatalf("target metadata not preserved: %+v", snapshot)
	}
	if snapshot.Source != "opencode-sqlite" {
		t.Fatalf("source = %q", snapshot.Source)
	}
	if snapshot.Confidence != usage.ConfidenceExact {
		t.Fatalf("confidence = %q, want exact", snapshot.Confidence)
	}
	if snapshot.CollectedAt != collectedAt {
		t.Fatalf("collected_at = %v, want %v", snapshot.CollectedAt, collectedAt)
	}
	wantTokens := usage.Tokens{
		FreshInput: 100,
		CacheRead:  40,
		CacheWrite: 3,
		Output:     27,
		Reasoning:  7,
		Total:      170,
	}
	if snapshot.Tokens != wantTokens {
		t.Fatalf("tokens = %+v, want %+v", snapshot.Tokens, wantTokens)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("snapshot validation: %v", err)
	}
}

func TestParseModelVariants(t *testing.T) {
	tests := []struct {
		name         string
		value        sql.NullString
		wantProvider string
		wantModel    string
		wantError    bool
	}{
		{
			name:         "uppercase ID suffix",
			value:        sql.NullString{String: `{"providerID":"anthropic","modelID":"claude-test"}`, Valid: true},
			wantProvider: "anthropic",
			wantModel:    "claude-test",
		},
		{
			name:         "camel Id suffix",
			value:        sql.NullString{String: `{"providerId":"openai","modelId":"gpt-test"}`, Valid: true},
			wantProvider: "openai",
			wantModel:    "gpt-test",
		},
		{
			name:         "current OpenCode id field",
			value:        sql.NullString{String: `{"id":"gpt-test","providerID":"openai","variant":"max"}`, Valid: true},
			wantProvider: "openai",
			wantModel:    "gpt-test",
		},
		{
			name:         "snake case",
			value:        sql.NullString{String: `{"provider_id":"local","model_id":"model-test"}`, Valid: true},
			wantProvider: "local",
			wantModel:    "model-test",
		},
		{
			name:      "JSON string",
			value:     sql.NullString{String: `"model-test"`, Valid: true},
			wantModel: "model-test",
		},
		{
			name:  "null",
			value: sql.NullString{String: "null", Valid: true},
		},
		{
			name:      "malformed JSON",
			value:     sql.NullString{String: `{`, Valid: true},
			wantError: true,
		},
		{
			name:      "wrong field type",
			value:     sql.NullString{String: `{"modelID":42}`, Valid: true},
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, model, err := parseModel(test.value)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError %v", err, test.wantError)
			}
			if provider != test.wantProvider || model != test.wantModel {
				t.Fatalf("model = %q/%q, want %q/%q", provider, model, test.wantProvider, test.wantModel)
			}
		})
	}
}

func TestCollectRejectsMissingSchema(t *testing.T) {
	path := createTestDB(t, `CREATE TABLE "session" (id TEXT PRIMARY KEY)`)
	_, err := New(Config{DBPath: path}).Collect(context.Background(), basecollector.Target{SessionID: "session-test"})
	if !errors.Is(err, basecollector.ErrMalformed) {
		t.Fatalf("error = %v, want ErrMalformed", err)
	}
	if errors.Is(err, basecollector.ErrSessionNotFound) {
		t.Fatalf("schema drift was reported as not found: %v", err)
	}
}

func TestCollectRejectsNegativeCounters(t *testing.T) {
	fields := []string{"input", "output", "reasoning", "cache read", "cache write"}
	for index, field := range fields {
		t.Run(field, func(t *testing.T) {
			path := createTestDB(t, testSessionSchema)
			values := []int64{1, 1, 1, 1, 1}
			values[index] = -1
			insertTestSession(t, path, testSession{
				id:         "session-negative-test",
				directory:  "/workspace/sanitized-project",
				model:      nil,
				input:      values[0],
				output:     values[1],
				reasoning:  values[2],
				cacheRead:  values[3],
				cacheWrite: values[4],
				updatedAt:  1,
			})

			_, err := New(Config{DBPath: path}).Collect(context.Background(), basecollector.Target{SessionID: "session-negative-test"})
			if !errors.Is(err, basecollector.ErrMalformed) {
				t.Fatalf("error = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestCollectReturnsNotFoundWithoutFallback(t *testing.T) {
	path := createTestDB(t, testSessionSchema)
	insertTestSession(t, path, testSession{
		id:        "session-present",
		directory: "/workspace/sanitized-project",
		updatedAt: 1,
	})

	_, err := New(Config{DBPath: path}).Collect(context.Background(), basecollector.Target{
		SessionID: "session-missing",
		CWD:       "/workspace/sanitized-project",
	})
	if !errors.Is(err, basecollector.ErrSessionNotFound) {
		t.Fatalf("error = %v, want ErrSessionNotFound", err)
	}
}

func TestCollectDoesNotReplaceAuthoritativeIDWithDirectoryFallback(t *testing.T) {
	path := createTestDB(t, testSessionSchema)
	insertTestSession(t, path, testSession{
		id:        "session-present",
		directory: "/workspace/sanitized-project",
		updatedAt: time.Now().UnixMilli(),
	})

	_, err := New(Config{
		DBPath:                 path,
		AllowDirectoryFallback: true,
	}).Collect(context.Background(), basecollector.Target{
		SessionID: "session-missing",
		CWD:       "/workspace/sanitized-project/.",
	})
	if !errors.Is(err, basecollector.ErrSessionNotFound) {
		t.Fatalf("error = %v, want ErrSessionNotFound", err)
	}
}

func TestCollectDirectoryFallbackUsesOnlyUniqueRecentSession(t *testing.T) {
	path := createTestDB(t, testSessionSchema)
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	insertTestSession(t, path, testSession{
		id:        "session-stale",
		directory: "/workspace/sanitized-project",
		input:     10,
		updatedAt: now.Add(-3 * time.Hour).UnixMilli(),
	})
	insertTestSession(t, path, testSession{
		id:        "session-recent",
		directory: "/workspace/sanitized-project",
		input:     20,
		updatedAt: now.Add(-10 * time.Minute).UnixMilli(),
	})

	item := New(Config{
		DBPath:                 path,
		AllowDirectoryFallback: true,
	})
	item.now = func() time.Time { return now }
	snapshot, err := item.Collect(context.Background(), basecollector.Target{CWD: "/workspace/sanitized-project"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SessionID != "session-recent" || snapshot.Tokens.FreshInput != 20 || snapshot.Confidence != usage.ConfidenceEstimated {
		t.Fatalf("fallback snapshot = %+v", snapshot)
	}
}

func TestCollectDirectoryFallbackIgnoresChildSessionsButExactIDCanResolveThem(t *testing.T) {
	path := createTestDB(t, testSessionSchema)
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	insertTestSession(t, path, testSession{
		id:        "session-root",
		directory: "/workspace/delegated-project",
		input:     20,
		updatedAt: now.Add(-10 * time.Minute).UnixMilli(),
	})
	insertTestSession(t, path, testSession{
		id:        "session-child",
		directory: "/workspace/delegated-project",
		parentID:  "session-root",
		input:     200,
		updatedAt: now.Add(-time.Minute).UnixMilli(),
	})

	item := New(Config{DBPath: path, AllowDirectoryFallback: true})
	item.now = func() time.Time { return now }
	fallback, err := item.Collect(context.Background(), basecollector.Target{CWD: "/workspace/delegated-project"})
	if err != nil {
		t.Fatal(err)
	}
	if fallback.SessionID != "session-root" || fallback.Tokens.FreshInput != 20 || fallback.Confidence != usage.ConfidenceEstimated {
		t.Fatalf("fallback snapshot = %+v, want root session only", fallback)
	}
	exact, err := item.Collect(context.Background(), basecollector.Target{SessionID: "session-child"})
	if err != nil {
		t.Fatal(err)
	}
	if exact.SessionID != "session-child" || exact.Tokens.FreshInput != 200 || exact.Confidence != usage.ConfidenceExact {
		t.Fatalf("exact snapshot = %+v, want exact child session", exact)
	}
}

func TestCollectDirectoryFallbackRejectsMultipleRecentSessions(t *testing.T) {
	path := createTestDB(t, testSessionSchema)
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	for index, id := range []string{"session-a", "session-b"} {
		insertTestSession(t, path, testSession{
			id:        id,
			directory: "/workspace/sanitized-project",
			updatedAt: now.Add(-time.Duration(index+1) * time.Minute).UnixMilli(),
		})
	}

	item := New(Config{DBPath: path, AllowDirectoryFallback: true})
	item.now = func() time.Time { return now }
	_, err := item.Collect(context.Background(), basecollector.Target{CWD: "/workspace/sanitized-project"})
	if !errors.Is(err, basecollector.ErrAmbiguous) {
		t.Fatalf("error = %v, want ErrAmbiguous", err)
	}
}

func TestCollectDirectoryFallbackReturnsNotFound(t *testing.T) {
	path := createTestDB(t, testSessionSchema)
	_, err := New(Config{
		DBPath:                 path,
		AllowDirectoryFallback: true,
	}).Collect(context.Background(), basecollector.Target{CWD: "/workspace/missing-project"})
	if !errors.Is(err, basecollector.ErrSessionNotFound) {
		t.Fatalf("error = %v, want ErrSessionNotFound", err)
	}
}

func TestOpenReadOnlyEnablesSafetyPragmas(t *testing.T) {
	path := createTestDB(t, testSessionSchema)
	db, err := openReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var queryOnly, timeout int
	if err := db.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if queryOnly != 1 || timeout != busyTimeout {
		t.Fatalf("pragmas query_only=%d busy_timeout=%d", queryOnly, timeout)
	}
	if _, err := db.Exec(`INSERT INTO "session" (id, directory, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, time_updated) VALUES ('write-test', '/workspace/test', 0, 0, 0, 0, 0, 0)`); err == nil {
		t.Fatal("read-only collector database accepted a write")
	}
}

func TestDefaultDBPathPrecedence(t *testing.T) {
	t.Run("OPENCODE_DB", func(t *testing.T) {
		t.Setenv("OPENCODE_DB", "/configured/opencode-test.db")
		t.Setenv("XDG_DATA_HOME", "/xdg-test")
		path, err := DefaultDBPath()
		if err != nil {
			t.Fatal(err)
		}
		if path != "/configured/opencode-test.db" {
			t.Fatalf("path = %q", path)
		}
	})

	t.Run("XDG_DATA_HOME", func(t *testing.T) {
		t.Setenv("OPENCODE_DB", "")
		t.Setenv("XDG_DATA_HOME", "/xdg-test")
		path, err := DefaultDBPath()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join("/xdg-test", "opencode", "opencode.db")
		if path != want {
			t.Fatalf("path = %q, want %q", path, want)
		}
	})

	t.Run("home fallback", func(t *testing.T) {
		t.Setenv("OPENCODE_DB", "")
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "/home/sanitized-user")
		path, err := DefaultDBPath()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join("/home/sanitized-user", ".local", "share", "opencode", "opencode.db")
		if path != want {
			t.Fatalf("path = %q, want %q", path, want)
		}
	})
}

func createTestDB(t *testing.T, schema string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode-test.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return path
}

func insertTestSession(t *testing.T, path string, session testSession) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		INSERT INTO "session" (
			id, directory, parent_id, model,
			tokens_input, tokens_output, tokens_reasoning,
			tokens_cache_read, tokens_cache_write, time_updated
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.id,
		session.directory,
		session.parentID,
		session.model,
		session.input,
		session.output,
		session.reasoning,
		session.cacheRead,
		session.cacheWrite,
		session.updatedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
}
