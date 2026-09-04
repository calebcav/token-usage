// Package pi collects token usage from local Pi coding-agent sessions.
package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	basecollector "github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const (
	sourceLabel            = "pi-session"
	defaultFallbackMaxAge  = 6 * time.Hour
	defaultMaxSessionBytes = 64 << 20
	maxJSONLine            = 8 << 20
)

type ContextLimitResolver interface {
	ResolveContextLimit(context.Context, string, string) (uint64, error)
}

type Config struct {
	SessionDir              string
	AllowCWDRecencyFallback bool
	FallbackMaxAge          time.Duration
	ContextResolver         ContextLimitResolver
	Now                     func() time.Time
}

type Collector struct {
	sessionDir              string
	allowCWDRecencyFallback bool
	fallbackMaxAge          time.Duration
	now                     func() time.Time
	contextResolver         ContextLimitResolver
	contextMu               sync.Mutex
	contextLimits           map[string]uint64
}

func New(config Config) *Collector {
	fallbackMaxAge := config.FallbackMaxAge
	if fallbackMaxAge <= 0 {
		fallbackMaxAge = defaultFallbackMaxAge
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Collector{
		sessionDir:              config.SessionDir,
		allowCWDRecencyFallback: config.AllowCWDRecencyFallback,
		fallbackMaxAge:          fallbackMaxAge,
		now:                     now,
		contextResolver:         contextResolver(config.ContextResolver),
		contextLimits:           make(map[string]uint64),
	}
}

func (c *Collector) Harnesses() []string { return []string{"pi"} }

func (c *Collector) MatchProcess(processes []basecollector.Process) (string, error) {
	for _, process := range processes {
		if isPiProcess(process.Name) || isPiProcess(process.Argv0) {
			return "pi", nil
		}
		for _, arg := range process.Argv {
			if isPiProcess(arg) || isPiPackagePath(arg) {
				return "pi", nil
			}
		}
	}
	return "", nil
}

func isPiProcess(value string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(value)))
	return base == "pi" || base == "pi-coding-agent" || base == "@earendil-works/pi-coding-agent"
}

func isPiPackagePath(value string) bool {
	value = strings.ToLower(filepath.ToSlash(strings.TrimSpace(value)))
	return strings.Contains(value, "/@earendil-works/pi-coding-agent/") || strings.Contains(value, "/pi-coding-agent/")
}

func (c *Collector) Collect(ctx context.Context, target basecollector.Target) (usage.Snapshot, error) {
	if basecollector.CanonicalHarness(target.Harness) != "pi" {
		return usage.Snapshot{}, basecollector.ErrUnsupported
	}
	root, err := c.resolveSessionDir()
	if err != nil {
		return usage.Snapshot{}, err
	}
	path, confidence, err := c.resolveSession(ctx, root, target)
	if err != nil {
		return usage.Snapshot{}, err
	}
	parsed, err := parseSessionFile(path)
	if err != nil {
		return usage.Snapshot{}, err
	}
	if parsed.sessionID == "" {
		return usage.Snapshot{}, malformed(errors.New("missing Pi session header"))
	}
	conf := confidence
	if target.Confidence == usage.ConfidenceEstimated {
		conf = usage.ConfidenceEstimated
	}
	var contextWindow *usage.ContextWindow
	if parsed.contextUsed != nil {
		if limit, ok := c.resolveContextLimit(ctx, parsed.provider, parsed.model); ok && *parsed.contextUsed <= limit {
			contextWindow = &usage.ContextWindow{Used: *parsed.contextUsed, Limit: limit}
		}
	}
	return usage.Snapshot{
		SchemaVersion: usage.SchemaVersion,
		Harness:       "pi",
		SessionID:     parsed.sessionID,
		Model:         parsed.model,
		Provider:      parsed.provider,
		Tokens:        parsed.tokens,
		Context:       contextWindow,
		Source:        sourceLabel,
		Confidence:    conf,
		CollectedAt:   c.now().UTC(),
	}, nil
}

func (c *Collector) resolveSessionDir() (string, error) {
	if strings.TrimSpace(c.sessionDir) != "" {
		return c.sessionDir, nil
	}
	if value := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_SESSION_DIR")); value != "" {
		return value, nil
	}
	base := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".pi", "agent")
	}
	return filepath.Join(base, "sessions"), nil
}

func (c *Collector) resolveSession(ctx context.Context, root string, target basecollector.Target) (string, usage.Confidence, error) {
	if strings.TrimSpace(target.SessionID) != "" {
		path, err := findSessionByID(ctx, root, target.SessionID)
		if err != nil {
			return "", "", err
		}
		return path, usage.ConfidenceExact, nil
	}
	if !c.allowCWDRecencyFallback || strings.TrimSpace(target.CWD) == "" {
		return "", "", basecollector.ErrSessionNotFound
	}
	path, err := findRecentSessionByCWD(ctx, root, target.CWD, c.now().Add(-c.fallbackMaxAge))
	if err != nil {
		return "", "", err
	}
	return path, usage.ConfidenceEstimated, nil
}

