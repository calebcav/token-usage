package opencode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/calebcav/token-usage/internal/usage"
)

const (
	contextCacheTTL        = 10 * time.Minute
	contextResolverTimeout = 5 * time.Second
	maxResolverOutput      = 4 << 20
	maxResolverError       = 4 << 10
	maxContextCacheBytes   = 512
)

var requiredMessageColumns = []string{"id", "session_id", "time_created", "data"}

// ContextLimitResolver provides the runtime-resolved limit for an exact
// OpenCode provider/model pair.
type ContextLimitResolver interface {
	ResolveContextLimit(ctx context.Context, cwd, providerID, modelID string) (uint64, error)
}

type messageContext struct {
	Provider string
	Model    string
	Used     uint64
}

// latestMessageContext projects only model identifiers and token counters out
// of message.data. Message and part content never crosses the SQLite boundary.
func latestMessageContext(ctx context.Context, db *sql.DB, sessionID string) (messageContext, bool) {
	if !hasCurrentMessageTable(ctx, db) {
		return messageContext{}, false
	}

	var provider, model, total, input, output, reasoning, cacheRead, cacheWrite any
	err := db.QueryRowContext(ctx, `
		SELECT
			json_extract(data, '$.providerID'),
			json_extract(data, '$.modelID'),
			json_extract(data, '$.tokens.total'),
			json_extract(data, '$.tokens.input'),
			json_extract(data, '$.tokens.output'),
			json_extract(data, '$.tokens.reasoning'),
			json_extract(data, '$.tokens.cache.read'),
			json_extract(data, '$.tokens.cache.write')
		FROM "message"
		WHERE session_id = ?
			AND json_extract(data, '$.role') = 'assistant'
			AND json_extract(data, '$.time.completed') IS NOT NULL
			AND (
				COALESCE(json_extract(data, '$.tokens.input'), 0) != 0
				OR COALESCE(json_extract(data, '$.tokens.output'), 0) != 0
				OR COALESCE(json_extract(data, '$.tokens.reasoning'), 0) != 0
				OR COALESCE(json_extract(data, '$.tokens.cache.read'), 0) != 0
				OR COALESCE(json_extract(data, '$.tokens.cache.write'), 0) != 0
			)
		ORDER BY time_created DESC, id DESC
		LIMIT 1`, sessionID).Scan(
		&provider,
		&model,
		&total,
		&input,
		&output,
		&reasoning,
		&cacheRead,
		&cacheWrite,
	)
	if err != nil {
		return messageContext{}, false
	}

	providerID, ok := provider.(string)
	if !ok || !validResolverID(providerID) {
		return messageContext{}, false
	}
	modelID, ok := model.(string)
	if !ok || !validResolverID(modelID) {
		return messageContext{}, false
	}

	values := []struct {
		name  string
		value any
	}{
		{"tokens.input", input},
		{"tokens.output", output},
		{"tokens.reasoning", reasoning},
		{"tokens.cache.read", cacheRead},
		{"tokens.cache.write", cacheWrite},
	}
	counts := make([]uint64, 0, len(values))
	for _, value := range values {
		count, ok := projectedTokenCount(value.value)
		if !ok {
			return messageContext{}, false
		}
		counts = append(counts, count)
	}
	if total != nil {
		if _, ok := projectedTokenCount(total); !ok {
			return messageContext{}, false
		}
	}

	used, ok := checkedSum(counts...)
	if !ok {
		return messageContext{}, false
	}
	return messageContext{Provider: providerID, Model: modelID, Used: used}, true
}

