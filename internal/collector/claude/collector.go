// Package claude collects token usage from local Claude Code transcripts.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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
	sourceName            = "claude-transcript"
	defaultFallbackWindow = 2 * time.Hour
	maxFutureClockSkew    = 5 * time.Minute
	maxJSONLine           = 8 << 20
)

// Collector reads Claude Code JSONL transcripts without retaining prompt or
// tool content. Its zero value is usable and follows Claude Code's standard
// configuration-directory discovery.
type Collector struct {
	configDir      string
	projectsDir    string
	fallbackWindow time.Duration
	now            func() time.Time
	cacheMu        sync.Mutex
	cache          map[string]transcriptCache
	locks          [32]sync.Mutex
}

// Option configures a Collector.
type Option func(*Collector)

// WithConfigDir uses path as CLAUDE_CONFIG_DIR. Transcripts are discovered in
// its projects subdirectory.
func WithConfigDir(path string) Option {
	return func(c *Collector) {
		c.configDir = path
		c.projectsDir = ""
	}
}

// WithProjectsDir directly specifies Claude Code's projects directory. This
// is primarily useful for tests and nonstandard installations.
func WithProjectsDir(path string) Option {
	return func(c *Collector) {
		c.projectsDir = path
	}
}

// WithFallbackWindow changes how recently a transcript must have been active
// to qualify for CWD fallback. A non-positive duration disables fallback.
func WithFallbackWindow(window time.Duration) Option {
	return func(c *Collector) {
		c.fallbackWindow = window
	}
}

// WithNow supplies the clock used by cautious CWD/recency fallback.
func WithNow(now func() time.Time) Option {
	return func(c *Collector) {
		if now != nil {
			c.now = now
		}
	}
}

// New constructs a Claude Code collector.
func New(options ...Option) *Collector {
	c := &Collector{
		fallbackWindow: defaultFallbackWindow,
		now:            time.Now,
		cache:          make(map[string]transcriptCache),
	}
	for _, option := range options {
		if option != nil {
			option(c)
		}
	}
	return c
}

// Harnesses implements collector.Collector.
func (*Collector) Harnesses() []string { return []string{"claude"} }

// Collect locates and aggregates a single Claude Code session transcript.
func (c *Collector) Collect(ctx context.Context, target basecollector.Target) (usage.Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if harness := basecollector.CanonicalHarness(target.Harness); harness != "" && harness != "claude" {
		return usage.Snapshot{}, basecollector.ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return usage.Snapshot{}, err
	}

	projectsDir, err := c.projectRoot()
	if err != nil {
		return usage.Snapshot{}, err
	}

	resolved, err := c.resolve(ctx, projectsDir, target)
	if err != nil {
		return usage.Snapshot{}, err
	}

	parsed, err := c.parseTranscript(ctx, resolved.path)
	if err != nil {
		return usage.Snapshot{}, err
	}
	if parsed.sessionID != "" && parsed.sessionID != resolved.sessionID {
		return usage.Snapshot{}, safeError(basecollector.ErrMalformed)
	}

	tokens, err := parsed.tokens()
	if err != nil {
		return usage.Snapshot{}, safeError(basecollector.ErrMalformed)
	}

	now := time.Now
	if c != nil && c.now != nil {
		now = c.now
	}
	snapshot := usage.Snapshot{
		SchemaVersion:  usage.SchemaVersion,
		PaneID:         target.PaneID,
		WorkspaceID:    target.WorkspaceID,
		Harness:        "claude",
		HarnessVersion: parsed.version,
		SessionID:      resolved.sessionID,
		Model:          parsed.model,
		State:          target.State,
		Tokens:         tokens,
		Source:         sourceName,
		Confidence:     resolved.confidence,
		CollectedAt:    now().UTC(),
	}
	if err := snapshot.Validate(); err != nil {
		return usage.Snapshot{}, safeError(basecollector.ErrMalformed)
	}
	return snapshot, nil
}

type resolvedTranscript struct {
	path       string
	sessionID  string
	confidence usage.Confidence
}

func (c *Collector) projectRoot() (string, error) {
	if c != nil && strings.TrimSpace(c.projectsDir) != "" {
		return filepath.Clean(c.projectsDir), nil
	}

	configDir := ""
	if c != nil {
		configDir = strings.TrimSpace(c.configDir)
	}
	if configDir == "" {
		configDir = strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	}
	if configDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("claude config directory is unavailable")
		}
		configDir = filepath.Join(homeDir, ".claude")
	}
	return filepath.Join(filepath.Clean(configDir), "projects"), nil
}

