package opencode

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	basecollector "github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const testMessageSchema = `
CREATE TABLE "message" (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	time_created INTEGER NOT NULL,
	time_updated INTEGER NOT NULL,
	data TEXT NOT NULL
)`

type contextResolverFunc func(context.Context, string, string, string) (uint64, error)

func (function contextResolverFunc) ResolveContextLimit(ctx context.Context, cwd, providerID, modelID string) (uint64, error) {
	return function(ctx, cwd, providerID, modelID)
}

func TestCollectUsesLatestCompletedAssistantMessageForContext(t *testing.T) {
	path := createTestDB(t, testSessionSchema+";"+testMessageSchema)
	insertTestSession(t, path, testSession{
		id:         "session-context",
		directory:  "/workspace/project",
		model:      `{"providerID":"session-provider","modelID":"session-model"}`,
		input:      900,
		output:     80,
		reasoning:  10,
		cacheRead:  300,
		cacheWrite: 5,
		updatedAt:  100,
	})
	insertTestMessage(t, path, "msg_old", "session-context", 10, `{
		"role":"assistant",
		"time":{"completed":10},
		"providerID":"provider-old",
		"modelID":"model-old",
		"tokens":{"total":3,"input":1,"output":2,"reasoning":0,"cache":{"read":0,"write":0}}
	}`)
	insertTestMessage(t, path, "msg_selected", "session-context", 20, `{
		"role":"assistant",
		"time":{"completed":20},
		"providerID":"provider-current",
		"modelID":"model-current",
		"tokens":{"total":170,"input":100,"output":20,"reasoning":7,"cache":{"read":40,"write":3}}
	}`)
	insertTestMessage(t, path, "msg_incomplete", "session-context", 30, `{
		"role":"assistant",
		"time":{"created":30},
		"providerID":"provider-incomplete",
		"modelID":"model-incomplete",
		"tokens":{"total":999,"input":999,"output":999,"reasoning":999,"cache":{"read":999,"write":999}}
	}`)
	insertTestMessage(t, path, "msg_zero_output", "session-context", 40, `{
		"role":"assistant",
		"time":{"completed":40},
		"providerID":"provider-zero",
		"modelID":"model-zero",
		"tokens":{"total":0,"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}
	}`)

	resolverCalls := 0
	item := New(Config{
		DBPath:          path,
		ContextCacheDir: t.TempDir(),
		ContextResolver: contextResolverFunc(func(_ context.Context, cwd, providerID, modelID string) (uint64, error) {
			resolverCalls++
			if cwd != "/workspace/project" || providerID != "provider-current" || modelID != "model-current" {
				t.Fatalf("resolver input = %q %q/%q", cwd, providerID, modelID)
			}
			return 4_000, nil
		}),
	})
	snapshot, err := item.Collect(context.Background(), basecollector.Target{
		SessionID: "session-context",
		CWD:       "/different/pane/directory",
	})
	if err != nil {
		t.Fatal(err)
	}

	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolverCalls)
	}
	wantContext := &usage.ContextWindow{Used: 170, Limit: 4_000}
	if snapshot.Context == nil || *snapshot.Context != *wantContext {
		t.Fatalf("Context = %#v, want %#v", snapshot.Context, wantContext)
	}
	if snapshot.Provider != "session-provider" || snapshot.Model != "session-model" {
		t.Fatalf("aggregate model = %q/%q", snapshot.Provider, snapshot.Model)
	}
	if snapshot.Tokens.Total != 1_295 {
		t.Fatalf("aggregate tokens changed: %+v", snapshot.Tokens)
	}
}

func TestCollectWithoutMessageTableKeepsValidSnapshotOffline(t *testing.T) {
	path := createTestDB(t, testSessionSchema)
	insertTestSession(t, path, testSession{
		id:        "session-legacy",
		directory: "/workspace/legacy",
		input:     12,
		output:    3,
		updatedAt: 1,
	})

	resolverCalls := 0
	snapshot, err := New(Config{
		DBPath: path,
		ContextResolver: contextResolverFunc(func(context.Context, string, string, string) (uint64, error) {
			resolverCalls++
			return 0, errors.New("must not be called")
		}),
	}).Collect(context.Background(), basecollector.Target{SessionID: "session-legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if resolverCalls != 0 || snapshot.Context != nil {
		t.Fatalf("resolver calls = %d, Context = %#v", resolverCalls, snapshot.Context)
	}
	if snapshot.Tokens.FreshInput != 12 || snapshot.Tokens.Output != 3 {
		t.Fatalf("aggregate tokens = %+v", snapshot.Tokens)
	}
}

func TestLatestMessageContextRejectsInvalidCounters(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{
			name: "negative",
			data: `{"role":"assistant","time":{"completed":1},"providerID":"provider","modelID":"model","tokens":{"total":1,"input":1,"output":-1,"reasoning":0,"cache":{"read":0,"write":0}}}`,
		},
		{
			name: "overflow",
			data: `{"role":"assistant","time":{"completed":1},"providerID":"provider","modelID":"model","tokens":{"total":1,"input":9223372036854775807,"output":9223372036854775807,"reasoning":9223372036854775807,"cache":{"read":0,"write":0}}}`,
		},
		{
			name: "fractional",
			data: `{"role":"assistant","time":{"completed":1},"providerID":"provider","modelID":"model","tokens":{"total":1,"input":1.5,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}}`,
		},
		{
			name: "negative total",
			data: `{"role":"assistant","time":{"completed":1},"providerID":"provider","modelID":"model","tokens":{"total":-1,"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := createTestDB(t, testSessionSchema+";"+testMessageSchema)
			insertTestMessage(t, path, "msg_invalid", "session-invalid", 1, test.data)
			db, err := openReadOnly(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if message, ok := latestMessageContext(context.Background(), db, "session-invalid"); ok {
				t.Fatalf("message = %+v, want invalid", message)
			}
		})
	}

	if _, ok := checkedSum(math.MaxUint64, 1); ok {
		t.Fatal("checkedSum accepted uint64 overflow")
	}
}

