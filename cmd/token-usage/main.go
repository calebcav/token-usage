package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/calebcav/token-usage/internal/app"
	"github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/collector/claude"
	"github.com/calebcav/token-usage/internal/collector/codex"
	"github.com/calebcav/token-usage/internal/collector/external"
	"github.com/calebcav/token-usage/internal/collector/opencode"
	"github.com/calebcav/token-usage/internal/collector/pi"
	"github.com/calebcav/token-usage/internal/herdr"
	"github.com/calebcav/token-usage/internal/sequence"
	"github.com/calebcav/token-usage/internal/ui"
	"github.com/calebcav/token-usage/internal/usage"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "token-usage:", usage.SanitizeText(err.Error(), 512))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	command := "dashboard"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}

	switch command {
	case "dashboard":
		service, err := newService(ctx)
		if err != nil {
			return err
		}
		return ui.Run(ctx, service)
	case "open", "open-dashboard":
		commandCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return herdr.NewClient().OpenDashboard(commandCtx)
	case "refresh", "restore":
		service, err := newService(ctx)
		if err != nil {
			return err
		}
		commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		results, err := service.Collect(commandCtx, app.CollectOptions{Publish: true, IncludeWorking: true})
		if err != nil {
			return err
		}
		if command == "refresh" {
			printRefreshSummary(stdout, results)
		}
		printWarnings(stderr, results)
		return nil
	case "event":
		return runEvent(ctx, stderr)
	case "claude-statusline":
		return claude.CaptureStatusLine(os.Stdin, stdout)
	case "status":
		return runStatus(ctx, args, stdout, stderr)
	case "setup":
		printSetup(stdout, resolveExternalConfigPath(ctx), executablePath())
		return nil
	case "contract", "external-contract":
		printExternalContract(stdout)
		return nil
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version)
		return nil
	case "help", "--help", "-h":
		printHelp(stdout)
		return nil
	default:
		printHelp(stderr)
		return fmt.Errorf("unknown command %q", command)
	}
}

func newService(ctx context.Context) (*app.Service, error) {
	collectors := []collector.Collector{
		codex.New(codex.Config{AllowCWDRecencyFallback: true, EnableAccountLimits: true}),
		claude.New(),
		opencode.New(opencode.Config{AllowDirectoryFallback: true}),
		pi.New(pi.Config{AllowCWDRecencyFallback: true}),
	}

	configPath := resolveExternalConfigPath(ctx)
	if _, err := os.Stat(configPath); err == nil {
		custom, loadErr := external.Load(configPath)
		if loadErr != nil {
			return nil, fmt.Errorf("load external collectors: %w", loadErr)
		}
		if len(custom.Harnesses()) > 0 {
			collectors = append(collectors, custom)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect external collector config: %w", err)
	}

	registry, err := collector.NewRegistry(collectors...)
	if err != nil {
		return nil, err
	}
	service := &app.Service{Registry: registry, Herdr: herdr.NewClient()}
	if stateDir := strings.TrimSpace(os.Getenv("HERDR_PLUGIN_STATE_DIR")); stateDir != "" {
		counter := sequence.Counter{Path: filepath.Join(stateDir, "metadata-seq")}
		service.NextSequence = func() uint64 {
			value, err := counter.Next()
			if err != nil {
				return sequence.MemoryNext()
			}
			return value
		}
	}
	return service, nil
}

func runEvent(ctx context.Context, stderr io.Writer) error {
	eventName := strings.TrimSpace(os.Getenv("HERDR_PLUGIN_EVENT"))
	paneID, status := parseEvent(os.Getenv("HERDR_PLUGIN_EVENT_JSON"))
	if eventName == "pane.agent_status_changed" && strings.EqualFold(status, "working") {
		return nil
	}
	service, err := newService(ctx)
	if err != nil {
		return err
	}
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	options := app.CollectOptions{
		PaneID:         paneID,
		Publish:        true,
		IncludeWorking: eventName != "pane.agent_status_changed",
	}
	results, err := service.Collect(commandCtx, options)
	if err != nil {
		return err
	}
	printWarnings(stderr, results)
	if eventName == "pane.agent_status_changed" {
		// Harness hooks and databases can flush just after Herdr observes the
		// settled state. Two bounded reconciliations avoid leaving the sidebar
		// on the preceding complete JSONL record until another focus event.
		for _, delay := range []time.Duration{400 * time.Millisecond, 1100 * time.Millisecond} {
			timer := time.NewTimer(delay)
			select {
			case <-commandCtx.Done():
				timer.Stop()
				return commandCtx.Err()
			case <-timer.C:
			}
			results, err = service.Collect(commandCtx, options)
			if err != nil {
				return err
			}
			printWarnings(stderr, results)
		}
	}
	return nil
}

func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "emit machine-readable JSON")
	includeWorking := flags.Bool("working", true, "include sessions that are currently working")
	if err := flags.Parse(args); err != nil {
		return err
	}
	service, err := newService(ctx)
	if err != nil {
		return err
	}
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	results, err := service.Collect(commandCtx, app.CollectOptions{IncludeWorking: *includeWorking})
	if err != nil {
		return err
	}
	results = app.UniqueSessions(results)
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(struct {
			SchemaVersion int          `json:"schema_version"`
			Results       []app.Result `json:"results"`
		}{SchemaVersion: usage.SchemaVersion, Results: results})
	}
	printStatus(stdout, results)
	return nil
}

