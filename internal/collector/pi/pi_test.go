package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	basecollector "github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

type contextResolverFunc func(context.Context, string, string) (uint64, error)

func (fn contextResolverFunc) ResolveContextLimit(ctx context.Context, provider, model string) (uint64, error) {
	return fn(ctx, provider, model)
}

func TestCollectExactSessionSumsActivePathUsage(t *testing.T) {
	root := t.TempDir()
	path := writePiSession(t, root, "proj", `
{"type":"session","version":3,"id":"session-1","timestamp":"2026-09-04T00:00:00.000Z","cwd":"/workspace/project"}
{"type":"model_change","id":"m1","parentId":null,"timestamp":"2026-09-04T00:00:01.000Z","provider":"anthropic","modelId":"claude-sonnet"}
{"type":"message","id":"u1","parentId":"m1","timestamp":"2026-09-04T00:00:02.000Z","message":{"role":"user","content":"hi"}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-09-04T00:00:03.000Z","message":{"role":"assistant","provider":"anthropic","model":"claude-sonnet","usage":{"input":10,"cacheRead":3,"cacheWrite":2,"output":5,"reasoning":1,"totalTokens":20}}}
{"type":"message","id":"t1","parentId":"a1","timestamp":"2026-09-04T00:00:04.000Z","message":{"role":"toolResult","toolName":"x","usage":{"input":7,"output":4,"totalTokens":11}}}
{"type":"compaction","id":"c1","parentId":"t1","timestamp":"2026-09-04T00:00:05.000Z","summary":"x","tokensBefore":99,"usage":{"input":2,"output":1,"totalTokens":3}}
{"type":"message","id":"abandoned","parentId":"a1","timestamp":"2026-09-04T00:00:06.000Z","message":{"role":"assistant","provider":"openai","model":"wrong","usage":{"input":100,"output":100,"totalTokens":200}}}
{"type":"message","id":"a2","parentId":"c1","timestamp":"2026-09-04T00:00:07.000Z","message":{"role":"assistant","provider":"openai-codex","model":"gpt-5.5","usage":{"input":1,"cacheRead":9,"output":2,"totalTokens":12}}}
`)
	_ = path

	collector := New(Config{
		SessionDir: root,
		Now:        fixedNow,
		ContextResolver: contextResolverFunc(func(_ context.Context, provider, model string) (uint64, error) {
			if provider != "openai-codex" || model != "gpt-5.5" {
				t.Fatalf("resolver model = %s/%s", provider, model)
			}
			return 272_000, nil
		}),
	})
	snapshot, err := collector.Collect(context.Background(), basecollector.Target{Harness: "pi", SessionID: "session-1", Confidence: usage.ConfidenceExact})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	want := usage.Tokens{FreshInput: 20, CacheRead: 12, CacheWrite: 2, Output: 12, Reasoning: 1, Total: 46}
	if snapshot.Tokens != want {
		t.Fatalf("tokens = %+v, want %+v", snapshot.Tokens, want)
	}
	if snapshot.SessionID != "session-1" || snapshot.Provider != "openai-codex" || snapshot.Model != "gpt-5.5" {
		t.Fatalf("metadata = session %q provider %q model %q", snapshot.SessionID, snapshot.Provider, snapshot.Model)
	}
	if snapshot.Context == nil || *snapshot.Context != (usage.ContextWindow{Used: 12, Limit: 272_000}) {
		t.Fatalf("context = %+v", snapshot.Context)
	}
	if snapshot.Source != sourceLabel || snapshot.Confidence != usage.ConfidenceExact || !snapshot.CollectedAt.Equal(fixedNow().UTC()) {
		t.Fatalf("snapshot metadata = %+v", snapshot)
	}
}

func TestCollectExactSessionPath(t *testing.T) {
	root := t.TempDir()
	path := writePiSession(t, root, "proj", `
{"type":"session","version":3,"id":"session-path","timestamp":"2026-09-04T00:00:00.000Z","cwd":"/workspace/project"}
{"type":"message","id":"a1","parentId":null,"timestamp":"2026-09-04T00:00:03.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":1,"output":2,"totalTokens":3}}}
`)
	collector := New(Config{SessionDir: root, Now: fixedNow})
	target := basecollector.Target{
		Harness:    "pi",
		SessionRef: &basecollector.SessionRef{Source: "herdr:pi", Agent: "pi", Kind: "path", Value: path},
		Confidence: usage.ConfidenceExact,
	}

	snapshot, err := collector.Collect(context.Background(), target)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if snapshot.SessionID != "session-path" || snapshot.Confidence != usage.ConfidenceExact || snapshot.Tokens.Total != 3 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestCollectRejectsSessionPathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outsidePath := writePiSession(t, t.TempDir(), "outside", `
{"type":"session","version":3,"id":"outside","timestamp":"2026-09-04T00:00:00.000Z","cwd":"/workspace/project"}
`)
	collector := New(Config{SessionDir: root, Now: fixedNow})
	_, err := collector.Collect(context.Background(), basecollector.Target{
		Harness:    "pi",
		SessionRef: &basecollector.SessionRef{Source: "herdr:pi", Agent: "pi", Kind: "path", Value: outsidePath},
		Confidence: usage.ConfidenceExact,
	})
	if !errors.Is(err, basecollector.ErrSessionNotFound) {
		t.Fatalf("error = %v, want session not found", err)
	}
}