func hasCurrentMessageTable(ctx context.Context, db *sql.DB) bool {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info("message")`)
	if err != nil {
		return false
	}
	defer rows.Close()

	columns := make(map[string]struct{})
	for rows.Next() {
		var (
			cid          int
			name         string
			columnType   string
			notNull      int
			defaultValue any
			primaryKey   int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false
		}
		columns[strings.ToLower(name)] = struct{}{}
	}
	if rows.Err() != nil {
		return false
	}
	for _, name := range requiredMessageColumns {
		if _, ok := columns[name]; !ok {
			return false
		}
	}
	return true
}

func projectedTokenCount(value any) (uint64, bool) {
	count, ok := value.(int64)
	if !ok || count < 0 {
		return 0, false
	}
	return uint64(count), true
}

func checkedSum(values ...uint64) (uint64, bool) {
	var total uint64
	for _, value := range values {
		var ok bool
		total, ok = checkedAdd(total, value)
		if !ok {
			return 0, false
		}
	}
	return total, true
}

func validResolverID(value string) bool {
	if strings.TrimSpace(value) == "" || len(value) > 512 {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func (c *Collector) resolveContextLimit(ctx context.Context, cwd, providerID, modelID string) (uint64, bool) {
	if c.contextResolver == nil || !validResolverID(providerID) || !validResolverID(modelID) {
		return 0, false
	}
	cwd = cleanCWD(cwd)
	cachePath := contextCachePath(c.contextCacheDir, cwd, providerID, modelID)
	if limit, ok := readContextCache(cachePath, c.now()); ok {
		return limit, limit > 0
	}
	key := contextCacheKey(cwd, providerID, modelID)
	resolved := c.contextGroup.DoChan(key, func() (any, error) {
		if limit, ok := readContextCache(cachePath, c.now()); ok {
			return limit, nil
		}
		resolverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), contextResolverTimeout)
		defer cancel()
		limit, err := c.contextResolver.ResolveContextLimit(resolverCtx, cwd, providerID, modelID)
		if err != nil || limit == 0 {
			return uint64(0), errors.New("OpenCode context limit is unavailable")
		}
		_ = writeContextCache(cachePath, contextCacheEntry{Version: 1, Limit: limit, ResolvedAt: c.now().Unix()})
		return limit, nil
	})
	select {
	case <-ctx.Done():
		return 0, false
	case result := <-resolved:
		if result.Err != nil {
			return 0, false
		}
		limit, ok := result.Val.(uint64)
		return limit, ok && limit > 0
	}
}

func cleanCWD(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	return filepath.Clean(cwd)
}

func defaultContextCacheDir() string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(cacheDir, "token-usage")
}

func contextCachePath(cacheDir, cwd, providerID, modelID string) string {
	if strings.TrimSpace(cacheDir) == "" {
		return ""
	}
	return filepath.Join(cacheDir, "opencode-context-"+contextCacheKey(cwd, providerID, modelID)+".json")
}

func contextCacheKey(cwd, providerID, modelID string) string {
	digest := sha256.New()
	for _, value := range []string{cwd, providerID, modelID} {
		_, _ = io.WriteString(digest, fmt.Sprintf("%d:", len(value)))
		_, _ = io.WriteString(digest, value)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

type contextCacheEntry struct {
	Version    int    `json:"version"`
	Limit      uint64 `json:"limit"`
	ResolvedAt int64  `json:"resolved_at"`
}

func readContextCache(path string, now time.Time) (uint64, bool) {
	if path == "" {
		return 0, false
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxContextCacheBytes+1))
	if err != nil || len(data) > maxContextCacheBytes {
		return 0, false
	}
	var entry contextCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil || entry.Version != 1 {
		return 0, false
	}
	age := now.Sub(time.Unix(entry.ResolvedAt, 0))
	if age < 0 || age >= contextCacheTTL {
		return 0, false
	}
	return entry.Limit, entry.Limit > 0
}

func writeContextCache(path string, entry contextCacheEntry) error {
	if path == "" {
		return errors.New("context cache path is unavailable")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("create context cache directory")
	}
	file, err := os.CreateTemp(directory, ".opencode-context-*")
	if err != nil {
		return errors.New("create context cache file")
	}
	temporaryPath := file.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return errors.New("secure context cache file")
	}
	if err := json.NewEncoder(file).Encode(entry); err != nil {
		file.Close()
		return errors.New("write context cache file")
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return errors.New("sync context cache file")
	}
	if err := file.Close(); err != nil {
		return errors.New("close context cache file")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.New("replace context cache file")
	}
	removeTemporary = false
	if err := os.Chmod(path, 0o600); err != nil {
		return errors.New("secure context cache file")
	}
	return nil
}

type commandContextLimitResolver struct {
	binary  string
	timeout time.Duration
}

func newCommandContextLimitResolver() ContextLimitResolver {
	binary := strings.TrimSpace(os.Getenv("OPENCODE_BIN_PATH"))
	if binary == "" {
		binary = "opencode"
	}
	return commandContextLimitResolver{binary: binary, timeout: contextResolverTimeout}
}

func (r commandContextLimitResolver) ResolveContextLimit(ctx context.Context, cwd, providerID, modelID string) (uint64, error) {
	if !validResolverID(providerID) || !validResolverID(modelID) {
		return 0, errors.New("invalid OpenCode model identifier")
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = contextResolverTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(commandCtx, r.binary, "models", providerID, "--pure", "--verbose")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return killResolverProcessGroup(command.Process.Pid)
	}
	command.WaitDelay = time.Second
	if cwd = cleanCWD(cwd); cwd != "" {
		command.Dir = cwd
	}
	var stdout, stderr cappedBuffer
	stdout.limit = maxResolverOutput
	stderr.limit = maxResolverError
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if command.Process != nil {
		_ = killResolverProcessGroup(command.Process.Pid)
	}
	if err != nil {
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return 0, errors.New("OpenCode model resolution timed out")
		}
		detail := usage.SanitizeText(stderr.String(), 160)
		if detail != "" {
			return 0, fmt.Errorf("OpenCode model resolution failed: %s", detail)
		}
		return 0, errors.New("OpenCode model resolver is unavailable")
	}
	limit, err := parseContextLimitOutput(stdout.Bytes(), providerID, modelID)
	if err != nil {
		if stdout.exceeded {
			return 0, errors.New("OpenCode model output exceeded the safe limit")
		}
		return 0, err
	}
	return limit, nil
}

func killResolverProcessGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *cappedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = buffer.exceeded || len(data) > 0
		return written, nil
	}
	if len(data) > remaining {
		buffer.exceeded = true
		data = data[:remaining]
	}
	_, _ = buffer.Buffer.Write(data)
	return written, nil
}

func parseContextLimitOutput(output []byte, providerID, modelID string) (uint64, error) {
	if !validResolverID(providerID) || !validResolverID(modelID) {
		return 0, errors.New("invalid OpenCode model identifier")
	}
	header := []byte(providerID + "/" + modelID)
	for offset := 0; offset < len(output); {
		end := bytes.IndexByte(output[offset:], '\n')
		if end < 0 {
			end = len(output)
		} else {
			end += offset
		}
		line := bytes.TrimSuffix(output[offset:end], []byte{'\r'})
		if bytes.Equal(line, header) {
			if end == len(output) {
				break
			}
			var block struct {
				Limit *struct {
					Context json.RawMessage `json:"context"`
				} `json:"limit"`
			}
			decoder := json.NewDecoder(bytes.NewReader(output[end+1:]))
			if err := decoder.Decode(&block); err != nil || block.Limit == nil || len(block.Limit.Context) == 0 {
				return 0, errors.New("OpenCode model metadata is malformed")
			}
			var limit uint64
			if err := json.Unmarshal(block.Limit.Context, &limit); err != nil || limit == 0 {
				return 0, errors.New("OpenCode context limit is invalid")
			}
			return limit, nil
		}
		if end == len(output) {
			break
		}
		offset = end + 1
	}
	return 0, errors.New("OpenCode model was not found")
}
