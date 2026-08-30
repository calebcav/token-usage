package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/calebcav/token-usage/internal/usage"
)

const (
	statusLineSource      = "claude-statusline"
	statusStateVersion    = 1
	statusQuotaTTL        = 5 * time.Minute
	maxStatusLineJSON     = 1 << 20
	maxStatusSessionID    = 4096
	maxStatusResetSeconds = int64(253402300799)
)

// CaptureOption configures status-line capture.
type CaptureOption func(*captureConfig)

type captureConfig struct {
	stateDir string
	now      func() time.Time
}

// WithCaptureStateDir writes status-line state to path instead of the shared
// user cache directory.
func WithCaptureStateDir(path string) CaptureOption {
	return func(config *captureConfig) {
		config.stateDir = path
	}
}

// WithCaptureNow supplies the collection clock used by status-line capture.
func WithCaptureNow(now func() time.Time) CaptureOption {
	return func(config *captureConfig) {
		if now != nil {
			config.now = now
		}
	}
}

// CaptureStatusLine reads one Claude Code status-line payload, persists its
// normalized non-content state, and prints a concise status line.
func CaptureStatusLine(input io.Reader, output io.Writer) error {
	return CaptureStatusLineWithOptions(input, output)
}

// CaptureStatusLineWithOptions is CaptureStatusLine with injectable state and
// clock settings for tests and nonstandard installations.
func CaptureStatusLineWithOptions(input io.Reader, output io.Writer, options ...CaptureOption) error {
	if input == nil || output == nil {
		return errors.New("Claude status-line input and output are required")
	}
	config := captureConfig{now: time.Now}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}

	var raw rawStatusLine
	if err := decodeBoundedJSON(input, &raw); err != nil {
		return errors.New("invalid Claude status-line payload")
	}
	collectedAt := config.now().UTC()
	state, err := normalizeStatusLine(raw, collectedAt)
	if err != nil {
		return errors.New("invalid Claude status-line payload")
	}
	stateDir, err := statusStateDir(config.stateDir)
	if err != nil {
		return errors.New("Claude status-line state directory is unavailable")
	}
	if err := writeStatusState(stateDir, state); err != nil {
		return errors.New("Claude status-line state could not be written")
	}
	if _, err := fmt.Fprintln(output, formatStatusLine(state, collectedAt)); err != nil {
		return errors.New("Claude status line could not be written")
	}
	return nil
}

type rawStatusLine struct {
	SessionID     string               `json:"session_id"`
	ContextWindow *rawStatusContext    `json:"context_window"`
	RateLimits    *rawStatusRateLimits `json:"rate_limits"`
}

type rawStatusContext struct {
	ContextWindowSize *uint64                `json:"context_window_size"`
	UsedPercentage    *float64               `json:"used_percentage"`
	CurrentUsage      *rawStatusCurrentUsage `json:"current_usage"`
}

type rawStatusCurrentUsage struct {
	InputTokens              *uint64 `json:"input_tokens"`
	OutputTokens             *uint64 `json:"output_tokens"`
	CacheCreationInputTokens *uint64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *uint64 `json:"cache_read_input_tokens"`
}

type rawStatusRateLimits struct {
	FiveHour   *rawStatusRateLimit `json:"five_hour"`
	SevenDay   *rawStatusRateLimit `json:"seven_day"`
	SpendLimit *rawStatusRateLimit `json:"spend_limit"`
}

type rawStatusRateLimit struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       *int64   `json:"resets_at"`
}

type statusState struct {
	Version     int             `json:"version"`
	SessionKey  string          `json:"session_key"`
	CollectedAt time.Time       `json:"collected_at"`
	Context     json.RawMessage `json:"context,omitempty"`
	Quota       json.RawMessage `json:"quota,omitempty"`
}

func normalizeStatusLine(raw rawStatusLine, collectedAt time.Time) (statusState, error) {
	sessionID := strings.TrimSpace(raw.SessionID)
	if sessionID == "" || len(sessionID) > maxStatusSessionID || collectedAt.IsZero() {
		return statusState{}, errors.New("invalid status-line identity")
	}
	contextWindow, err := normalizeStatusContext(raw.ContextWindow)
	if err != nil {
		return statusState{}, err
	}
	quota, err := normalizeStatusQuota(raw.RateLimits, collectedAt)
	if err != nil {
		return statusState{}, err
	}

	state := statusState{
		Version:     statusStateVersion,
		SessionKey:  statusSessionKey(sessionID),
		CollectedAt: collectedAt,
	}
	if contextWindow != nil {
		state.Context, err = json.Marshal(contextWindow)
		if err != nil {
			return statusState{}, err
		}
	}
	if quota != nil {
		state.Quota, err = json.Marshal(quota)
		if err != nil {
			return statusState{}, err
		}
	}
	return state, nil
}

