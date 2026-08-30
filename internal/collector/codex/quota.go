package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/calebcav/token-usage/internal/usage"
)

const (
	defaultCodexBinary  = "codex"
	quotaSource         = "codex-app-server"
	quotaCacheFilename  = "codex-account-limits"
	defaultQuotaTimeout = 5 * time.Second
	quotaCacheTTL       = time.Minute
	maxAppServerMessage = 1 << 20
	maxQuotaCacheBytes  = 64 << 10
)

// QuotaLoader returns the encoded result object from an
// account/rateLimits/read app-server response.
type QuotaLoader func(context.Context) ([]byte, error)

type rawRateLimitWindow struct {
	UsedPercent       *float64 `json:"usedPercent"`
	WindowDurationMin *int64   `json:"windowDurationMins"`
	ResetsAt          *int64   `json:"resetsAt"`
}

type rawSpendLimit struct {
	RemainingPercent *float64 `json:"remainingPercent"`
	ResetsAt         *int64   `json:"resetsAt"`
}

type rawRateLimits struct {
	Primary         *rawRateLimitWindow `json:"primary"`
	Secondary       *rawRateLimitWindow `json:"secondary"`
	IndividualLimit *rawSpendLimit      `json:"individualLimit"`
}

type quotaCache struct {
	Version int                 `json:"version"`
	Quota   usage.QuotaSnapshot `json:"quota"`
}

func (c *Collector) collectQuota(ctx context.Context) *usage.QuotaSnapshot {
	select {
	case <-ctx.Done():
		return nil
	case <-c.quotaGate:
	}
	defer func() { c.quotaGate <- struct{}{} }()

	now := c.now().UTC()
	if quota, err := readQuotaCache(ctx, c.quotaCachePath, now); err == nil {
		c.quotaFailureAt = time.Time{}
		return quota
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	if age := now.Sub(c.quotaFailureAt); !c.quotaFailureAt.IsZero() && age >= 0 && age < quotaCacheTTL {
		return nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, c.quotaTimeout)
	defer cancel()
	loader := c.quotaLoader
	if loader == nil {
		loader = func(ctx context.Context) ([]byte, error) {
			return loadAppServerResult(ctx, c.binary, c.home)
		}
	}
	result, err := loader(fetchCtx)
	if err != nil || fetchCtx.Err() != nil {
		if ctx.Err() == nil {
			c.quotaFailureAt = now
		}
		return nil
	}
	quota, err := parseQuotaResult(result, now)
	if err != nil {
		c.quotaFailureAt = now
		return nil
	}
	c.quotaFailureAt = time.Time{}
	_ = writeQuotaCache(ctx, c.quotaCachePath, *quota)
	return quota
}

func parseQuotaResult(result []byte, collectedAt time.Time) (*usage.QuotaSnapshot, error) {
	var response struct {
		RateLimits json.RawMessage `json:"rateLimits"`
	}
	if err := json.Unmarshal(result, &response); err != nil || len(response.RateLimits) == 0 || string(response.RateLimits) == "null" {
		return nil, errors.New("codex account limits response is malformed")
	}

	var raw rawRateLimits
	if err := json.Unmarshal(response.RateLimits, &raw); err != nil {
		return nil, errors.New("codex account limits response is malformed")
	}

	windows := make([]usage.QuotaWindow, 0, 3)
	labels := make(map[string]struct{}, 3)
	addWindow := func(label string, usedPercent, maximum float64, resetsAt *int64) error {
		if _, exists := labels[label]; exists {
			return nil
		}
		if math.IsNaN(usedPercent) || math.IsInf(usedPercent, 0) || usedPercent < 0 || usedPercent > maximum {
			return errors.New("codex account limits percentage is invalid")
		}
		reset, active, err := quotaResetTime(resetsAt, collectedAt)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		labels[label] = struct{}{}
		windows = append(windows, usage.QuotaWindow{
			Label:       label,
			UsedPercent: usedPercent,
			ResetsAt:    reset,
		})
		return nil
	}
	addRateWindow := func(raw *rawRateLimitWindow, fallback string) error {
		if raw == nil {
			return nil
		}
		if raw.UsedPercent == nil {
			return errors.New("codex account limits percentage is missing")
		}
		return addWindow(durationLabel(raw.WindowDurationMin, fallback), *raw.UsedPercent, 100, raw.ResetsAt)
	}

	if err := addRateWindow(raw.Primary, "primary"); err != nil {
		return nil, err
	}
	if err := addRateWindow(raw.Secondary, "secondary"); err != nil {
		return nil, err
	}
	if raw.IndividualLimit != nil {
		if raw.IndividualLimit.RemainingPercent == nil {
			return nil, errors.New("codex spend limit percentage is missing")
		}
		remaining := *raw.IndividualLimit.RemainingPercent
		if math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < -899 || remaining > 100 {
			return nil, errors.New("codex spend limit percentage is invalid")
		}
		if err := addWindow("spend", 100-remaining, 999, raw.IndividualLimit.ResetsAt); err != nil {
			return nil, err
		}
	}
	if len(windows) == 0 {
		return nil, errors.New("codex account limits response contains no windows")
	}

	quota := &usage.QuotaSnapshot{
		Windows:     windows,
		Source:      quotaSource,
		CollectedAt: collectedAt,
	}
	if err := validateQuota(*quota); err != nil {
		return nil, err
	}
	return quota, nil
}

func durationLabel(minutes *int64, fallback string) string {
	if minutes == nil || *minutes <= 0 {
		return fallback
	}
	if *minutes%1440 == 0 {
		return fmt.Sprintf("%dd", *minutes/1440)
	}
	if *minutes%60 == 0 {
		return fmt.Sprintf("%dh", *minutes/60)
	}
	return fmt.Sprintf("%dm", *minutes)
}

func quotaResetTime(seconds *int64, collectedAt time.Time) (*time.Time, bool, error) {
	if seconds == nil {
		return nil, true, nil
	}
	value := time.Unix(*seconds, 0).UTC()
	if _, err := value.MarshalJSON(); err != nil {
		return nil, false, errors.New("codex account limits reset time is invalid")
	}
	if !value.After(collectedAt) {
		return nil, false, nil
	}
	return &value, true, nil
}

func validateQuota(quota usage.QuotaSnapshot) error {
	if quota.Source != quotaSource {
		return errors.New("codex quota source is invalid")
	}
	snapshot := usage.Snapshot{
		SchemaVersion: usage.SchemaVersion,
		Harness:       "codex",
		SessionID:     "quota-cache-validation",
		Quota:         &quota,
		Source:        source,
		Confidence:    usage.ConfidenceExact,
		CollectedAt:   quota.CollectedAt,
	}
	return snapshot.Validate()
}

func readQuotaCache(ctx context.Context, path string, now time.Time) (*usage.QuotaSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("codex quota cache is disabled")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("codex quota cache could not be read")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxQuotaCacheBytes+1))
	if err != nil || len(data) > maxQuotaCacheBytes {
		return nil, errors.New("codex quota cache could not be read")
	}
	var cached quotaCache
	if err := json.Unmarshal(data, &cached); err != nil || cached.Version != 1 {
		return nil, errors.New("codex quota cache is malformed")
	}
	age := now.Sub(cached.Quota.CollectedAt)
	if age < 0 || age >= quotaCacheTTL {
		return nil, errors.New("codex quota cache is expired")
	}
	for _, window := range cached.Quota.Windows {
		if window.ResetsAt != nil && !window.ResetsAt.After(now) {
			return nil, errors.New("codex quota cache is expired")
		}
	}
	if err := validateQuota(cached.Quota); err != nil {
		return nil, errors.New("codex quota cache is malformed")
	}
	return &cached.Quota, nil
}

