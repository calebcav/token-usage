package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const (
	DefaultBinary  = "herdr"
	MetadataSource = "plugin:token-usage"
)

// Runner is the narrow process boundary used by Client. Tests can replace it
// without requiring a running Herdr server.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, []byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type Client struct {
	Binary string
	Runner Runner
}

func NewClient() *Client {
	binary := strings.TrimSpace(os.Getenv("HERDR_BIN_PATH"))
	if binary == "" {
		binary = DefaultBinary
	}
	return &Client{Binary: binary, Runner: ExecRunner{}}
}

type SessionRef struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

// Record contains the subset shared by PaneInfo and AgentInfo that the plugin
// needs. Unknown fields are intentionally ignored so the client stays tolerant
// of additive protocol changes.
type Record struct {
	PaneID          string            `json:"pane_id"`
	WorkspaceID     string            `json:"workspace_id"`
	TabID           string            `json:"tab_id"`
	Agent           string            `json:"agent"`
	AgentName       string            `json:"agent_name"`
	Status          string            `json:"status"`
	AgentStatus     string            `json:"agent_status"`
	CWD             string            `json:"cwd"`
	ForegroundCWD   string            `json:"foreground_cwd"`
	Focused         bool              `json:"focused"`
	Tokens          map[string]string `json:"tokens"`
	AgentSession    *SessionRef       `json:"agent_session"`
	LifecycleSource string            `json:"agent_source"`
}

type Snapshot struct {
	Version            string   `json:"version"`
	Protocol           int      `json:"protocol"`
	ProtocolVersion    int      `json:"protocol_version"`
	FocusedWorkspaceID string   `json:"focused_workspace_id"`
	FocusedTabID       string   `json:"focused_tab_id"`
	FocusedPaneID      string   `json:"focused_pane_id"`
	Panes              []Record `json:"panes"`
	Agents             []Record `json:"agents"`
}

func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	stdout, stderr, err := c.run(ctx, "api", "snapshot")
	if err != nil {
		return Snapshot{}, commandError("read Herdr session snapshot", stderr, err)
	}

	snapshot, err := decodeSnapshot(stdout)
	if err != nil {
		return Snapshot{}, fmt.Errorf("decode Herdr session snapshot: %w", err)
	}
	return snapshot, nil
}

