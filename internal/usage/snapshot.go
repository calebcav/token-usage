package usage

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Confidence describes how reliably a snapshot was associated with a live
// Herdr pane. It is deliberately separate from freshness.
type Confidence string

const (
	ConfidenceExact       Confidence = "exact"
	ConfidenceEstimated   Confidence = "estimated"
	ConfidenceUnsupported Confidence = "unsupported"
)

// Tokens is a non-overlapping token breakdown. Output includes Reasoning;
// Reasoning is retained as an informational subset and is never added to Total.
type Tokens struct {
	FreshInput uint64 `json:"fresh_input"`
	CacheRead  uint64 `json:"cache_read"`
	CacheWrite uint64 `json:"cache_write"`
	Output     uint64 `json:"output"`
	Reasoning  uint64 `json:"reasoning"`
	Total      uint64 `json:"total"`
}

// NewTokens creates a canonical, additive breakdown.
func NewTokens(freshInput, cacheRead, cacheWrite, output, reasoning uint64) (Tokens, error) {
	if reasoning > output {
		return Tokens{}, fmt.Errorf("reasoning tokens (%d) exceed inclusive output tokens (%d)", reasoning, output)
	}

	total, ok := add(freshInput, cacheRead, cacheWrite, output)
	if !ok {
		return Tokens{}, errors.New("token total overflows uint64")
	}

	return Tokens{
		FreshInput: freshInput,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
		Output:     output,
		Reasoning:  reasoning,
		Total:      total,
	}, nil
}

func (t Tokens) Validate() error {
	canonical, err := NewTokens(t.FreshInput, t.CacheRead, t.CacheWrite, t.Output, t.Reasoning)
	if err != nil {
		return err
	}
	if canonical.Total != t.Total {
		return fmt.Errorf("token total is %d; expected %d", t.Total, canonical.Total)
	}
	return nil
}

func add(values ...uint64) (uint64, bool) {
	var total uint64
	for _, value := range values {
		if math.MaxUint64-total < value {
			return 0, false
		}
		total += value
	}
	return total, true
}

type ContextWindow struct {
	Used  uint64 `json:"used"`
	Limit uint64 `json:"limit"`
}

func (c ContextWindow) Percent() float64 {
	if c.Limit == 0 {
		return 0
	}
	return float64(c.Used) / float64(c.Limit) * 100
}

// Snapshot is the single contract consumed by every renderer and external
// collector. Source is a safe source kind such as "codex-rollout"; it must not
// contain a transcript path or other user content.
type Snapshot struct {
	SchemaVersion  int            `json:"schema_version"`
	PaneID         string         `json:"pane_id,omitempty"`
	WorkspaceID    string         `json:"workspace_id,omitempty"`
	Harness        string         `json:"harness"`
	HarnessVersion string         `json:"harness_version,omitempty"`
	SessionID      string         `json:"session_id"`
	Model          string         `json:"model,omitempty"`
	Provider       string         `json:"provider,omitempty"`
	State          string         `json:"state,omitempty"`
	Tokens         Tokens         `json:"tokens"`
	Context        *ContextWindow `json:"context,omitempty"`
	Source         string         `json:"source"`
	Confidence     Confidence     `json:"confidence"`
	CollectedAt    time.Time      `json:"collected_at"`
}

const SchemaVersion = 1

func (s Snapshot) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported snapshot schema version %d", s.SchemaVersion)
	}
	if strings.TrimSpace(s.Harness) == "" {
		return errors.New("snapshot harness is required")
	}
	for name, value := range map[string]string{
		"harness":         s.Harness,
		"harness_version": s.HarnessVersion,
		"model":           s.Model,
		"provider":        s.Provider,
		"state":           s.State,
	} {
		if value != "" && !validDisplayText(value, 80) {
			return fmt.Errorf("snapshot %s contains unsafe display text", name)
		}
	}
	if strings.TrimSpace(s.SessionID) == "" && s.Confidence != ConfidenceUnsupported {
		return errors.New("snapshot session_id is required")
	}
	if strings.TrimSpace(s.Source) == "" {
		return errors.New("snapshot source is required")
	}
	if !isSafeSource(s.Source) {
		return errors.New("snapshot source must be a safe source label")
	}
	switch s.Confidence {
	case ConfidenceExact, ConfidenceEstimated, ConfidenceUnsupported:
	default:
		return fmt.Errorf("invalid snapshot confidence %q", s.Confidence)
	}
	if s.CollectedAt.IsZero() {
		return errors.New("snapshot collected_at is required")
	}
	if err := s.Tokens.Validate(); err != nil {
		return fmt.Errorf("snapshot tokens: %w", err)
	}
	if s.Context != nil && s.Context.Limit == 0 {
		return errors.New("context window limit must be greater than zero")
	}
	return nil
}

func validDisplayText(value string, maxRunes int) bool {
	if maxRunes > 0 && utf8.RuneCountInString(value) > maxRunes {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

// SanitizeText makes untrusted diagnostics safe to render in a terminal. ANSI
// introducers and all other control characters are removed before truncation.
func SanitizeText(value string, maxRunes int) string {
	value = strings.Map(func(char rune) rune {
		if unicode.IsControl(char) {
			return ' '
		}
		return char
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if maxRunes > 0 && len(runes) > maxRunes {
		if maxRunes == 1 {
			return "…"
		}
		value = string(runes[:maxRunes-1]) + "…"
	}
	return value
}

func isSafeSource(value string) bool {
	if len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' || char == ':' {
			continue
		}
		return false
	}
	return true
}

func FormatCount(value uint64) string {
	switch {
	case value >= 1_000_000_000:
		return trimDecimal(float64(value)/1_000_000_000) + "b"
	case value >= 1_000_000:
		return trimDecimal(float64(value)/1_000_000) + "m"
	case value >= 1_000:
		return trimDecimal(float64(value)/1_000) + "k"
	default:
		return strconv.FormatUint(value, 10)
	}
}

func trimDecimal(value float64) string {
	precision := 1
	if value >= 100 {
		precision = 0
	}
	return strconv.FormatFloat(value, 'f', precision, 64)
}

func (s Snapshot) CompactUsage() string {
	return "Σ " + FormatCount(s.Tokens.Total)
}

func (s Snapshot) CompactContext() string {
	if s.Context == nil {
		return ""
	}
	return fmt.Sprintf("ctx %.0f%% · %s/%s", s.Context.Percent(), FormatCount(s.Context.Used), FormatCount(s.Context.Limit))
}
