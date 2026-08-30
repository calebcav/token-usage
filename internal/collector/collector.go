package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/calebcav/token-usage/internal/usage"
)

var (
	ErrUnsupported     = errors.New("unsupported harness")
	ErrSessionNotFound = errors.New("session not found")
	ErrAmbiguous       = errors.New("ambiguous session")
	ErrMalformed       = errors.New("malformed usage source")
)

type Target struct {
	PaneID      string           `json:"pane_id,omitempty"`
	WorkspaceID string           `json:"workspace_id,omitempty"`
	Harness     string           `json:"harness"`
	SessionID   string           `json:"session_id,omitempty"`
	SessionRef  *SessionRef      `json:"session_ref,omitempty"`
	CWD         string           `json:"cwd,omitempty"`
	State       string           `json:"state,omitempty"`
	Confidence  usage.Confidence `json:"confidence"`
}

type SessionRef struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

type Process struct {
	Name  string   `json:"name"`
	Argv0 string   `json:"argv0,omitempty"`
	Argv  []string `json:"argv,omitempty"`
	CWD   string   `json:"cwd,omitempty"`
}

type ProcessMatcher interface {
	MatchProcess([]Process) (string, error)
}

type Collector interface {
	Harnesses() []string
	Collect(context.Context, Target) (usage.Snapshot, error)
}

type Registry struct {
	byHarness       map[string]Collector
	processMatchers []ProcessMatcher
}

func NewRegistry(collectors ...Collector) (*Registry, error) {
	registry := &Registry{byHarness: make(map[string]Collector)}
	for _, item := range collectors {
		if item == nil {
			return nil, errors.New("collector is nil")
		}
		for _, harness := range item.Harnesses() {
			canonical := CanonicalHarness(harness)
			if canonical == "" {
				return nil, errors.New("collector declared an empty harness")
			}
			if _, exists := registry.byHarness[canonical]; exists {
				return nil, fmt.Errorf("collector already registered for %q", canonical)
			}
			registry.byHarness[canonical] = item
		}
		if matcher, ok := item.(ProcessMatcher); ok {
			if state, ok := item.(interface{ HasProcessMatchers() bool }); !ok || state.HasProcessMatchers() {
				registry.processMatchers = append(registry.processMatchers, matcher)
			}
		}
	}
	return registry, nil
}

func (r *Registry) HasProcessMatchers() bool {
	return r != nil && len(r.processMatchers) > 0
}

func (r *Registry) DetectHarness(processes []Process) (string, error) {
	if r == nil {
		return "", errors.New("collector registry is nil")
	}
	var match string
	for _, matcher := range r.processMatchers {
		harness, err := matcher.MatchProcess(processes)
		if err != nil {
			return "", err
		}
		harness = CanonicalHarness(harness)
		if harness == "" {
			continue
		}
		if match != "" && match != harness {
			return "", fmt.Errorf("%w: foreground process matches %s and %s", ErrAmbiguous, match, harness)
		}
		match = harness
	}
	return match, nil
}

func (r *Registry) CollectorFor(harness string) (Collector, error) {
	canonical := CanonicalHarness(harness)
	item, ok := r.byHarness[canonical]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, harness)
	}
	return item, nil
}

func CanonicalHarness(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "claude-code", "claude_code":
		return "claude"
	case "codex-cli", "codex_cli":
		return "codex"
	case "open-code", "open_code":
		return "opencode"
	default:
		return value
	}
}