func (c *Collector) resolve(ctx context.Context, projectsDir string, target basecollector.Target) (resolvedTranscript, error) {
	sessionID := strings.TrimSpace(target.SessionID)
	if sessionID != "" {
		path, err := findExact(ctx, projectsDir, sessionID)
		if err != nil {
			return resolvedTranscript{}, err
		}
		return resolvedTranscript{
			path:       path,
			sessionID:  sessionID,
			confidence: usage.ConfidenceExact,
		}, nil
	}

	window := defaultFallbackWindow
	now := time.Now
	if c != nil {
		window = c.fallbackWindow
		if c.now != nil {
			now = c.now
		}
	}
	if window <= 0 || !filepath.IsAbs(target.CWD) {
		return resolvedTranscript{}, safeError(basecollector.ErrSessionNotFound)
	}
	return findByCWD(ctx, projectsDir, filepath.Clean(target.CWD), now(), window)
}

func findExact(ctx context.Context, projectsDir, sessionID string) (string, error) {
	// Comparing a basename rather than constructing a caller-controlled path
	// also prevents separators in a malformed session ID from escaping root.
	wanted := sessionID + ".jsonl"
	var matches []string
	err := walkTranscripts(ctx, projectsDir, func(path string, entry os.DirEntry) error {
		if entry.Name() == wanted {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", safeError(basecollector.ErrSessionNotFound)
	case 1:
		return matches[0], nil
	default:
		return "", safeError(basecollector.ErrAmbiguous)
	}
}

func findByCWD(ctx context.Context, projectsDir, cwd string, now time.Time, window time.Duration) (resolvedTranscript, error) {
	var candidates []resolvedTranscript
	err := walkTranscripts(ctx, projectsDir, func(path string, _ os.DirEntry) error {
		metadata, err := inspectMetadata(ctx, path)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err != nil || metadata.cwd == "" || filepath.Clean(metadata.cwd) != cwd {
			return nil
		}
		if metadata.sessionID == "" || metadata.updatedAt.IsZero() {
			return nil
		}
		age := now.Sub(metadata.updatedAt)
		if age < -maxFutureClockSkew || age > window {
			return nil
		}
		candidates = append(candidates, resolvedTranscript{
			path:       path,
			sessionID:  metadata.sessionID,
			confidence: usage.ConfidenceEstimated,
		})
		return nil
	})
	if err != nil {
		return resolvedTranscript{}, err
	}
	switch len(candidates) {
	case 0:
		return resolvedTranscript{}, safeError(basecollector.ErrSessionNotFound)
	case 1:
		return candidates[0], nil
	default:
		return resolvedTranscript{}, safeError(basecollector.ErrAmbiguous)
	}
}

func walkTranscripts(ctx context.Context, projectsDir string, visit func(string, os.DirEntry) error) error {
	err := filepath.WalkDir(projectsDir, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			// Do not return an os.PathError: transcript paths can encode private
			// workspace names and must not escape through error strings.
			return errors.New("claude projects directory could not be read")
		}
		if entry.IsDir() {
			// Claude stores child-agent transcripts below
			// <root-session>/subagents. Version 0.1 reports the direct/root
			// session only, and including these files would make cwd fallback
			// ambiguous whenever a root session delegated work.
			if entry.Name() == "subagents" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		return visit(path, entry)
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		return safeError(basecollector.ErrSessionNotFound)
	}
	return errors.New("claude projects directory could not be read")
}

type transcriptMetadata struct {
	sessionID string
	cwd       string
	updatedAt time.Time
}

func inspectMetadata(ctx context.Context, path string) (transcriptMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return transcriptMetadata{}, errors.New("claude transcript could not be read")
	}
	defer file.Close()

	metadata := transcriptMetadata{}
	err = readLines(ctx, file, func(line []byte, _ int) error {
		var envelope rawEnvelope
		if json.Unmarshal(line, &envelope) != nil {
			return nil
		}
		if envelope.SessionID != "" {
			if metadata.sessionID != "" && metadata.sessionID != envelope.SessionID {
				return errInconsistentSession
			}
			metadata.sessionID = envelope.SessionID
		}
		if envelope.CWD != "" {
			metadata.cwd = envelope.CWD
		}
		if timestamp, err := time.Parse(time.RFC3339Nano, envelope.Timestamp); err == nil && timestamp.After(metadata.updatedAt) {
			metadata.updatedAt = timestamp
		}
		return nil
	})
	if err != nil {
		return transcriptMetadata{}, err
	}
	if metadata.sessionID == "" {
		metadata.sessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	if metadata.updatedAt.IsZero() {
		if info, err := file.Stat(); err == nil {
			metadata.updatedAt = info.ModTime()
		}
	}
	return metadata, nil
}

var errInconsistentSession = errors.New("inconsistent transcript session")

type rawEnvelope struct {
	Type        string          `json:"type"`
	SessionID   string          `json:"sessionId"`
	CWD         string          `json:"cwd"`
	Version     string          `json:"version"`
	Timestamp   string          `json:"timestamp"`
	UUID        string          `json:"uuid"`
	RequestID   string          `json:"requestId"`
	Message     json.RawMessage `json:"message"`
	IsSidechain bool            `json:"isSidechain"`
}

type rawMessage struct {
	ID    string          `json:"id"`
	Role  string          `json:"role"`
	Model string          `json:"model"`
	Usage json.RawMessage `json:"usage"`
}

type parsedTranscript struct {
	messages  map[string]messageUsage
	sessionID string
	model     string
	version   string
	valid     bool
}

type transcriptCache struct {
	parsed      parsedTranscript
	offset      int64
	records     int
	modTime     time.Time
	fileInfo    os.FileInfo
	fingerprint [sha256.Size]byte
}

func (c *Collector) parseTranscript(ctx context.Context, path string) (parsedTranscript, error) {
	lock := c.transcriptLock(path)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return parsedTranscript{}, err
	}

	file, err := os.Open(path)
	if err != nil {
		return parsedTranscript{}, errors.New("claude transcript could not be read")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return parsedTranscript{}, errors.New("claude transcript could not be read")
	}

	empty := transcriptCache{parsed: parsedTranscript{messages: make(map[string]messageUsage)}}
	base := empty
	if cached, ok := c.cachedTranscript(path); ok && cacheGenerationMatches(info, cached) {
		switch {
		case info.Size() == cached.offset && info.ModTime().Equal(cached.modTime):
			fingerprint, fingerprintErr := fingerprintAt(ctx, file, cached.offset)
			if fingerprintErr != nil {
				if err := ctx.Err(); err != nil {
					return parsedTranscript{}, err
				}
				return parsedTranscript{}, errors.New("claude transcript could not be read")
			}
			if fingerprint != cached.fingerprint {
				break
			}
			if err := ctx.Err(); err != nil {
				return parsedTranscript{}, err
			}
			return cached.parsed, nil
		case info.Size() > cached.offset:
			base = cached
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := file.Seek(base.offset, io.SeekStart); err != nil {
			return parsedTranscript{}, errors.New("claude transcript could not be read")
		}
		parsed := cloneParsedTranscript(base.parsed)
		stats, parseErr := readLinesFrom(ctx, file, base.records, base.records > 0, func(line []byte, lineNumber int) error {
			return consumeTranscriptLine(&parsed, line, lineNumber)
		})
		latestInfo, statErr := file.Stat()
		if base.offset > 0 {
			stable := statErr == nil && cacheGenerationMatches(latestInfo, base)
			if stable {
				fingerprint, fingerprintErr := fingerprintAt(ctx, file, base.offset)
				stable = fingerprintErr == nil && fingerprint == base.fingerprint
			}
			if !stable {
				// The transcript was replaced or rewritten while/after the cached
				// suffix was read. Retry once from byte zero so removed messages
				// can never remain in the normalized total.
				base = empty
				continue
			}
		}
		if parseErr != nil {
			if errors.Is(parseErr, context.Canceled) || errors.Is(parseErr, context.DeadlineExceeded) {
				return parsedTranscript{}, parseErr
			}
			return parsedTranscript{}, safeError(basecollector.ErrMalformed)
		}
		if !parsed.valid {
			return parsedTranscript{}, safeError(basecollector.ErrMalformed)
		}
		if stats.clean || stats.ignoredTail {
			offset := base.offset + stats.bytes
			if statErr == nil && latestInfo.Size() >= offset {
				fingerprint := base.fingerprint
				var fingerprintErr error
				if offset != base.offset {
					fingerprint, fingerprintErr = fingerprintAt(ctx, file, offset)
				}
				if fingerprintErr != nil {
					if err := ctx.Err(); err != nil {
						return parsedTranscript{}, err
					}
				}
				if fingerprintErr == nil {
					c.storeTranscript(path, transcriptCache{
						parsed:      parsed,
						offset:      offset,
						records:     base.records + stats.records,
						modTime:     latestInfo.ModTime(),
						fileInfo:    latestInfo,
						fingerprint: fingerprint,
					})
				}
			}
		}
		return parsed, nil
	}
	return parsedTranscript{}, safeError(basecollector.ErrMalformed)
}

func consumeTranscriptLine(parsed *parsedTranscript, line []byte, lineNumber int) error {
	var envelope rawEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return errMalformedLine
	}
	sessionID := parsed.sessionID
	if envelope.SessionID != "" {
		if sessionID != "" && sessionID != envelope.SessionID {
			return errInconsistentSession
		}
		sessionID = envelope.SessionID
	}
	version := parsed.version
	if envelope.Version != "" {
		version = envelope.Version
	}
	if envelope.Type != "assistant" {
		parsed.sessionID = sessionID
		parsed.version = version
		return nil
	}

	var message rawMessage
	if len(envelope.Message) == 0 || string(envelope.Message) == "null" {
		parsed.sessionID = sessionID
		parsed.version = version
		return nil
	}
	if err := json.Unmarshal(envelope.Message, &message); err != nil {
		return errMalformedLine
	}
	if message.Role != "" && message.Role != "assistant" {
		parsed.sessionID = sessionID
		parsed.version = version
		return nil
	}
	if len(message.Usage) == 0 || string(message.Usage) == "null" {
		parsed.sessionID = sessionID
		parsed.version = version
		return nil
	}
	candidate, err := decodeUsage(message.Usage)
	if err != nil {
		return errMalformedLine
	}
	parsed.sessionID = sessionID
	parsed.version = version
	parsed.valid = true
	if message.Model != "" {
		parsed.model = message.Model
	}

	key := message.ID
	if key == "" {
		key = envelope.RequestID
	}
	if key == "" {
		key = envelope.UUID
	}
	if key == "" {
		key = fmt.Sprintf("anonymous:%d", lineNumber)
	}
	if previous, exists := parsed.messages[key]; exists {
		parsed.messages[key] = previous.mergeHighest(candidate)
	} else {
		parsed.messages[key] = candidate
	}
	return nil
}

func cloneParsedTranscript(value parsedTranscript) parsedTranscript {
	clone := value
	clone.messages = make(map[string]messageUsage, len(value.messages))
	for key, message := range value.messages {
		clone.messages[key] = message
	}
	return clone
}

func (c *Collector) cachedTranscript(path string) (transcriptCache, bool) {
	if c == nil {
		return transcriptCache{}, false
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	value, ok := c.cache[path]
	return value, ok
}

func (c *Collector) transcriptLock(path string) *sync.Mutex {
	if c == nil {
		return &nilCollectorLock
	}
	// Fixed lock striping keeps collection for one transcript serialized without
	// retaining every historical path in a long-lived dashboard process.
	hash := uint32(2166136261)
	for index := 0; index < len(path); index++ {
		hash ^= uint32(path[index])
		hash *= 16777619
	}
	return &c.locks[hash%uint32(len(c.locks))]
}

var nilCollectorLock sync.Mutex

func cacheGenerationMatches(info os.FileInfo, cached transcriptCache) bool {
	return cached.fileInfo != nil && os.SameFile(cached.fileInfo, info) && info.Size() >= cached.offset
}

func fingerprintAt(ctx context.Context, file *os.File, offset int64) ([sha256.Size]byte, error) {
	if offset < 0 {
		return [sha256.Size]byte{}, errors.New("invalid transcript offset")
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var position int64
	for position < offset {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, err
		}
		want := int64(len(buffer))
		if remaining := offset - position; remaining < want {
			want = remaining
		}
		read, err := file.ReadAt(buffer[:want], position)
		if err != nil && !errors.Is(err, io.EOF) {
			return [sha256.Size]byte{}, err
		}
		if read != int(want) {
			return [sha256.Size]byte{}, io.ErrUnexpectedEOF
		}
		_, _ = hash.Write(buffer[:read])
		position += int64(read)
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func (c *Collector) storeTranscript(path string, value transcriptCache) {
	if c == nil {
		return
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.cache == nil {
		c.cache = make(map[string]transcriptCache)
	}
	if _, exists := c.cache[path]; !exists && len(c.cache) >= 128 {
		for key := range c.cache {
			delete(c.cache, key)
			break
		}
	}
	c.cache[path] = value
}

var errMalformedLine = errors.New("malformed JSONL line")

// readLines allows a malformed final record to be ignored when at least one
// earlier record was usable. Claude Code can be observed while it is midway
// through appending that final JSON object.
func readLines(ctx context.Context, reader io.Reader, visit func([]byte, int) error) error {
	_, err := readLinesFrom(ctx, reader, 0, false, visit)
	return err
}

type readStats struct {
	bytes       int64
	records     int
	clean       bool
	ignoredTail bool
}

func readLinesFrom(ctx context.Context, reader io.Reader, startLine int, previouslyVisited bool, visit func([]byte, int) error) (readStats, error) {
	buffered := bufio.NewReader(reader)
	lineNumber := startLine
	visited := previouslyVisited
	var stats readStats
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		line, terminated, readErr := readBoundedLine(buffered)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return stats, readErr
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) != 0 {
			lineNumber++
			if err := visit(trimmed, lineNumber); err != nil {
				if errors.Is(err, errMalformedLine) {
					// Only an unterminated final record can be an in-progress
					// append. A newline-terminated malformed record is complete
					// schema drift and must fail rather than publish stale usage.
					if errors.Is(readErr, io.EOF) && !terminated && visited {
						stats.ignoredTail = true
						return stats, nil
					}
					return stats, errMalformedLine
				} else {
					return stats, err
				}
			} else {
				visited = true
			}
		}
		if terminated {
			stats.bytes += int64(len(line))
			if len(trimmed) != 0 {
				stats.records++
			}
		}
		if readErr != nil {
			stats.clean = len(line) == 0
			return stats, nil
		}
	}
}

func readBoundedLine(reader *bufio.Reader) ([]byte, bool, error) {
	line := make([]byte, 0, min(64<<10, maxJSONLine))
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > maxJSONLine-len(line) {
			return nil, false, errors.New("claude transcript record exceeds 8 MiB")
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
			return nil, false, errors.New("claude transcript could not be read")
		}
	}
}

type rawUsage struct {
	InputTokens              *uint64                    `json:"input_tokens"`
	CacheReadInputTokens     *uint64                    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *uint64                    `json:"cache_creation_input_tokens"`
	OutputTokens             *uint64                    `json:"output_tokens"`
	ReasoningTokens          *uint64                    `json:"reasoning_tokens"`
	ThinkingTokens           *uint64                    `json:"thinking_tokens"`
	CacheCreation            map[string]json.RawMessage `json:"cache_creation"`
	OutputTokenDetails       map[string]json.RawMessage `json:"output_tokens_details"`
	Iterations               []json.RawMessage          `json:"iterations"`
}

type messageUsage struct {
	freshInput uint64
	cacheRead  uint64
	cacheWrite uint64
	output     uint64
	reasoning  uint64
}

func decodeUsage(data json.RawMessage) (messageUsage, error) {
	var raw rawUsage
	if err := json.Unmarshal(data, &raw); err != nil {
		return messageUsage{}, err
	}
	if len(raw.Iterations) > 0 {
		var total messageUsage
		for _, encoded := range raw.Iterations {
			var iteration rawUsage
			if err := json.Unmarshal(encoded, &iteration); err != nil {
				return messageUsage{}, err
			}
			candidate, err := decodeFlatUsage(iteration)
			if err != nil {
				return messageUsage{}, err
			}
			total, err = total.add(candidate)
			if err != nil {
				return messageUsage{}, err
			}
		}
		return total, nil
	}
	return decodeFlatUsage(raw)
}

func decodeFlatUsage(raw rawUsage) (messageUsage, error) {
	candidate := messageUsage{
		freshInput: value(raw.InputTokens),
		cacheRead:  value(raw.CacheReadInputTokens),
		output:     value(raw.OutputTokens),
	}
	if raw.CacheCreationInputTokens != nil {
		candidate.cacheWrite = *raw.CacheCreationInputTokens
	} else {
		var err error
		candidate.cacheWrite, err = sumBreakdown(raw.CacheCreation,
			"ephemeral_5m_input_tokens",
			"ephemeral_1h_input_tokens",
		)
		if err != nil {
			return messageUsage{}, err
		}
	}

	candidate.reasoning = max(value(raw.ReasoningTokens), value(raw.ThinkingTokens))
	if details, err := breakdownValue(raw.OutputTokenDetails, "reasoning_tokens"); err != nil {
		return messageUsage{}, err
	} else {
		candidate.reasoning = max(candidate.reasoning, details)
	}
	if candidate.reasoning > candidate.output {
		return messageUsage{}, errors.New("reasoning exceeds inclusive output")
	}
	if _, ok := candidate.total(); !ok {
		return messageUsage{}, errors.New("token count overflow")
	}
	return candidate, nil
}

func (m messageUsage) add(other messageUsage) (messageUsage, error) {
	values := [5]uint64{}
	pairs := [][2]uint64{
		{m.freshInput, other.freshInput},
		{m.cacheRead, other.cacheRead},
		{m.cacheWrite, other.cacheWrite},
		{m.output, other.output},
		{m.reasoning, other.reasoning},
	}
	for index, pair := range pairs {
		value, ok := safeAdd(pair[0], pair[1])
		if !ok {
			return messageUsage{}, errors.New("token count overflow")
		}
		values[index] = value
	}
	if values[4] > values[3] {
		return messageUsage{}, errors.New("reasoning exceeds inclusive output")
	}
	return messageUsage{
		freshInput: values[0],
		cacheRead:  values[1],
		cacheWrite: values[2],
		output:     values[3],
		reasoning:  values[4],
	}, nil
}

func value(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}

func sumBreakdown(values map[string]json.RawMessage, keys ...string) (uint64, error) {
	var total uint64
	for _, key := range keys {
		value, err := breakdownValue(values, key)
		if err != nil {
			return 0, err
		}
		var ok bool
		total, ok = safeAdd(total, value)
		if !ok {
			return 0, errors.New("token count overflow")
		}
	}
	return total, nil
}

func breakdownValue(values map[string]json.RawMessage, key string) (uint64, error) {
	data, exists := values[key]
	if !exists || len(data) == 0 || string(data) == "null" {
		return 0, nil
	}
	var value uint64
	if err := json.Unmarshal(data, &value); err != nil {
		return 0, err
	}
	return value, nil
}

func (m messageUsage) total() (uint64, bool) {
	return safeAdd(m.freshInput, m.cacheRead, m.cacheWrite, m.output)
}

func (m messageUsage) mergeHighest(other messageUsage) messageUsage {
	// Streamed transcript records are cumulative but can omit fields while an
	// event is in flight. Keeping each category's highest observation recovers
	// the most complete record without ever summing the same message twice.
	return messageUsage{
		freshInput: max(m.freshInput, other.freshInput),
		cacheRead:  max(m.cacheRead, other.cacheRead),
		cacheWrite: max(m.cacheWrite, other.cacheWrite),
		output:     max(m.output, other.output),
		reasoning:  max(m.reasoning, other.reasoning),
	}
}

func (p parsedTranscript) tokens() (usage.Tokens, error) {
	var freshInput, cacheRead, cacheWrite, output, reasoning uint64
	var ok bool
	for _, message := range p.messages {
		if freshInput, ok = safeAdd(freshInput, message.freshInput); !ok {
			return usage.Tokens{}, errors.New("token count overflow")
		}
		if cacheRead, ok = safeAdd(cacheRead, message.cacheRead); !ok {
			return usage.Tokens{}, errors.New("token count overflow")
		}
		if cacheWrite, ok = safeAdd(cacheWrite, message.cacheWrite); !ok {
			return usage.Tokens{}, errors.New("token count overflow")
		}
		if output, ok = safeAdd(output, message.output); !ok {
			return usage.Tokens{}, errors.New("token count overflow")
		}
		if reasoning, ok = safeAdd(reasoning, message.reasoning); !ok {
			return usage.Tokens{}, errors.New("token count overflow")
		}
	}
	return usage.NewTokens(freshInput, cacheRead, cacheWrite, output, reasoning)
}

func safeAdd(values ...uint64) (uint64, bool) {
	var total uint64
	for _, value := range values {
		if math.MaxUint64-total < value {
			return 0, false
		}
		total += value
	}
	return total, true
}

func max(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func safeError(sentinel error) error {
	return fmt.Errorf("%w: claude transcript", sentinel)
}

var _ basecollector.Collector = (*Collector)(nil)