func printStatus(output io.Writer, results []app.Result) {
	if len(results) == 0 {
		fmt.Fprintln(output, "No active coding-harness sessions found.")
		return
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "HARNESS\tPANE\tMODEL\tSPENT\tCONTEXT\tLIMIT\tSTATE\tMATCH")
	for _, result := range results {
		if result.Snapshot == nil {
			fmt.Fprintf(writer, "%s\t%s\t—\t—\tnot reported\tnot reported\t%s\t%s\n",
				usage.SanitizeText(result.Target.Harness, 80),
				usage.SanitizeText(result.Target.PaneID, 80),
				usage.SanitizeText(result.Target.State, 80),
				usage.SanitizeText(result.Error, 512))
			continue
		}
		snapshot := result.Snapshot
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			snapshot.Harness,
			snapshot.PaneID,
			defaultText(snapshot.Model, "—"),
			usage.FormatCount(snapshot.Tokens.Spent()),
			defaultText(snapshot.CompactContext(), "not reported"),
			defaultText(snapshot.CompactLimit(), "not reported"),
			defaultText(snapshot.State, "unknown"),
			snapshot.Confidence,
		)
	}
	_ = writer.Flush()
}

func printRefreshSummary(output io.Writer, results []app.Result) {
	var refreshed, unavailable int
	for _, result := range results {
		if result.Snapshot != nil && result.PublishErr == "" {
			refreshed++
		} else {
			unavailable++
		}
	}
	fmt.Fprintf(output, "Refreshed %d pane%s", refreshed, plural(refreshed))
	if unavailable > 0 {
		fmt.Fprintf(output, "; %d unavailable", unavailable)
	}
	fmt.Fprintln(output, ".")
}

func printWarnings(output io.Writer, results []app.Result) {
	for _, result := range results {
		if err := result.Err(); err != nil {
			fmt.Fprintf(output, "%s (%s): %s\n",
				usage.SanitizeText(result.Target.Harness, 80),
				usage.SanitizeText(result.Target.PaneID, 80),
				usage.SanitizeText(err.Error(), 512))
		}
	}
}

func parseEvent(raw string) (paneID, status string) {
	if strings.TrimSpace(raw) == "" {
		return "", ""
	}
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return "", ""
	}
	return findEventString(value, "pane_id"), firstNonEmpty(
		findEventString(value, "agent_status"),
		findEventString(value, "status"),
	)
}

func findEventString(value any, key string) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if direct, ok := object[key].(string); ok && strings.TrimSpace(direct) != "" {
		return strings.TrimSpace(direct)
	}
	for _, container := range []string{"data", "payload", "event", "pane", "agent"} {
		if nested := findEventString(object[container], key); nested != "" {
			return nested
		}
	}
	return ""
}