func normalizeStatusContext(raw *rawStatusContext) (*usage.ContextWindow, error) {
	if raw == nil {
		return nil, nil
	}
	if raw.UsedPercentage != nil && !validPercentage(*raw.UsedPercentage, 100) {
		return nil, errors.New("invalid context percentage")
	}
	if raw.ContextWindowSize == nil {
		return nil, nil
	}
	limit := *raw.ContextWindowSize
	if limit == 0 {
		return nil, errors.New("invalid context limit")
	}

	var used uint64
	hasExactUsage := false
	if raw.CurrentUsage != nil {
		current := raw.CurrentUsage
		hasExactUsage = current.InputTokens != nil || current.CacheCreationInputTokens != nil || current.CacheReadInputTokens != nil
		if hasExactUsage {
			var ok bool
			used, ok = safeAdd(value(current.InputTokens), value(current.CacheCreationInputTokens), value(current.CacheReadInputTokens))
			if !ok {
				return nil, errors.New("context usage overflow")
			}
		}
	}
	if !hasExactUsage {
		if raw.UsedPercentage == nil {
			return nil, nil
		}
		used = contextUsedFromPercentage(limit, *raw.UsedPercentage)
	}
	if used > limit {
		return nil, errors.New("context usage exceeds limit")
	}
	return &usage.ContextWindow{Used: used, Limit: limit}, nil
}

func contextUsedFromPercentage(limit uint64, percentage float64) uint64 {
	if percentage <= 0 {
		return 0
	}
	if percentage >= 100 {
		return limit
	}
	value := new(big.Float).SetPrec(256).SetUint64(limit)
	value.Mul(value, new(big.Float).SetPrec(256).SetFloat64(percentage))
	value.Quo(value, new(big.Float).SetPrec(256).SetInt64(100))
	value.Add(value, new(big.Float).SetPrec(256).SetFloat64(0.5))
	result, _ := value.Uint64()
	return result
}

func normalizeStatusQuota(raw *rawStatusRateLimits, collectedAt time.Time) (*usage.QuotaSnapshot, error) {
	if raw == nil {
		return nil, nil
	}
	windows := make([]usage.QuotaWindow, 0, 3)
	for _, candidate := range []struct {
		label      string
		limit      float64
		rateWindow *rawStatusRateLimit
	}{
		{label: "5h", limit: 100, rateWindow: raw.FiveHour},
		{label: "7d", limit: 100, rateWindow: raw.SevenDay},
		{label: "spend", limit: 999, rateWindow: raw.SpendLimit},
	} {
		if candidate.rateWindow == nil {
			continue
		}
		window := candidate.rateWindow
		if window.UsedPercentage == nil {
			if window.ResetsAt != nil {
				return nil, errors.New("rate-limit percentage is missing")
			}
			continue
		}
		if !validPercentage(*window.UsedPercentage, candidate.limit) {
			return nil, errors.New("invalid rate-limit percentage")
		}
		normalized := usage.QuotaWindow{Label: candidate.label, UsedPercent: *window.UsedPercentage}
		if window.ResetsAt != nil {
			if *window.ResetsAt < 0 || *window.ResetsAt > maxStatusResetSeconds {
				return nil, errors.New("invalid rate-limit reset")
			}
			reset := time.Unix(*window.ResetsAt, 0).UTC()
			normalized.ResetsAt = &reset
		}
		windows = append(windows, normalized)
	}
	if len(windows) == 0 {
		return nil, nil
	}
	return &usage.QuotaSnapshot{Windows: windows, Source: statusLineSource, CollectedAt: collectedAt}, nil
}

func validPercentage(value, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= maximum
}

func statusSessionKey(sessionID string) string {
	digest := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(digest[:])
}

func statusStateDir(configured string) (string, error) {
	if path := strings.TrimSpace(configured); path != "" {
		return filepath.Clean(path), nil
	}
	if path := strings.TrimSpace(os.Getenv("TOKEN_USAGE_CLAUDE_STATE_DIR")); path != "" {
		return filepath.Clean(path), nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "token-usage", "claude-statusline"), nil
}

func statusStatePath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, statusSessionKey(strings.TrimSpace(sessionID))+".json")
}