func decodeSnapshot(data []byte) (Snapshot, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Snapshot{}, errors.New("empty response")
	}

	var envelope struct {
		Result struct {
			Snapshot json.RawMessage `json:"snapshot"`
		} `json:"result"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Snapshot{}, err
	}

	payload := data
	if len(envelope.Result.Snapshot) != 0 && string(envelope.Result.Snapshot) != "null" {
		payload = envelope.Result.Snapshot
	} else if len(envelope.Snapshot) != 0 && string(envelope.Snapshot) != "null" {
		payload = envelope.Snapshot
	}

	var snapshot Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return Snapshot{}, err
	}
	if snapshot.Protocol == 0 {
		snapshot.Protocol = snapshot.ProtocolVersion
	}
	if snapshot.Version == "" && snapshot.Protocol == 0 && snapshot.Panes == nil && snapshot.Agents == nil {
		return Snapshot{}, errors.New("response does not contain a session snapshot")
	}
	return snapshot, nil
}

// Targets converts Herdr's runtime records into collector targets. AgentInfo is
// authoritative; pane records only fill fields that older snapshots omit. If a
// native session reference is unavailable, the target is explicitly estimated.
func (s Snapshot) Targets() []collector.Target {
	panes := make(map[string]Record, len(s.Panes))
	for _, pane := range s.Panes {
		if pane.PaneID != "" {
			panes[pane.PaneID] = pane
		}
	}

	records := s.Agents
	if len(records) == 0 {
		records = s.Panes
	}

	byPane := make(map[string]collector.Target, len(records))
	for _, record := range records {
		if fallback, ok := panes[record.PaneID]; ok {
			record = mergeRecord(record, fallback)
		}

		harness := firstNonEmpty(record.Agent, record.AgentName)
		if record.AgentSession != nil {
			harness = firstNonEmpty(record.AgentSession.Agent, harness)
		}
		harness = collector.CanonicalHarness(harness)
		if strings.TrimSpace(record.PaneID) == "" || harness == "" {
			continue
		}

		target := collector.Target{
			PaneID:      record.PaneID,
			WorkspaceID: record.WorkspaceID,
			Harness:     harness,
			CWD:         firstNonEmpty(record.ForegroundCWD, record.CWD),
			State:       firstNonEmpty(record.AgentStatus, record.Status),
			Confidence:  usage.ConfidenceEstimated,
		}
		if record.AgentSession != nil && strings.TrimSpace(record.AgentSession.Value) != "" {
			target.SessionID = strings.TrimSpace(record.AgentSession.Value)
			target.SessionRef = &collector.SessionRef{
				Source: record.AgentSession.Source,
				Agent:  record.AgentSession.Agent,
				Kind:   record.AgentSession.Kind,
				Value:  strings.TrimSpace(record.AgentSession.Value),
			}
			target.Confidence = usage.ConfidenceExact
		}
		byPane[target.PaneID] = target
	}

	targets := make([]collector.Target, 0, len(byPane))
	for _, target := range byPane {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].WorkspaceID != targets[j].WorkspaceID {
			return targets[i].WorkspaceID < targets[j].WorkspaceID
		}
		return targets[i].PaneID < targets[j].PaneID
	})
	return targets
}

func mergeRecord(primary, fallback Record) Record {
	primary.PaneID = firstNonEmpty(primary.PaneID, fallback.PaneID)
	primary.WorkspaceID = firstNonEmpty(primary.WorkspaceID, fallback.WorkspaceID)
	primary.TabID = firstNonEmpty(primary.TabID, fallback.TabID)
	primary.Agent = firstNonEmpty(primary.Agent, fallback.Agent)
	primary.AgentName = firstNonEmpty(primary.AgentName, fallback.AgentName)
	primary.Status = firstNonEmpty(primary.Status, fallback.Status)
	primary.AgentStatus = firstNonEmpty(primary.AgentStatus, fallback.AgentStatus)
	primary.CWD = firstNonEmpty(primary.CWD, fallback.CWD)
	primary.ForegroundCWD = firstNonEmpty(primary.ForegroundCWD, fallback.ForegroundCWD)
	if primary.AgentSession == nil {
		primary.AgentSession = fallback.AgentSession
	}
	return primary
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

type Metadata struct {
	Tokens map[string]*string
	TTL    time.Duration
	Seq    uint64
}

type ProcessInfo struct {
	PaneID              string        `json:"pane_id"`
	ForegroundProcesses []ProcessItem `json:"foreground_processes"`
}

type ProcessItem struct {
	Name  string   `json:"name"`
	Argv0 string   `json:"argv0"`
	Argv  []string `json:"argv"`
	CWD   string   `json:"cwd"`
}

func (c *Client) ProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error) {
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		return ProcessInfo{}, errors.New("pane id is required")
	}
	stdout, stderr, err := c.run(ctx, "pane", "process-info", "--pane", paneID)
	if err != nil {
		return ProcessInfo{}, commandError("read Herdr pane process info", stderr, err)
	}
	var envelope struct {
		Result struct {
			ProcessInfo json.RawMessage `json:"process_info"`
		} `json:"result"`
		ProcessInfo json.RawMessage `json:"process_info"`
	}
	if err := json.Unmarshal(stdout, &envelope); err != nil {
		return ProcessInfo{}, fmt.Errorf("decode Herdr pane process info: %w", err)
	}
	payload := stdout
	if len(envelope.Result.ProcessInfo) > 0 && string(envelope.Result.ProcessInfo) != "null" {
		payload = envelope.Result.ProcessInfo
	} else if len(envelope.ProcessInfo) > 0 && string(envelope.ProcessInfo) != "null" {
		payload = envelope.ProcessInfo
	}
	var info ProcessInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return ProcessInfo{}, fmt.Errorf("decode Herdr pane process info: %w", err)
	}
	if info.PaneID == "" {
		return ProcessInfo{}, errors.New("decode Herdr pane process info: missing pane id")
	}
	return info, nil
}

func (c *Client) ReportMetadata(ctx context.Context, paneID string, metadata Metadata) error {
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		return errors.New("pane id is required")
	}
	if len(metadata.Tokens) > 16 {
		return errors.New("herdr accepts at most 16 metadata tokens per report")
	}

	keys := make([]string, 0, len(metadata.Tokens))
	for key := range metadata.Tokens {
		if !validTokenName(key) {
			return fmt.Errorf("invalid metadata token name %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	args := []string{"pane", "report-metadata", paneID, "--source", MetadataSource}
	for _, key := range keys {
		value := ""
		if metadata.Tokens[key] != nil {
			value = *metadata.Tokens[key]
		}
		args = append(args, "--token", key+"="+value)
	}
	if metadata.TTL > 0 {
		args = append(args, "--ttl-ms", strconv.FormatInt(metadata.TTL.Milliseconds(), 10))
	}
	if metadata.Seq > 0 {
		args = append(args, "--seq", strconv.FormatUint(metadata.Seq, 10))
	}

	_, stderr, err := c.run(ctx, args...)
	if err != nil {
		return commandError("report Herdr pane metadata", stderr, err)
	}
	return nil
}

func validTokenName(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func (c *Client) OpenDashboard(ctx context.Context) error {
	_, stderr, err := c.run(ctx,
		"plugin", "pane", "open",
		"--plugin", "token-usage",
		"--entrypoint", "dashboard",
		"--focus",
	)
	if err != nil {
		return commandError("open token usage dashboard", stderr, err)
	}
	return nil
}

func (c *Client) FocusAgent(ctx context.Context, target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return errors.New("pane id is required")
	}
	_, stderr, err := c.run(ctx, "agent", "focus", target)
	if err != nil {
		return commandError("focus coding-agent pane", stderr, err)
	}
	return nil
}

func (c *Client) PluginConfigDir(ctx context.Context, pluginID string) (string, error) {
	pluginID = strings.TrimSpace(pluginID)
	if pluginID == "" {
		return "", errors.New("plugin id is required")
	}
	stdout, stderr, err := c.run(ctx, "plugin", "config-dir", pluginID)
	if err != nil {
		return "", commandError("resolve Herdr plugin config directory", stderr, err)
	}
	directory := strings.TrimSpace(string(stdout))
	if directory == "" {
		return "", errors.New("resolve Herdr plugin config directory: empty response")
	}
	if strings.ContainsAny(directory, "\r\n") {
		return "", errors.New("resolve Herdr plugin config directory: malformed response")
	}
	return directory, nil
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if c == nil {
		return nil, nil, errors.New("herdr client is nil")
	}
	binary := strings.TrimSpace(c.Binary)
	if binary == "" {
		binary = DefaultBinary
	}
	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	return runner.Run(ctx, binary, args...)
}

func commandError(action string, stderr []byte, err error) error {
	detail := usage.SanitizeText(string(stderr), 512)
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, detail)
}