func writeQuotaCache(ctx context.Context, path string, quota usage.QuotaSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("codex quota cache is disabled")
	}
	if err := validateQuota(quota); err != nil {
		return errors.New("codex quota cache is malformed")
	}
	data, err := json.Marshal(quotaCache{Version: 1, Quota: quota})
	if err != nil {
		return errors.New("codex quota cache could not be encoded")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("codex quota cache could not be written")
	}
	temporary, err := os.CreateTemp(directory, ".codex-account-limits-*")
	if err != nil {
		return errors.New("codex quota cache could not be written")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return errors.New("codex quota cache could not be written")
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return errors.New("codex quota cache could not be written")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return errors.New("codex quota cache could not be written")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("codex quota cache could not be written")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.New("codex quota cache could not be written")
	}
	return nil
}

func loadAppServerResult(ctx context.Context, binary, home string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, "app-server", "--stdio")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return killAppServerProcessGroup(command.Process.Pid)
	}
	command.Stderr = io.Discard
	command.WaitDelay = time.Second
	command.Env = environmentWithValue(os.Environ(), "CODEX_HOME", home)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, errors.New("codex app-server could not be started")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, errors.New("codex app-server could not be started")
	}
	if err := command.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, errors.New("codex app-server could not be started")
	}
	defer func() {
		stdin.Close()
		stdout.Close()
		if command.Process != nil {
			_ = killAppServerProcessGroup(command.Process.Pid)
		}
		_ = command.Wait()
	}()

	result, err := exchangeAppServer(ctx, stdin, stdout)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, errors.New("codex app-server request failed")
	}
	return result, nil
}

func defaultQuotaCachePath(directory, home, binary string) string {
	digest := sha256.Sum256([]byte(home + "\x00" + binary))
	return filepath.Join(directory, quotaCacheFilename+"-"+hex.EncodeToString(digest[:8])+".json")
}

func environmentWithValue(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	if strings.TrimSpace(value) != "" {
		result = append(result, prefix+value)
	}
	return result
}

func killAppServerProcessGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func exchangeAppServer(ctx context.Context, stdin io.Writer, stdout io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	encoder := json.NewEncoder(stdin)
	if err := encoder.Encode(map[string]any{
		"method": "initialize",
		"id":     1,
		"params": map[string]any{
			"clientInfo": map[string]string{
				"name":    "token-usage",
				"version": "0.1.0",
			},
		},
	}); err != nil {
		return nil, errors.New("codex app-server request failed")
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), maxAppServerMessage)
	if _, err := awaitAppServerResponse(ctx, scanner, 1); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized"}); err != nil {
		return nil, errors.New("codex app-server request failed")
	}
	if err := encoder.Encode(map[string]any{
		"method": "account/rateLimits/read",
		"id":     2,
		"params": nil,
	}); err != nil {
		return nil, errors.New("codex app-server request failed")
	}
	return awaitAppServerResponse(ctx, scanner, 2)
}

func awaitAppServerResponse(ctx context.Context, scanner *bufio.Scanner, wantedID int) ([]byte, error) {
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return nil, errors.New("codex app-server response is malformed")
		}
		var id int
		if len(message.ID) == 0 || json.Unmarshal(message.ID, &id) != nil || id != wantedID {
			// Notifications have no ID. Unrelated messages are also harmless
			// while waiting for the response to our fixed request IDs.
			continue
		}
		if len(message.Error) != 0 && string(message.Error) != "null" {
			return nil, errors.New("codex app-server request was rejected")
		}
		if len(message.Result) == 0 || string(message.Result) == "null" {
			return nil, errors.New("codex app-server response is malformed")
		}
		return append([]byte(nil), message.Result...), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("codex app-server response was unavailable")
}
