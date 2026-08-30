# External collectors

External collectors make another coding harness available without adding it to the Go binary. They are local subprocesses and use schema version 2. Version 1 collectors remain supported without quota fields.

## Configuration

Place `collectors.json` under `HERDR_PLUGIN_CONFIG_DIR`. Outside a plugin invocation, `token-usage setup` prints the fallback path.

```json
{
  "schema_version": 2,
  "collectors": {
    "aider": {
      "command": ["/absolute/path/to/aider-usage", "--json"],
      "process_names": ["aider"]
    }
  }
}
```

Commands are argv arrays; no shell expansion is applied. Use an absolute executable path when possible. A custom harness name must not collide with a built-in collector or another canonical alias.

`process_names` is optional. For panes that Herdr has not already identified as an agent, Token Usage compares these names with the basename of each foreground process's `name`, `argv0`, and first argv entry. Exactly one configured harness must match. This is enough for local tools such as Aider; a harness with wrapper processes or richer lifecycle needs can instead report its agent/session to Herdr and omit `process_names`.

## Request

The command receives one JSON object on stdin:

```json
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
```

No prompt, completion, or tool payload is provided. `session_ref` preserves Herdr's native source, agent, kind, and value; `session_id` is the compatibility copy of its value. Both can be absent for process-detected panes. If the command performs fallback matching, it must preserve `estimated` confidence.

## Response

Write exactly one `UsageSnapshot` JSON object to stdout:

```json
{
  "schema_version": 2,
  "harness": "aider",
  "session_id": "native-session-id",
  "model": "model-name",
  "provider": "provider-name",
  "tokens": {
    "fresh_input": 100,
    "cache_read": 20,
    "cache_write": 0,
    "output": 30,
    "reasoning": 5,
    "total": 150
  },
  "context": {
    "used": 150,
    "limit": 128000
  },
  "quota": {
    "windows": [
      {
        "label": "5h",
        "used_percent": 42,
        "resets_at": "2026-08-30T16:00:00Z"
      },
      {
        "label": "7d",
        "used_percent": 73
      }
    ],
    "source": "aider-provider-limits",
    "collected_at": "2026-08-30T12:00:00Z"
  },
  "source": "aider-local-log",
  "confidence": "exact",
  "collected_at": "2026-08-30T12:00:00Z"
}
```

The plugin can fill a missing harness, session ID, source, confidence, or collection time from the trusted request boundary. If supplied, harness and session ID must match the request. The token arithmetic must validate exactly, context limit must be nonzero, and reasoning cannot exceed output.

`quota` is optional in schema version 2. It contains one to sixteen uniquely labeled windows, a safe source label, and its own collection timestamp. `used_percent` is a finite percentage from 0 through 999 so spend-overage sources can report values above 100. `resets_at` is optional. Omit `quota` entirely when the harness cannot report account limits; do not estimate it from token totals.

An existing version 1 `collectors.json` continues to receive version 1 requests and must return version 1 snapshots without `quota`. Change the configuration and collector response to version 2 together when adding quota support.

## Runtime limits

- Five-second deadline.
- 1 MiB cap on stdout and stderr.
- Unknown JSON fields are rejected.
- Nonzero exit status is reported for that pane without breaking other collectors.
- The plugin does not invoke a shell or network itself.

The configured executable still runs with your user permissions and can do anything that user can do. Review it before adding it to the config.
