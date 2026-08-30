package external

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

type fakeRunner struct {
	stdout []byte
	stderr []byte
	err    error
	argv   []string
	input  []byte
}

func (runner *fakeRunner) Run(_ context.Context, argv []string, input []byte) ([]byte, []byte, error) {
	runner.argv = append([]string(nil), argv...)
	runner.input = append([]byte(nil), input...)
	return runner.stdout, runner.stderr, runner.err
}

func TestCollectUsesVersionedJSONContract(t *testing.T) {
	runner := &fakeRunner{stdout: []byte(`{
      "schema_version":1,
      "model":"custom-model",
      "tokens":{"fresh_input":80,"cache_read":20,"cache_write":0,"output":10,"reasoning":2,"total":110}
    }`)}
	now := time.Unix(500, 0).UTC()
	item, err := New(Config{SchemaVersion: 1, Collectors: map[string]Command{
		"aider": {Command: []string{"usage-for-aider", "--stdin"}},
	}}, WithRunner(runner), WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	target := collector.Target{PaneID: "w1:p1", Harness: "aider", SessionID: "s1", CWD: "/repo", Confidence: usage.ConfidenceExact}

	snapshot, err := item.Collect(context.Background(), target)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if snapshot.Harness != "aider" || snapshot.SessionID != "s1" || snapshot.Source != "external-collector" || snapshot.CollectedAt != now {
		t.Fatalf("unexpected defaults: %+v", snapshot)
	}
	if !reflect.DeepEqual(runner.argv, []string{"usage-for-aider", "--stdin"}) {
		t.Fatalf("argv = %v", runner.argv)
	}
	var request Request
	if err := json.Unmarshal(runner.input, &request); err != nil {
		t.Fatal(err)
	}
	if request.SchemaVersion != 1 || request.Target.CWD != "/repo" {
		t.Fatalf("request = %+v", request)
	}
	if !strings.Contains(string(runner.input), `"pane_id":"w1:p1"`) || strings.Contains(string(runner.input), `"PaneID"`) {
		t.Fatalf("request does not use the documented field names: %s", runner.input)
	}
}

func TestCollectRejectsSessionMismatch(t *testing.T) {
	runner := &fakeRunner{stdout: []byte(`{
      "schema_version":1,"harness":"aider","session_id":"other","source":"test","confidence":"exact",
      "collected_at":"2026-01-01T00:00:00Z",
      "tokens":{"fresh_input":1,"cache_read":0,"cache_write":0,"output":1,"reasoning":0,"total":2}
    }`)}
	item, _ := New(Config{SchemaVersion: 1, Collectors: map[string]Command{"aider": {Command: []string{"collector"}}}}, WithRunner(runner))
	_, err := item.Collect(context.Background(), collector.Target{Harness: "aider", SessionID: "expected", Confidence: usage.ConfidenceExact})
	if !errors.Is(err, collector.ErrMalformed) {
		t.Fatalf("Collect() error = %v, want ErrMalformed", err)
	}
}

func TestNewCanonicalizesHarnesses(t *testing.T) {
	item, err := New(Config{SchemaVersion: 1, Collectors: map[string]Command{
		"claude-code": {Command: []string{"custom-claude"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := item.Harnesses(); !reflect.DeepEqual(got, []string{"claude"}) {
		t.Fatalf("Harnesses() = %v", got)
	}
}

func TestMatchProcessDiscoversConfiguredHarness(t *testing.T) {
	item, err := New(Config{SchemaVersion: 1, Collectors: map[string]Command{
		"aider": {Command: []string{"collector"}, ProcessNames: []string{"aider", "/opt/bin/aider-chat"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	harness, err := item.MatchProcess([]collector.Process{{Name: "python3", Argv0: "/opt/bin/aider-chat"}})
	if err != nil || harness != "aider" {
		t.Fatalf("MatchProcess() = %q, %v", harness, err)
	}
}

func TestNewRejectsAmbiguousProcessDetection(t *testing.T) {
	_, err := New(Config{SchemaVersion: 1, Collectors: map[string]Command{
		"aider": {Command: []string{"a"}, ProcessNames: []string{"agent"}},
		"pi":    {Command: []string{"p"}, ProcessNames: []string{"agent"}},
	}})
	if err == nil {
		t.Fatal("New() error = nil, want ambiguous detector error")
	}
}

func TestCollectIncludesBoundedStderr(t *testing.T) {
	runner := &fakeRunner{stderr: []byte("configuration missing"), err: errors.New("exit status 2")}
	item, _ := New(Config{SchemaVersion: 1, Collectors: map[string]Command{"aider": {Command: []string{"collector"}}}}, WithRunner(runner))
	_, err := item.Collect(context.Background(), collector.Target{Harness: "aider"})
	if err == nil || err.Error() != "external aider collector failed: exit status 2: configuration missing" {
		t.Fatalf("Collect() error = %v", err)
	}
}

func TestCollectRejectsTrailingJSONValue(t *testing.T) {
	runner := &fakeRunner{stdout: []byte(`{"schema_version":1,"tokens":{"total":0}} {}`)}
	item, _ := New(Config{SchemaVersion: 1, Collectors: map[string]Command{"aider": {Command: []string{"collector"}}}}, WithRunner(runner))
	_, err := item.Collect(context.Background(), collector.Target{Harness: "aider", SessionID: "one", Confidence: usage.ConfidenceExact})
	if !errors.Is(err, collector.ErrMalformed) {
		t.Fatalf("Collect() error = %v, want ErrMalformed", err)
	}
}

func TestAllowedEnvironmentDoesNotForwardCredentials(t *testing.T) {
	t.Setenv("PATH", "/safe/bin")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-leak")
	environment := strings.Join(allowedEnvironment(), "\n")
	if !strings.Contains(environment, "PATH=/safe/bin") {
		t.Fatalf("allowed environment omitted PATH: %q", environment)
	}
	if strings.Contains(environment, "ANTHROPIC_API_KEY") || strings.Contains(environment, "must-not-leak") {
		t.Fatalf("allowed environment leaked a credential: %q", environment)
	}
}

func TestExecRunnerBoundsDescendantsHoldingOutputPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err := (ExecRunner{}).Run(ctx, []string{"/bin/sh", "-c", "sleep 30 & exit 0"}, nil)
	if err == nil {
		t.Fatal("ExecRunner.Run() error = nil, want bounded descendant failure")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("ExecRunner.Run() took %v, want process-tree timeout under 3s", elapsed)
	}
}
