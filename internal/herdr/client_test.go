package herdr

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/calebcav/token-usage/internal/usage"
)

type fakeRunner struct {
	stdout []byte
	stderr []byte
	err    error
	name   string
	args   []string
}

func (runner *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	runner.name = name
	runner.args = append([]string(nil), args...)
	return runner.stdout, runner.stderr, runner.err
}

func TestSnapshotDecodesCLIEnvelope(t *testing.T) {
	runner := &fakeRunner{stdout: []byte(`{
      "id":"cli:api:snapshot",
      "result":{"type":"session_snapshot","snapshot":{
        "version":"0.8.2","protocol":1,"focused_pane_id":"w1:p1",
        "panes":[{"pane_id":"w1:p1","workspace_id":"w1","cwd":"/repo","foreground_cwd":"/repo/pkg"}],
        "agents":[{"pane_id":"w1:p1","workspace_id":"w1","agent":"codex","agent_status":"idle","agent_session":{"source":"herdr:codex","agent":"codex","kind":"id","value":"thread-1"}}]
      }}
    }`)}
	client := &Client{Binary: "/opt/herdr", Runner: runner}

	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.FocusedPaneID != "w1:p1" || snapshot.Protocol != 1 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	wantArgs := []string{"api", "snapshot"}
	if runner.name != "/opt/herdr" || !reflect.DeepEqual(runner.args, wantArgs) {
		t.Fatalf("command = %q %v, want %q %v", runner.name, runner.args, "/opt/herdr", wantArgs)
	}

	targets := snapshot.Targets()
	if len(targets) != 1 {
		t.Fatalf("Targets() len = %d, want 1", len(targets))
	}
	target := targets[0]
	if target.SessionID != "thread-1" || target.CWD != "/repo/pkg" || target.State != "idle" {
		t.Fatalf("unexpected target: %+v", target)
	}
	if target.SessionRef == nil || target.SessionRef.Kind != "id" || target.SessionRef.Value != "thread-1" {
		t.Fatalf("session ref = %+v", target.SessionRef)
	}
	if target.Confidence != usage.ConfidenceExact {
		t.Fatalf("confidence = %q, want exact", target.Confidence)
	}
}

func TestSnapshotAcceptsBarePayloadAndPaneFallback(t *testing.T) {
	runner := &fakeRunner{stdout: []byte(`{
      "version":"0.8.2","protocol_version":2,
      "panes":[{"pane_id":"w2:p3","workspace_id":"w2","agent":"claude-code","status":"working","cwd":"/repo"}]
    }`)}
	client := &Client{Runner: runner}

	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	targets := snapshot.Targets()
	if len(targets) != 1 {
		t.Fatalf("Targets() len = %d, want 1", len(targets))
	}
	if targets[0].Harness != "claude" || targets[0].Confidence != usage.ConfidenceEstimated {
		t.Fatalf("unexpected fallback target: %+v", targets[0])
	}
	if snapshot.Protocol != 2 {
		t.Fatalf("protocol = %d, want 2", snapshot.Protocol)
	}
}

func TestReportMetadataUsesStablePatchArguments(t *testing.T) {
	runner := &fakeRunner{}
	client := &Client{Binary: "herdr-test", Runner: runner}
	usageValue := "Σ 12.4k"
	modelValue := "gpt-5"

	err := client.ReportMetadata(context.Background(), "w1:p2", Metadata{
		Tokens: map[string]*string{
			"usage":   &usageValue,
			"context": nil,
			"model":   &modelValue,
		},
		TTL: time.Hour,
		Seq: 9,
	})
	if err != nil {
		t.Fatalf("ReportMetadata() error = %v", err)
	}
	want := []string{
		"pane", "report-metadata", "w1:p2", "--source", MetadataSource,
		"--token", "context=", "--token", "model=gpt-5", "--token", "usage=Σ 12.4k",
		"--ttl-ms", "3600000", "--seq", "9",
	}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestSnapshotCommandErrorIncludesHerdrDetail(t *testing.T) {
	runner := &fakeRunner{stderr: []byte("server unavailable"), err: errors.New("exit status 1")}
	client := &Client{Runner: runner}
	_, err := client.Snapshot(context.Background())
	if err == nil || err.Error() != "read Herdr session snapshot: exit status 1: server unavailable" {
		t.Fatalf("Snapshot() error = %v", err)
	}
}

func TestSnapshotCommandErrorSanitizesHerdrDetail(t *testing.T) {
	runner := &fakeRunner{stderr: []byte("bad\x1b[2J\nserver detail"), err: errors.New("exit status 1")}
	client := &Client{Runner: runner}
	_, err := client.Snapshot(context.Background())
	if err == nil || err.Error() != "read Herdr session snapshot: exit status 1: bad [2J server detail" {
		t.Fatalf("Snapshot() error = %q", err)
	}
}

func TestOpenDashboard(t *testing.T) {
	runner := &fakeRunner{}
	client := &Client{Runner: runner}
	if err := client.OpenDashboard(context.Background()); err != nil {
		t.Fatalf("OpenDashboard() error = %v", err)
	}
	want := []string{"plugin", "pane", "open", "--plugin", "token-usage", "--entrypoint", "dashboard", "--focus"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestFocusAgent(t *testing.T) {
	runner := &fakeRunner{}
	client := &Client{Binary: "herdr-test", Runner: runner}
	if err := client.FocusAgent(context.Background(), "w1:p2"); err != nil {
		t.Fatalf("FocusAgent() error = %v", err)
	}
	want := []string{"agent", "focus", "w1:p2"}
	if runner.name != "herdr-test" || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("command = %q %#v, want %q %#v", runner.name, runner.args, "herdr-test", want)
	}
}

func TestPluginConfigDir(t *testing.T) {
	runner := &fakeRunner{stdout: []byte("/tmp/herdr/token-usage\n")}
	client := &Client{Runner: runner}
	directory, err := client.PluginConfigDir(context.Background(), "token-usage")
	if err != nil {
		t.Fatalf("PluginConfigDir() error = %v", err)
	}
	if directory != "/tmp/herdr/token-usage" {
		t.Fatalf("PluginConfigDir() = %q", directory)
	}
	want := []string{"plugin", "config-dir", "token-usage"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestProcessInfo(t *testing.T) {
	runner := &fakeRunner{stdout: []byte(`{"id":"cli:pane:process_info","result":{"type":"pane_process_info","process_info":{"pane_id":"w1:p4","foreground_processes":[{"name":"aider","argv0":"/opt/bin/aider","argv":["aider"],"cwd":"/repo"}]}}}`)}
	client := &Client{Runner: runner}
	info, err := client.ProcessInfo(context.Background(), "w1:p4")
	if err != nil {
		t.Fatalf("ProcessInfo() error = %v", err)
	}
	if info.PaneID != "w1:p4" || len(info.ForegroundProcesses) != 1 || info.ForegroundProcesses[0].Name != "aider" {
		t.Fatalf("ProcessInfo() = %+v", info)
	}
	want := []string{"pane", "process-info", "--pane", "w1:p4"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestReportMetadataRejectsInvalidTokenName(t *testing.T) {
	client := &Client{Runner: &fakeRunner{}}
	value := "bad"
	err := client.ReportMetadata(context.Background(), "w1:p1", Metadata{Tokens: map[string]*string{"not valid": &value}})
	if err == nil {
		t.Fatal("ReportMetadata() error = nil, want validation error")
	}
}