type sessionCandidate struct {
	path    string
	modTime time.Time
}

func findSessionByID(ctx context.Context, root, id string) (string, error) {
	id = strings.TrimSpace(id)
	var match string
	err := walkSessionFiles(ctx, root, func(path string, info os.FileInfo) error {
		header, err := readHeader(path)
		if err != nil {
			return nil
		}
		if header.ID == id {
			match = path
			return io.EOF
		}
		return nil
	})
	if errors.Is(err, io.EOF) {
		return match, nil
	}
	if err != nil {
		return "", err
	}
	return "", basecollector.ErrSessionNotFound
}

func findRecentSessionByCWD(ctx context.Context, root, cwd string, cutoff time.Time) (string, error) {
	cwd = filepath.Clean(strings.TrimSpace(cwd))
	var candidates []sessionCandidate
	err := walkSessionFiles(ctx, root, func(path string, info os.FileInfo) error {
		if info.ModTime().Before(cutoff) {
			return nil
		}
		header, err := readHeader(path)
		if err != nil || filepath.Clean(header.CWD) != cwd {
			return nil
		}
		candidates = append(candidates, sessionCandidate{path: path, modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", basecollector.ErrSessionNotFound
	}
	if len(candidates) > 1 {
		return "", basecollector.ErrAmbiguous
	}
	return candidates[0].path, nil
}

func walkSessionFiles(ctx context.Context, root string, visit func(string, os.FileInfo) error) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > defaultMaxSessionBytes {
			return nil
		}
		return visit(path, info)
	})
}

type rawHeader struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	CWD  string `json:"cwd"`
}

func readHeader(path string) (rawHeader, error) {
	file, err := os.Open(path)
	if err != nil {
		return rawHeader{}, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return rawHeader{}, err
	}
	var header rawHeader
	if err := json.Unmarshal(bytes.TrimSpace(line), &header); err != nil {
		return rawHeader{}, err
	}
	if header.Type != "session" {
		return rawHeader{}, errors.New("not a Pi session")
	}
	return header, nil
}

type parsedSession struct {
	sessionID   string
	provider    string
	model       string
	tokens      usage.Tokens
	contextUsed *uint64
}

type rawEntry struct {
	Type     string     `json:"type"`
	ID       string     `json:"id"`
	ParentID *string    `json:"parentId"`
	Provider string     `json:"provider"`
	ModelID  string     `json:"modelId"`
	Message  rawMessage `json:"message"`
	Usage    *rawUsage  `json:"usage"`
}

