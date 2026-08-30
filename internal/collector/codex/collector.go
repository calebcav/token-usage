// Package codex collects cumulative token usage from local Codex rollout files.
//
// The collector deliberately decodes only session metadata, model names, and
// token counters. Prompt, response, and tool payloads are never retained.
package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	basecollector "github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const (
	source               = "codex-rollout"
	DefaultRecencyWindow = 24 * time.Hour
)

// Config controls where the collector looks and whether it may associate a
// pane without a session ID by cwd and recency. Fallback matching is opt-in and
// succeeds only when exactly one rollout qualifies.
type Config struct {
	// Home is CODEX_HOME, not the sessions subdirectory itself.
	Home string

	AllowCWDRecencyFallback bool
	RecencyWindow           time.Duration

	// Now exists so recency checks and snapshots can be deterministic in tests.
	// It defaults to time.Now.
	Now func() time.Time
}

// Collector reads Codex's local, append-only rollout records.
type Collector struct {
	home                    string
	allowCWDRecencyFallback bool
	recencyWindow           time.Duration
	now                     func() time.Time
}

// New constructs a Codex collector. An empty Home follows Codex's normal
// CODEX_HOME resolution: the environment variable first, then ~/.codex.
func New(config Config) *Collector {
	home := strings.TrimSpace(config.Home)
	if home == "" {
		home = strings.TrimSpace(os.Getenv("CODEX_HOME"))
	}
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".codex")
		}
	}

	window := config.RecencyWindow
	if window <= 0 {
		window = DefaultRecencyWindow
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}

	return &Collector{
		home:                    home,
		allowCWDRecencyFallback: config.AllowCWDRecencyFallback,
		recencyWindow:           window,
		now:                     now,
	}
}

func (c *Collector) Harnesses() []string {
	return []string{"codex"}
}

// Collect resolves one rollout and returns the final complete cumulative token
// event. Repeated cumulative events are intentionally not summed.
func (c *Collector) Collect(ctx context.Context, target basecollector.Target) (usage.Snapshot, error) {
	if harness := basecollector.CanonicalHarness(target.Harness); harness != "" && harness != "codex" {
		return usage.Snapshot{}, basecollector.ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return usage.Snapshot{}, err
	}
	if strings.TrimSpace(c.home) == "" {
		return usage.Snapshot{}, basecollector.ErrSessionNotFound
	}

	paths, err := listRollouts(ctx, filepath.Join(c.home, "sessions"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return usage.Snapshot{}, basecollector.ErrSessionNotFound
		}
		return usage.Snapshot{}, err
	}

	resolved, confidence, err := c.resolve(ctx, paths, target)
	if err != nil {
		return usage.Snapshot{}, err
	}

	parsed, err := readRollout(ctx, resolved.path)
	if err != nil {
		return usage.Snapshot{}, err
	}

	sessionID := strings.TrimSpace(target.SessionID)
	if confidence == usage.ConfidenceEstimated {
		sessionID = resolved.metadata.ID
	}

	snapshot := usage.Snapshot{
		SchemaVersion:  usage.SchemaVersion,
		PaneID:         target.PaneID,
		WorkspaceID:    target.WorkspaceID,
		Harness:        "codex",
		HarnessVersion: safeLabel(resolved.metadata.CLIVersion),
		SessionID:      sessionID,
		Model:          safeLabel(parsed.model),
		Provider:       safeLabel(resolved.metadata.ModelProvider),
		State:          target.State,
		Tokens:         parsed.tokens,
		Context:        parsed.context,
		Source:         source,
		Confidence:     confidence,
		CollectedAt:    c.now().UTC(),
	}
	if err := snapshot.Validate(); err != nil {
		return usage.Snapshot{}, malformed(err)
	}
	return snapshot, nil
}
