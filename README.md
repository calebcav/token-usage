# Token Usage for Herdr

Token Usage gives every active coding-agent pane a consistent, local token count and a fast popup dashboard. It ships adapters for Codex, Claude Code, and OpenCode, plus a small JSON subprocess contract for other harnesses.

It intentionally does one job: current-session token accounting. It does not call provider APIs, read credentials, estimate prices, track account quotas, or build usage history.

## What you get

- Compact Herdr sidebar tokens: `$usage`, `$context`, `$model`, `$harness`, and `$confidence`.
- A responsive Bubble Tea popup with per-session context bars, normalized token breakdowns, keyboard navigation, and five-second refreshes.
- Exact Herdr-native session matching when available, with cautious and visibly `estimated` cwd fallback.
- Local-only collectors that never persist prompts, completions, tool calls, transcript paths, or credentials.
- A versioned external collector contract for Aider, Pi, custom wrappers, and future harnesses.

## Requirements

- [Herdr](https://github.com/herdrdev/herdr) 0.8.2 or newer. The plugin needs session snapshots, popup panes, and custom metadata tokens.
- Go 1.26.1 or newer when installing from source.
- At least one supported coding harness with a local session store.

## Install from GitHub

```sh
herdr plugin install calebcav/token-usage
```

Continue with the Herdr configuration below. You can open the dashboard without a keybinding at any time with `herdr plugin action invoke token-usage.open`.

## Install from a local checkout

```sh
go build -trimpath -ldflags "-s -w -X main.version=0.1.0" -o bin/token-usage ./cmd/token-usage
herdr plugin link .
./bin/token-usage setup
```

`plugin link` deliberately does not run manifest build commands, so build the binary first. GitHub installation runs the declared Go build automatically.

## Configure Herdr

Add the metadata tokens to the Agent sidebar layout in `~/.config/herdr/config.toml`. The `rows` setting replaces the complete layout, so merge the `$` fields into any rows you already customized:

```toml
[ui.sidebar.agents]
rows = [
  ["state_icon", "workspace", "tab"],
  ["agent", "state_text"],
  ["$usage", "$context"],
  ["$model"],
]
```

Optionally bind the popup dashboard to `prefix+u` (`Ctrl+B`, then `U` with Herdr's default prefix):

```toml
[[keys.command]]
key = "prefix+u"
type = "plugin_action"
command = "token-usage.open"
description = "token usage dashboard"
```

Then reload Herdr:

```sh
herdr server reload-config
```

Install Herdr's official harness integrations to make native session IDs available for exact matching:

```sh
herdr integration install claude
herdr integration install codex
herdr integration install opencode
herdr integration status
```

Install the integration before starting the corresponding harness. OpenCode loads its Herdr plugin at process startup, so after installing it, exit and relaunch any already-running OpenCode processes (or recreate their Herdr panes). Relaunching to OpenCode's `Ask anything...` home screen is not enough: continue an existing root session, or send the first prompt to create one, and wait a moment for Herdr to receive its session ID. Then republish usage metadata:

```sh
herdr plugin action invoke token-usage.refresh
```

If an OpenCode row reports `session not found: OpenCode directory fallback`, run `herdr integration status` first. The message normally means Herdr has no native OpenCode session ID and the plugin refused to guess from old or ambiguous cwd matches. Install the integration, restart OpenCode, enter a root session, and refresh. If OpenCode is still on its home screen, `unavailable` is expected because there is no current session to measure. Widening the fallback window is not recommended because it can associate usage with the wrong pane.

You can also invoke the actions directly:

```sh
herdr plugin action invoke token-usage.open
herdr plugin action invoke token-usage.refresh
```

## Usage

```text
token-usage dashboard       Interactive popup UI
token-usage status          One-shot terminal table
token-usage status --json   Stable machine-readable result
token-usage refresh         Republish sidebar metadata
token-usage setup           Print Herdr configuration
token-usage contract        Print the external adapter contract
```

Herdr automatically republishes metadata after startup, when an agent is detected, when a pane is focused, and when an agent settles. Working-state events are skipped; the dashboard can still refresh a working session on demand.

Context bars use the harness's reported live context occupancy, not cumulative token totals. Codex and external collectors can provide that value today; when a harness does not expose a trustworthy context window, the dashboard says `not reported` instead of inventing a percentage. Bars remain readable without color and change from green to amber at 70%, then red at 90%.

## Normalized token semantics

Every adapter returns the same additive breakdown:

```text
fresh input + cache read + cache write + output = total
```

Reasoning is an informational subset of output and is never added to total a second time.

| Harness | Local source | Normalization |
| --- | --- | --- |
| Codex | `CODEX_HOME/sessions` rollout JSONL | Uses the final complete cumulative token event; cumulative events are never summed. |
| Claude Code | `CLAUDE_CONFIG_DIR/projects` transcript JSONL | Sums unique assistant messages, deduplicating repeated message IDs and all cache-creation components; repeated refreshes parse only newly appended complete records. |
| OpenCode | Local `opencode.db` SQLite store | Adds OpenCode's raw input, cache, output, and reasoning counters; the database is opened read-only. |

An exact native session ID always wins. If Herdr has no native session reference, fallback only succeeds when a recent cwd match is unambiguous, and the result is labeled `estimated`. The plugin would rather show “unavailable” than attach another agent's tokens to the wrong pane.

## Add another harness

Create `collectors.json` in the directory printed by `token-usage setup`:

```json
{
  "schema_version": 1,
  "collectors": {
    "aider": {
      "command": ["/absolute/path/to/aider-usage", "--json"],
      "process_names": ["aider"]
    }
  }
}
```

`process_names` lets the plugin recognize a harness that Herdr does not detect itself by inspecting each unclaimed pane's local foreground process. Omit it when another Herdr integration already reports the agent. The command receives a versioned target as JSON on stdin and returns one normalized snapshot on stdout. Run `token-usage contract` for a complete example, or see [the external collector guide](docs/external-collectors.md).

## Privacy and safety

All reads and computation stay on the machine. Collectors decode only identifiers, cwd metadata, model/provider labels, timestamps, and numeric usage fields. User prompts, assistant text, and tool payloads are ignored. Errors and snapshots expose a safe source kind such as `codex-rollout`, never a transcript or database path.

External collectors are ordinary local programs configured by you. They receive pane/session metadata including cwd, so only configure commands you trust. Output is capped at 1 MiB and each command has a five-second timeout.

## Development

```sh
make check
go test -race ./...
```

The fixture suite covers cumulative-vs-delta semantics, duplicate records, malformed counters, partial trailing JSONL, ambiguous matching, SQLite schema drift, metadata publication, and the external process boundary. See [the architecture notes](docs/architecture.md) for the package layout and invariants.

## Scope

Version 0.1 reports active local root sessions only. Claude subagent transcripts, Codex child rollouts, and OpenCode child-session rows are not folded into the parent total yet. Provider billing, subscription quotas, historical charts, remote sessions, and automatic Herdr config mutation are intentionally out of scope.

## License

MIT