type rawMessage struct {
	Role       string    `json:"role"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Usage      *rawUsage `json:"usage"`
	StopReason string    `json:"stopReason"`
}

type rawUsage struct {
	Input       *uint64 `json:"input"`
	Output      *uint64 `json:"output"`
	CacheRead   *uint64 `json:"cacheRead"`
	CacheWrite  *uint64 `json:"cacheWrite"`
	Reasoning   *uint64 `json:"reasoning"`
	TotalTokens *uint64 `json:"totalTokens"`
}

type storedEntry struct {
	id         string
	parentID   string
	typeName   string
	provider   string
	model      string
	usage      *rawUsage
	assistant  bool
	stopReason string
}

func parseSessionFile(path string) (parsedSession, error) {
	file, err := os.Open(path)
	if err != nil {
		return parsedSession{}, err
	}
	defer file.Close()

	lineNumber := 0
	entries := make(map[string]storedEntry)
	var sessionID string
	var lastID string

	err = readSessionLines(file, func(line []byte) error {
		lineNumber++
		if lineNumber == 1 {
			var header rawHeader
			if err := json.Unmarshal(line, &header); err != nil {
				return malformed(fmt.Errorf("line %d", lineNumber))
			}
			if header.Type != "session" || header.ID == "" {
				return malformed(errors.New("missing session header"))
			}
			sessionID = header.ID
			return nil
		}
		var raw rawEntry
		if err := json.Unmarshal(line, &raw); err != nil {
			return malformed(fmt.Errorf("line %d", lineNumber))
		}
		if raw.ID == "" {
			return nil
		}
		entry := storedEntry{id: raw.ID, typeName: raw.Type}
		if raw.ParentID != nil {
			entry.parentID = *raw.ParentID
		}
		switch raw.Type {
		case "message":
			if raw.Message.Provider != "" || raw.Message.Model != "" || raw.Message.Usage != nil {
				entry.provider = raw.Message.Provider
				entry.model = raw.Message.Model
				entry.usage = raw.Message.Usage
				entry.assistant = raw.Message.Role == "assistant"
				entry.stopReason = raw.Message.StopReason
			}
		case "model_change":
			entry.provider = raw.Provider
			entry.model = raw.ModelID
		case "compaction", "branch_summary":
			entry.usage = raw.Usage
		}
		entries[raw.ID] = entry
		lastID = raw.ID
		return nil
	})
	if err != nil {
		return parsedSession{}, err
	}
	if sessionID == "" {
		return parsedSession{}, malformed(errors.New("empty session"))
	}

	pathIDs := activePath(lastID, entries)
	var freshInput, cacheRead, cacheWrite, output, reasoning uint64
	var provider, model string
	var contextUsed *uint64
	for _, id := range pathIDs {
		entry := entries[id]
		if entry.provider != "" {
			provider = entry.provider
		}
		if entry.model != "" {
			model = entry.model
		}
		if entry.typeName == "compaction" {
			contextUsed = nil
		}
		if entry.usage == nil {
			continue
		}
		input, out, read, write, think, err := normalizeRawUsage(*entry.usage)
		if err != nil {
			return parsedSession{}, malformed(err)
		}
		if !addInto(&freshInput, input) || !addInto(&cacheRead, read) || !addInto(&cacheWrite, write) || !addInto(&output, out) || !addInto(&reasoning, think) {
			return parsedSession{}, malformed(errors.New("token count overflow"))
		}
		if entry.assistant && entry.stopReason != "aborted" && entry.stopReason != "error" {
			used := value(entry.usage.TotalTokens)
			if used == 0 {
				used, _ = addValues(input, out, read, write)
			}
			if used > 0 {
				contextUsed = &used
			}
		}
	}
	tokens, err := usage.NewTokens(freshInput, cacheRead, cacheWrite, output, reasoning)
	if err != nil {
		return parsedSession{}, malformed(err)
	}
	return parsedSession{sessionID: sessionID, provider: provider, model: model, tokens: tokens, contextUsed: contextUsed}, nil
}

func readSessionLines(reader io.Reader, visit func([]byte) error) error {
	buffered := bufio.NewReader(reader)
	visited := false
	for {
		line, terminated, readErr := readBoundedLine(buffered)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) != 0 {
			if err := visit(trimmed); err != nil {
				if errors.Is(readErr, io.EOF) && !terminated && visited {
					return nil
				}
				return err
			}
			visited = true
		}
		if readErr != nil {
			return nil
		}
	}
}

func readBoundedLine(reader *bufio.Reader) ([]byte, bool, error) {
	line := make([]byte, 0, 64<<10)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > maxJSONLine-len(line) {
			return nil, false, errors.New("Pi session record exceeds 8 MiB")
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return line, true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return line, false, io.EOF
		default:
			return nil, false, errors.New("Pi session could not be read")
		}
	}
}

func activePath(lastID string, entries map[string]storedEntry) []string {
	if lastID == "" {
		return nil
	}
	var reversed []string
	seen := map[string]struct{}{}
	for id := lastID; id != ""; {
		entry, ok := entries[id]
		if !ok {
			break
		}
		if _, exists := seen[id]; exists {
			break
		}
		seen[id] = struct{}{}
		reversed = append(reversed, id)
		id = entry.parentID
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

func normalizeRawUsage(raw rawUsage) (input, output, cacheRead, cacheWrite, reasoning uint64, err error) {
	input = value(raw.Input)
	output = value(raw.Output)
	cacheRead = value(raw.CacheRead)
	cacheWrite = value(raw.CacheWrite)
	reasoning = value(raw.Reasoning)
	if reasoning > output {
		return 0, 0, 0, 0, 0, errors.New("reasoning exceeds output")
	}
	if raw.TotalTokens != nil {
		total, ok := addValues(input, output, cacheRead, cacheWrite)
		if !ok {
			return 0, 0, 0, 0, 0, errors.New("token count overflow")
		}
		if total != *raw.TotalTokens {
			return 0, 0, 0, 0, 0, errors.New("Pi usage total is inconsistent")
		}
	}
	return input, output, cacheRead, cacheWrite, reasoning, nil
}

func value(pointer *uint64) uint64 {
	if pointer == nil {
		return 0
	}
	return *pointer
}

func addInto(dst *uint64, value uint64) bool {
	if math.MaxUint64-*dst < value {
		return false
	}
	*dst += value
	return true
}

func addValues(values ...uint64) (uint64, bool) {
	var total uint64
	for _, value := range values {
		if math.MaxUint64-total < value {
			return 0, false
		}
		total += value
	}
	return total, true
}

func malformed(err error) error {
	return fmt.Errorf("%w: Pi session usage counters: %v", basecollector.ErrMalformed, err)
}
