package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/herdr"
	"github.com/calebcav/token-usage/internal/usage"
)

type fakeHerdr struct {
	snapshot herdr.Snapshot
	reports  map[string]herdr.Metadata
	process  map[string]herdr.ProcessInfo
	mu       sync.Mutex
}

func (client *fakeHerdr) ProcessInfo(_ context.Context, paneID string) (herdr.ProcessInfo, error) {
	info, ok := client.process[paneID]
	if !ok {
		return herdr.ProcessInfo{}, errors.New("process info unavailable")
	}
	return info, nil
}

func (client *fakeHerdr) Snapshot(context.Context) (herdr.Snapshot, error) {
	return client.snapshot, nil
}

func (client *fakeHerdr) ReportMetadata(_ context.Context, paneID string, metadata herdr.Metadata) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.reports == nil {
		client.reports = make(map[string]herdr.Metadata)
	}
	client.reports[paneID] = metadata
	return nil
}

func (client *fakeHerdr) report(paneID string) herdr.Metadata {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.reports[paneID]
}

type fakeCollector struct{}

func (fakeCollector) Harnesses() []string { return []string{"codex"} }

func (fakeCollector) Collect(_ context.Context, target collector.Target) (usage.Snapshot, error) {
	if target.SessionID == "broken" {
		return usage.Snapshot{}, errors.New("fixture is broken")
	}
	tokens, _ := usage.NewTokens(100, 20, 5, 30, 10)
	return usage.Snapshot{
		SchemaVersion: usage.SchemaVersion,
		Harness:       "codex",
		SessionID:     target.SessionID,
		Model:         "gpt-5",
		Tokens:        tokens,
		Context:       &usage.ContextWindow{Used: 155, Limit: 1_000},
		Source:        "fixture",
		Confidence:    target.Confidence,
		CollectedAt:   time.Unix(100, 0),
	}, nil
}

type detectingCollector struct{}

func (detectingCollector) Harnesses() []string { return []string{"aider"} }

func (detectingCollector) MatchProcess(processes []collector.Process) (string, error) {
	for _, process := range processes {
		if process.Name == "aider" {
			return "aider", nil
		}
	}
	return "", nil
}

func (detectingCollector) Collect(_ context.Context, target collector.Target) (usage.Snapshot, error) {
	tokens, _ := usage.NewTokens(10, 0, 0, 2, 0)
	return usage.Snapshot{
		SchemaVersion: usage.SchemaVersion,
		Harness:       "aider",
		SessionID:     "detected-session",
		Tokens:        tokens,
		Source:        "external-collector",
		Confidence:    target.Confidence,
		CollectedAt:   time.Unix(200, 0),
	}, nil
}

func TestCollectPublishesSuccessAndClearsFailures(t *testing.T) {
	registry, err := collector.NewRegistry(fakeCollector{})
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeHerdr{snapshot: herdr.Snapshot{
		Version:  "0.8.2",
		Protocol: 1,
		Agents: []herdr.Record{
			{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "codex", AgentStatus: "idle", AgentSession: &herdr.SessionRef{Agent: "codex", Value: "session-1"}},
			{PaneID: "w1:p2", WorkspaceID: "w1", Agent: "claude", AgentStatus: "idle", AgentSession: &herdr.SessionRef{Agent: "claude", Value: "session-2"}},
		},
	}}
	service := &Service{Registry: registry, Herdr: client}

	results, err := service.Collect(context.Background(), CollectOptions{Publish: true})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(results) != 2 || results[0].Snapshot == nil || results[1].Snapshot != nil {
		t.Fatalf("unexpected results: %+v", results)
	}
	if results[0].Snapshot.PaneID != "w1:p1" || results[0].Snapshot.State != "idle" {
		t.Fatalf("snapshot was not enriched: %+v", results[0].Snapshot)
	}
	if value := client.report("w1:p1").Tokens["usage"]; value == nil || *value != "Σ 155" {
		t.Fatalf("usage metadata = %v", value)
	}
	if value := client.report("w1:p2").Tokens["usage"]; value != nil {
		t.Fatalf("failed collector should clear usage, got %v", *value)
	}
}

func TestCollectFiltersWorkingAndPane(t *testing.T) {
	registry, _ := collector.NewRegistry(fakeCollector{})
	client := &fakeHerdr{snapshot: herdr.Snapshot{Version: "0.8.2", Agents: []herdr.Record{
		{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "codex", AgentStatus: "working", AgentSession: &herdr.SessionRef{Agent: "codex", Value: "one"}},
		{PaneID: "w1:p2", WorkspaceID: "w1", Agent: "codex", AgentStatus: "idle", AgentSession: &herdr.SessionRef{Agent: "codex", Value: "two"}},
	}}}
	service := &Service{Registry: registry, Herdr: client}

	results, err := service.Collect(context.Background(), CollectOptions{PaneID: "w1:p2"})
	if err != nil || len(results) != 1 || results[0].Target.PaneID != "w1:p2" {
		t.Fatalf("Collect() = %+v, %v", results, err)
	}
	results, err = service.Collect(context.Background(), CollectOptions{})
	if err != nil || len(results) != 1 || results[0].Target.PaneID != "w1:p2" {
		t.Fatalf("Collect() working filter = %+v, %v", results, err)
	}
}

func TestUniqueSessionsKeepsNewest(t *testing.T) {
	older := usage.Snapshot{Harness: "codex", SessionID: "same", CollectedAt: time.Unix(1, 0)}
	newer := usage.Snapshot{Harness: "codex", SessionID: "same", CollectedAt: time.Unix(2, 0)}
	results := []Result{
		{Target: collector.Target{PaneID: "w1:p1", WorkspaceID: "w1", Harness: "codex"}, Snapshot: &older},
		{Target: collector.Target{PaneID: "w1:p2", WorkspaceID: "w1", Harness: "codex"}, Snapshot: &newer},
		{Target: collector.Target{PaneID: "w1:p3", WorkspaceID: "w1", Harness: "claude"}, Error: "unsupported"},
	}
	unique := UniqueSessions(results)
	if len(unique) != 2 {
		t.Fatalf("UniqueSessions() len = %d, want 2", len(unique))
	}
	if unique[0].Snapshot == nil || unique[0].Snapshot.CollectedAt != newer.CollectedAt {
		t.Fatalf("newest duplicate was not retained: %+v", unique[0])
	}
}

func TestCollectDiscoversExternalHarnessByForegroundProcess(t *testing.T) {
	registry, err := collector.NewRegistry(detectingCollector{})
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeHerdr{
		snapshot: herdr.Snapshot{Version: "0.8.2", Panes: []herdr.Record{{PaneID: "w1:p4", WorkspaceID: "w1", CWD: "/repo", AgentStatus: "idle"}}},
		process: map[string]herdr.ProcessInfo{
			"w1:p4": {PaneID: "w1:p4", ForegroundProcesses: []herdr.ProcessItem{{Name: "aider", CWD: "/repo/subdir"}}},
		},
	}
	service := &Service{Registry: registry, Herdr: client}
	results, err := service.Collect(context.Background(), CollectOptions{IncludeWorking: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Snapshot == nil || results[0].Target.Harness != "aider" || results[0].Target.CWD != "/repo/subdir" {
		t.Fatalf("process discovery results = %+v", results)
	}
	if results[0].Snapshot.Confidence != usage.ConfidenceEstimated {
		t.Fatalf("confidence = %q", results[0].Snapshot.Confidence)
	}
}