func writeStatusState(stateDir string, state statusState) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(stateDir, ".status-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(stateDir, state.SessionKey+".json"))
}

func decodeBoundedJSON(reader io.Reader, destination any) error {
	encoded, err := io.ReadAll(io.LimitReader(reader, maxStatusLineJSON+1))
	if err != nil {
		return err
	}
	if len(encoded) > maxStatusLineJSON {
		return errors.New("JSON payload exceeds limit")
	}
	return json.Unmarshal(encoded, destination)
}

func (c *Collector) loadStatusState(sessionID string, now time.Time, transcriptUpdatedAt ...time.Time) (*usage.ContextWindow, *usage.QuotaSnapshot) {
	configuredDir := ""
	if c != nil {
		configuredDir = c.statusDir
	}
	stateDir, err := statusStateDir(configuredDir)
	if err != nil {
		return nil, nil
	}
	path := statusStatePath(stateDir, sessionID)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer file.Close()

	var state statusState
	if decodeBoundedJSON(file, &state) != nil || state.Version != statusStateVersion || state.SessionKey != statusSessionKey(strings.TrimSpace(sessionID)) {
		return nil, nil
	}
	age := now.Sub(state.CollectedAt)
	if state.CollectedAt.IsZero() || age < -maxFutureClockSkew {
		return nil, nil
	}
	contextIsStale := len(transcriptUpdatedAt) > 0 && !transcriptUpdatedAt[0].IsZero() && state.CollectedAt.Before(transcriptUpdatedAt[0])

	var contextWindow *usage.ContextWindow
	if !contextIsStale && len(state.Context) != 0 && string(state.Context) != "null" {
		var candidate usage.ContextWindow
		if json.Unmarshal(state.Context, &candidate) == nil && candidate.Limit > 0 && candidate.Used <= candidate.Limit {
			contextWindow = &candidate
		}
	}

	var quota *usage.QuotaSnapshot
	if len(state.Quota) != 0 && string(state.Quota) != "null" {
		var candidate usage.QuotaSnapshot
		if json.Unmarshal(state.Quota, &candidate) == nil && validStatusQuota(candidate, now) {
			quota = activeStatusQuota(candidate, now)
		}
	}
	return contextWindow, quota
}

func validStatusQuota(quota usage.QuotaSnapshot, now time.Time) bool {
	if quota.Source != statusLineSource || quota.CollectedAt.IsZero() || quota.CollectedAt.After(now.Add(maxFutureClockSkew)) || len(quota.Windows) == 0 || len(quota.Windows) > 3 {
		return false
	}
	seen := make(map[string]struct{}, len(quota.Windows))
	for _, window := range quota.Windows {
		maximum := float64(100)
		switch window.Label {
		case "5h", "7d":
		case "spend":
			maximum = 999
		default:
			return false
		}
		if _, exists := seen[window.Label]; exists || !validPercentage(window.UsedPercent, maximum) {
			return false
		}
		seen[window.Label] = struct{}{}
	}
	return true
}

func activeStatusQuota(quota usage.QuotaSnapshot, now time.Time) *usage.QuotaSnapshot {
	age := now.Sub(quota.CollectedAt)
	if age < 0 || age >= statusQuotaTTL {
		return nil
	}
	active := make([]usage.QuotaWindow, 0, len(quota.Windows))
	for _, window := range quota.Windows {
		if window.ResetsAt != nil && !window.ResetsAt.After(now) {
			continue
		}
		active = append(active, window)
	}
	if len(active) == 0 {
		return nil
	}
	quota.Windows = active
	return &quota
}

func formatStatusLine(state statusState, now time.Time) string {
	parts := make([]string, 0, 4)
	if len(state.Context) != 0 {
		var contextWindow usage.ContextWindow
		if json.Unmarshal(state.Context, &contextWindow) == nil && contextWindow.Limit > 0 {
			parts = append(parts, fmt.Sprintf("ctx %.0f%%", contextWindow.Percent()))
		}
	}
	if len(state.Quota) != 0 {
		var quota usage.QuotaSnapshot
		if json.Unmarshal(state.Quota, &quota) == nil {
			if active := activeStatusQuota(quota, now); active != nil {
				for _, window := range active.Windows {
					parts = append(parts, fmt.Sprintf("%s %.0f%%", window.Label, window.UsedPercent))
				}
			}
		}
	}
	if len(parts) == 0 {
		return "not reported"
	}
	return strings.Join(parts, " | ")
}