func TestLatestMessageContextAcceptsReasoningOnlyCompletion(t *testing.T) {
	path := createTestDB(t, testSessionSchema+";"+testMessageSchema)
	insertTestMessage(t, path, "msg_reasoning", "session-reasoning", 1, `{
		"role":"assistant",
		"time":{"completed":1},
		"providerID":"provider",
		"modelID":"model",
		"tokens":{"total":8,"input":0,"output":0,"reasoning":8,"cache":{"read":0,"write":0}}
	}`)
	db, err := openReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	message, ok := latestMessageContext(context.Background(), db, "session-reasoning")
	if !ok || message.Used != 8 || message.Provider != "provider" || message.Model != "model" {
		t.Fatalf("message = %+v, %v", message, ok)
	}
}

func TestParseContextLimitOutputFindsExactModelBlock(t *testing.T) {
	output := []byte(`provider/model-prefix
{"limit":{"context":111}}
provider/model-target-extra
{"limit":{"context":222}}
provider/model-target
{
  "id": "model-target",
  "limit": {
    "context": 333,
    "output": 20
  }
}
provider/model-z
{"limit":{"context":444}}
`)

	limit, err := parseContextLimitOutput(output, "provider", "model-target")
	if err != nil {
		t.Fatal(err)
	}
	if limit != 333 {
		t.Fatalf("limit = %d, want 333", limit)
	}
}

func TestCollectResolverFailureLeavesContextNil(t *testing.T) {
	path := createContextTestDB(t, "session-failure", "/workspace/failure", "provider", "model", "private response")
	resolverCalls := 0
	item := New(Config{
		DBPath:          path,
		ContextCacheDir: t.TempDir(),
		ContextResolver: contextResolverFunc(func(context.Context, string, string, string) (uint64, error) {
			resolverCalls++
			return 0, errors.New("resolver failure with\x1b[31m control data")
		}),
	})

	for range 2 {
		snapshot, err := item.Collect(context.Background(), basecollector.Target{
			SessionID: "session-failure",
			CWD:       "/workspace/failure",
		})
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Context != nil || snapshot.Tokens.FreshInput != 10 {
			t.Fatalf("snapshot = %+v", snapshot)
		}
	}
	if resolverCalls != 2 {
		t.Fatalf("resolver calls = %d, want transient failures retried", resolverCalls)
	}
}

