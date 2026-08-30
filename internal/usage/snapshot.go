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

// Spent returns fresh input and output tokens for the session. Cache activity
// remains available in the detailed breakdown but does not inflate this metric.
func (t Tokens) Spent() uint64 {
	return t.FreshInput + t.Output
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

type QuotaWindow struct {
	Label       string     `json:"label"`
	UsedPercent float64    `json:"used_percent"`
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
}

type QuotaSnapshot struct {
	Windows     []QuotaWindow `json:"windows"`
	Source      string        `json:"source"`
	CollectedAt time.Time     `json:"collected_at"`
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
	Quota          *QuotaSnapshot `json:"quota,omitempty"`
	Source         string         `json:"source"`
	Confidence     Confidence     `json:"confidence"`
	CollectedAt    time.Time      `json:"collected_at"`
}

const SchemaVersion = 2

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
	if s.Quota != nil {
		if len(s.Quota.Windows) == 0 || len(s.Quota.Windows) > 16 {
			return errors.New("quota must contain between 1 and 16 windows")
		}
		if strings.TrimSpace(s.Quota.Source) == "" || !isSafeSource(s.Quota.Source) {
			return errors.New("quota source must be a safe source label")
		}
		if s.Quota.CollectedAt.IsZero() {
			return errors.New("quota collected_at is required")
		}
		seen := make(map[string]struct{}, len(s.Quota.Windows))
		for _, window := range s.Quota.Windows {
			label := strings.TrimSpace(window.Label)
			if label == "" || !validDisplayText(label, 32) {
				return errors.New("quota window label is invalid")
			}
			if _, exists := seen[label]; exists {
				return fmt.Errorf("quota window label %q is duplicated", label)
			}
			seen[label] = struct{}{}
			if math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) || window.UsedPercent < 0 || window.UsedPercent > 999 {
				return fmt.Errorf("quota window %q percentage is invalid", label)
			}
			if window.ResetsAt != nil {
				if _, err := window.ResetsAt.MarshalJSON(); err != nil {
					return fmt.Errorf("quota window %q reset time is invalid", label)
				}
			}
		}
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
	return "Σ " + FormatCount(s.Tokens.Spent())
}

func (s Snapshot) CompactContext() string {
	if s.Context == nil {
		return ""
	}
	return fmt.Sprintf("ctx %.0f%% · %s/%s", s.Context.Percent(), FormatCount(s.Context.Used), FormatCount(s.Context.Limit))
}

func (s Snapshot) CompactLimit() string {
	window, ok := s.MostConstrainedQuota()
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s %.0f%%", window.Label, window.UsedPercent)
}

func (s Snapshot) MostConstrainedQuota() (QuotaWindow, bool) {
	if s.Quota == nil || len(s.Quota.Windows) == 0 {
		return QuotaWindow{}, false
	}
	selected := s.Quota.Windows[0]
	for _, window := range s.Quota.Windows[1:] {
		if window.UsedPercent > selected.UsedPercent {
			selected = window
		}
	}
	return selected, true
}