func resolveExternalConfigPath(ctx context.Context) string {
	if strings.TrimSpace(os.Getenv("HERDR_PLUGIN_CONFIG_DIR")) != "" {
		return external.DefaultConfigPath()
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if directory, err := herdr.NewClient().PluginConfigDir(lookupCtx, "token-usage"); err == nil {
		return filepath.Join(directory, "collectors.json")
	}
	return external.DefaultConfigPath()
}

func printSetup(output io.Writer, configPath, executable string) {
	statusCommand, _ := json.Marshal(shellWord(executable) + " claude-statusline")
	fmt.Fprintf(output, `Token Usage reads privacy-filtered harness usage and publishes compact metadata to Herdr.

1. Add the metadata tokens to your Agent sidebar layout in ~/.config/herdr/config.toml.
   The rows setting replaces the whole layout, so merge these $ fields into any rows you already customized:

[ui.sidebar.agents]
rows = [
  ["state_icon", "workspace", "tab"],
  ["agent", "state_text"],
  ["$usage", "$context"],
  ["$limit", "$model"],
]

For exact native session matching, make sure Herdr's harness integrations are installed:

herdr integration install claude
herdr integration install codex
herdr integration install opencode
herdr integration status

2. To let Claude report context and account limits, merge this into ~/.claude/settings.json.
   Use the absolute executable path if token-usage is not on PATH:

{
  "statusLine": {
    "type": "command",
    "command": %s,
    "refreshInterval": 60
  }
}

Claude reports context and account limits after the session receives its first API response.
Until then, "not reported" is expected.

3. Optional: bind the popup dashboard:

[[keys.command]]
key = "prefix+u"
type = "plugin_action"
command = "token-usage.open"
description = "token usage dashboard"

4. Reload Herdr after editing config:

herdr server reload-config

External collectors can be declared at:
%s

Run "token-usage contract" for the JSON protocol.
`, statusCommand, configPath)
}

func printExternalContract(output io.Writer) {
	fmt.Fprintln(output, `External collector contract (schema_version 2)

Create collectors.json:
{
  "schema_version": 2,
  "collectors": {
    "aider": {
      "command": ["/absolute/path/to/aider-usage", "--json"],
      "process_names": ["aider"]
    }
  }
}

The command receives this JSON on stdin:
{
  "schema_version": 2,
  "target": {
    "pane_id": "w1:p1",
    "workspace_id": "w1",
    "harness": "aider",
    "session_id": "native-session-id",
    "session_ref": {
      "source": "custom:aider",
      "agent": "aider",
      "kind": "id",
      "value": "native-session-id"
    },
    "cwd": "/workspace",
    "state": "idle",
    "confidence": "exact"
  }
}

It returns one UsageSnapshot. Reasoning is a subset of output, not an extra addend:
{
  "schema_version": 2,
  "harness": "aider",
  "session_id": "native-session-id",
  "model": "model-name",
  "tokens": {
    "fresh_input": 100,
    "cache_read": 20,
    "cache_write": 0,
    "output": 30,
    "reasoning": 5,
    "total": 150
  },
  "context": {"used": 150, "limit": 128000},
  "quota": {
    "windows": [
      {"label": "5h", "used_percent": 42, "resets_at": "2026-08-30T16:00:00Z"},
      {"label": "7d", "used_percent": 73}
    ],
    "source": "aider-provider-limits",
    "collected_at": "2026-08-30T12:00:00Z"
  },
  "source": "aider-local-log",
  "confidence": "exact",
  "collected_at": "2026-08-30T12:00:00Z"
}`)
}

func printHelp(output io.Writer) {
	fmt.Fprintln(output, `Usage: token-usage [command]

Commands:
  dashboard           open the interactive local dashboard (default)
  open                ask Herdr to open the plugin popup
  refresh             refresh sidebar metadata for active panes
  restore             republish metadata after Herdr starts
  event               refresh from a Herdr plugin event hook
  claude-statusline   capture Claude context and account limits
  status [--json]     print a one-shot usage report
  setup               print sidebar and keybinding configuration
  contract            print the external collector JSON contract
  version             print the plugin version
  help                show this help`)
}

func defaultText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func plural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func executablePath() string {
	path, err := os.Executable()
	if err != nil || strings.TrimSpace(path) == "" {
		return "token-usage"
	}
	return filepath.Clean(path)
}

func shellWord(value string) string {
	if value == "" {
		return "token-usage"
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("/_+.,:@%-=", char) {
			continue
		}
		return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	return value
}
