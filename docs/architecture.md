# Architecture

The plugin has four boundaries:

```text
Herdr session snapshot
        │
        ▼
 pane/session targets ──► harness collectors ──► UsageSnapshot v2
        │                                            │
        └────────────────────────────────────────────┤
                                                     ├─► sidebar metadata
                                                     ├─► popup dashboard
                                                     └─► status JSON/table
```

## Packages

- `internal/herdr` is the only Herdr CLI boundary. It accepts wrapped or bare session snapshots, derives collection targets, opens the popup, and publishes metadata patches.
- `internal/collector` defines the target interface and harness registry.
- `internal/collector/{codex,claude,opencode,pi}` contains source-specific resolution and normalization.
- `internal/collector/external` implements the versioned local subprocess contract.
- `internal/usage` owns the normalized snapshot and its arithmetic invariants.
- `internal/app` coordinates bounded parallel collection, metadata publication, and session deduplication.
- `internal/ui` renders the Bubble Tea dashboard. It never parses a harness source directly.
- `cmd/token-usage` wires the plugin actions, startup/event hooks, status output, and setup guidance.

## Matching invariant

The Herdr native `agent_session` reference is the primary key. Its typed `{source, agent, kind, value}` form is preserved for external collectors, while built-ins use the compatibility `session_id` value. Collectors search for that exact value and reject duplicate matches. A collector may fall back to cwd and recency only when no reference exists and the match is unique. Fallback snapshots must carry `confidence: "estimated"`; an external collector cannot upgrade an estimated target to exact.

External collectors can optionally declare foreground process names. The service queries `pane process-info` only for otherwise-unclaimed panes and creates an estimated target when exactly one configured harness matches.

## Accounting invariant

`Tokens` is deliberately non-overlapping:

```text
Total = FreshInput + CacheRead + CacheWrite + Output
Spent = FreshInput + Output
Reasoning <= Output
```

Source-specific inclusive/subset fields are normalized before a snapshot crosses the collector boundary. Human-facing headline values use `Spent`, while `Total` preserves all processed tokens for the detailed breakdown and machine-readable snapshot. `Snapshot.Validate` rejects arithmetic drift, impossible context windows, unknown confidence values, missing source labels, and unsupported schema versions.

Context occupancy and account quotas are separate optional values. A context window has token `used` and `limit` counters. A quota snapshot has one or more labeled percentage windows, optional reset timestamps, collection time, and safe provenance. Renderers show `not reported` when either value is absent; they never derive an account quota from session token totals. `$limit` selects the highest-used reported quota window for compact metadata.

## Freshness and ordering

Herdr metadata is runtime-only, so the startup hook republishes it after session restoration. Focus and settled-status events keep the sidebar fresh. Settled events reconcile twice over the next 1.5 seconds so a harness's final file/database flush is captured. Runtime plugin processes allocate metadata sequence values through a file-locked counter under `HERDR_PLUGIN_STATE_DIR`; Herdr ignores a late result from an older process for the same pane/source.

The dashboard checks Herdr every five seconds and includes working sessions. Normalized per-pane snapshots use a 15-second file-cache TTL, which avoids repeatedly walking harness stores or opening databases when the source data was just collected. Startup, event, status, and explicit refresh paths bypass cache reads while warming the cache; `r` also forces a source refresh. Cache hits update live pane state but do not republish unchanged metadata. Entries are keyed by a hash of the harness/workspace/pane/cwd identity, stored with user-only permissions, rejected after quota reset, and contain neither cwd nor harness source content. Duplicate panes attached to the same harness/session are collapsed to the newest snapshot in the popup and one-shot status view, while sidebar metadata remains pane-specific.

Claude transcript state is cached in memory by path and byte offset. After a clean read, later refreshes parse only appended records; truncation or replacement falls back to a complete parse, and an unterminated final record is never committed to the cache.

Claude status-line state is privacy-filtered before it reaches disk. The generated configuration invokes the bridge on Claude's event-driven updates and every 60 seconds. A new session can initially provide only its identity; context and account windows remain absent until Claude receives its first API response. Once reported, context remains valid while the transcript is unchanged, and account percentages expire after five minutes or their window reset. Codex app-server quota state is keyed by `CODEX_HOME` and executable, expires after one minute or an earlier window reset, and is always best effort. OpenCode resolves model context with external plugins disabled, uses the session's authoritative directory, and caches only successful limits for ten minutes. Pi reads only JSONL session metadata and usage counters from the active branch, then resolves the selected model's context limit from Pi's offline model catalog.

Version 0.1 aggregates direct/root-session records. Claude subagent transcript directories, Codex child rollouts, and OpenCode child-session rows remain separate and are not added to their parent's total.

## Failure policy

A source error is attached to that pane instead of failing all collectors. The service clears plugin-owned tokens for an unavailable pane so an old number is not mistaken for current data. Herdr discovery failure is global and aborts the operation because no pane can be attributed safely.