func TestCollectFallbackByRecentCWD(t *testing.T) {
	root := t.TempDir()
	path := writePiSession(t, root, "proj", `
{"type":"session","version":3,"id":"session-cwd","timestamp":"2026-09-04T00:00:00.000Z","cwd":"/workspace/project"}
{"type":"message","id":"a1","parentId":null,"timestamp":"2026-09-04T00:00:03.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":1,"output":2,"totalTokens":3}}}
`)
	mod := fixedNow().Add(-time.Minute)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}

	collector := New(Config{SessionDir: root, AllowCWDRecencyFallback: true, Now: fixedNow})
	snapshot, err := collector.Collect(context.Background(), basecollector.Target{Harness: "pi", CWD: "/workspace/project", Confidence: usage.ConfidenceEstimated})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if snapshot.SessionID != "session-cwd" || snapshot.Confidence != usage.ConfidenceEstimated || snapshot.Tokens.Total != 3 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestCollectFallbackAmbiguous(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		path := writePiSession(t, root, name, `
{"type":"session","version":3,"id":"session-`+name+`","timestamp":"2026-09-04T00:00:00.000Z","cwd":"/workspace/project"}
{"type":"message","id":"a1","parentId":null,"timestamp":"2026-09-04T00:00:03.000Z","message":{"role":"assistant","usage":{"input":1,"output":1,"totalTokens":2}}}
`)
		mod := fixedNow().Add(-time.Minute)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	collector := New(Config{SessionDir: root, AllowCWDRecencyFallback: true, Now: fixedNow})
	_, err := collector.Collect(context.Background(), basecollector.Target{Harness: "pi", CWD: "/workspace/project", Confidence: usage.ConfidenceEstimated})
	if !errors.Is(err, basecollector.ErrAmbiguous) {
		t.Fatalf("error = %v, want ambiguous", err)
	}
}

func TestCollectFallbackUsesNewestRecentCWDWhenIdle(t *testing.T) {
	root := t.TempDir()
	for index, name := range []string{"old", "new"} {
		path := writePiSession(t, root, name, `
{"type":"session","version":3,"id":"session-`+name+`","timestamp":"2026-09-04T00:00:00.000Z","cwd":"/workspace/project"}
{"type":"message","id":"a1","parentId":null,"timestamp":"2026-09-04T00:00:03.000Z","message":{"role":"assistant","usage":{"input":1,"output":1,"totalTokens":2}}}
`)
		mod := fixedNow().Add(time.Duration(index-2) * time.Minute)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	collector := New(Config{SessionDir: root, AllowCWDRecencyFallback: true, Now: fixedNow})
	snapshot, err := collector.Collect(context.Background(), basecollector.Target{Harness: "pi", CWD: "/workspace/project", State: "idle", Confidence: usage.ConfidenceEstimated})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if snapshot.SessionID != "session-new" || snapshot.Confidence != usage.ConfidenceEstimated {
		t.Fatalf("snapshot = %+v, want newest estimated fallback", snapshot)
	}
}

func TestCollectIgnoresPartialTrailingRecord(t *testing.T) {
	root := t.TempDir()
	path := writePiSession(t, root, "proj", `
{"type":"session","version":3,"id":"partial","timestamp":"2026-09-04T00:00:00.000Z","cwd":"/workspace/project"}
{"type":"message","id":"a1","parentId":null,"timestamp":"2026-09-04T00:00:03.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":1,"output":2,"totalTokens":3}}}
`)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"message","id":"half","parentId":"a1","timestamp":"2026-09-04T00:00:04.000Z","message":{"role":"assistant","usage":`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	collector := New(Config{SessionDir: root, Now: fixedNow})
	snapshot, err := collector.Collect(context.Background(), basecollector.Target{Harness: "pi", SessionID: "partial", Confidence: usage.ConfidenceExact})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if snapshot.Tokens.Total != 3 || snapshot.Model != "m" || snapshot.Provider != "p" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestCollectRejectsMalformedUsage(t *testing.T) {
	root := t.TempDir()
	writePiSession(t, root, "proj", `
{"type":"session","version":3,"id":"bad","timestamp":"2026-09-04T00:00:00.000Z","cwd":"/workspace/project"}
{"type":"message","id":"a1","parentId":null,"timestamp":"2026-09-04T00:00:03.000Z","message":{"role":"assistant","usage":{"input":1,"output":1,"reasoning":2,"totalTokens":2}}}
`)
	collector := New(Config{SessionDir: root, Now: fixedNow})
	_, err := collector.Collect(context.Background(), basecollector.Target{Harness: "pi", SessionID: "bad", Confidence: usage.ConfidenceExact})
	if !errors.Is(err, basecollector.ErrMalformed) {
		t.Fatalf("error = %v, want malformed", err)
	}
}

func TestParseContextLimit(t *testing.T) {
	output := "provider      model    context  max-out\nopenai-codex  gpt-5.5  272K     128K\n"
	limit, err := parseContextLimit(output, "openai-codex", "gpt-5.5")
	if err != nil || limit != 272_000 {
		t.Fatalf("parseContextLimit() = %d, %v", limit, err)
	}
}

func TestMatchProcess(t *testing.T) {
	collector := New(Config{})
	harness, err := collector.MatchProcess([]basecollector.Process{{Name: "node", Argv: []string{"/usr/lib/node_modules/@earendil-works/pi-coding-agent/dist/cli.js"}}})
	if err != nil || harness != "pi" {
		t.Fatalf("MatchProcess() = %q, %v", harness, err)
	}
}

func writePiSession(t *testing.T, root, project, content string) string {
	t.Helper()
	dir := filepath.Join(root, "--"+project+"--")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, project+".jsonl")
	if err := os.WriteFile(path, []byte(content[1:]), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixedNow() time.Time {
	return time.Date(2026, 9, 4, 1, 0, 0, 0, time.UTC)
}