func TestContextResolutionDoesNotSerializeDifferentModels(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	item := New(Config{
		ContextCacheDir: t.TempDir(),
		ContextResolver: contextResolverFunc(func(context.Context, string, string, string) (uint64, error) {
			started <- struct{}{}
			<-release
			return 4_000, nil
		}),
	})

	var wait sync.WaitGroup
	wait.Add(2)
	for _, model := range []string{"model-one", "model-two"} {
		go func() {
			defer wait.Done()
			if limit, ok := item.resolveContextLimit(context.Background(), "/workspace", "provider", model); !ok || limit != 4_000 {
				t.Errorf("resolveContextLimit() = %d, %v", limit, ok)
			}
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			wait.Wait()
			t.Fatal("different model resolutions were serialized")
		}
	}
	close(release)
	wait.Wait()
}

func TestContextResolutionFollowerHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	item := New(Config{
		ContextCacheDir: t.TempDir(),
		ContextResolver: contextResolverFunc(func(context.Context, string, string, string) (uint64, error) {
			close(started)
			<-release
			return 4_000, nil
		}),
	})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		item.resolveContextLimit(context.Background(), "/workspace", "provider", "model")
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	startedAt := time.Now()
	if limit, ok := item.resolveContextLimit(ctx, "/workspace", "provider", "model"); ok || limit != 0 {
		t.Fatalf("canceled follower = %d, %v", limit, ok)
	}
	if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled follower waited %v for the leader", elapsed)
	}
	close(release)
	<-leaderDone
}

func TestContextLimitCachePersistsExpiresAndContainsNoPrivateData(t *testing.T) {
	const (
		cwd     = "/workspace/private-customer-project"
		content = "PRIVATE_PROMPT_AND_RESPONSE"
	)
	path := createContextTestDB(t, "session-cache", cwd, "provider-private", "model-private", content)
	cacheDir := t.TempDir()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	resolverCalls := 0
	resolver := contextResolverFunc(func(_ context.Context, gotCWD, providerID, modelID string) (uint64, error) {
		resolverCalls++
		if gotCWD != cwd || providerID != "provider-private" || modelID != "model-private" {
			t.Fatalf("resolver input = %q %q/%q", gotCWD, providerID, modelID)
		}
		return 8_000, nil
	})
	config := Config{
		DBPath:          path,
		ContextCacheDir: cacheDir,
		ContextResolver: resolver,
		Clock:           func() time.Time { return now },
	}
	target := basecollector.Target{SessionID: "session-cache", CWD: cwd}

	for range 2 {
		snapshot, err := New(config).Collect(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Context == nil || snapshot.Context.Limit != 8_000 {
			t.Fatalf("Context = %#v", snapshot.Context)
		}
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1 across collector instances", resolverCalls)
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache entries = %d, want 1", len(entries))
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if permission := info.Mode().Perm(); permission != 0o600 {
		t.Fatalf("cache permissions = %o, want 600", permission)
	}
	cachePath := filepath.Join(cacheDir, entries[0].Name())
	cacheData, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	persisted := entries[0].Name() + string(cacheData)
	for _, private := range []string{cwd, content} {
		if strings.Contains(persisted, private) {
			t.Fatalf("cache persisted private value %q: %s", private, persisted)
		}
	}

	now = now.Add(contextCacheTTL + time.Second)
	if _, err := New(config).Collect(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if resolverCalls != 2 {
		t.Fatalf("resolver calls after expiry = %d, want 2", resolverCalls)
	}
}

func createContextTestDB(t *testing.T, sessionID, directory, providerID, modelID, privateContent string) string {
	t.Helper()
	path := createTestDB(t, testSessionSchema+";"+testMessageSchema)
	insertTestSession(t, path, testSession{
		id:        sessionID,
		directory: directory,
		input:     10,
		output:    2,
		updatedAt: 1,
	})
	data := `{
		"role":"assistant",
		"time":{"completed":1},
		"providerID":"` + providerID + `",
		"modelID":"` + modelID + `",
		"tokens":{"total":18,"input":10,"output":2,"reasoning":1,"cache":{"read":4,"write":1}},
		"prompt":"` + privateContent + `",
		"response":"` + privateContent + `"
	}`
	insertTestMessage(t, path, "msg_context", sessionID, 1, data)
	return path
}

func insertTestMessage(t *testing.T, path, id, sessionID string, createdAt int64, data string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO "message" (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`, id, sessionID, createdAt, createdAt, data); err != nil {
		t.Fatal(err)
	}
}
