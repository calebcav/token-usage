// Package opencode collects token usage from OpenCode's local SQLite database.
package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	basecollector "github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const (
	sourceName            = "opencode-sqlite"
	busyTimeout           = 1_000
	defaultFallbackWindow = 2 * time.Hour
	maxFutureClockSkew    = 5 * time.Minute
)

var requiredSessionColumns = []string{
	"id",
	"directory",
	"parent_id",
	"model",
	"tokens_input",
	"tokens_output",
	"tokens_reasoning",
	"tokens_cache_read",
	"tokens_cache_write",
	"time_updated",
}

// Config controls how an OpenCode collector locates sessions.
type Config struct {
	// DBPath overrides the standard OpenCode database location. An empty path
	// is resolved by DefaultDBPath.
	DBPath string

	// AllowDirectoryFallback permits an estimated match by Target.CWD only when
	// no native session ID was supplied and exactly one recent session matches.
	AllowDirectoryFallback bool

	// FallbackWindow bounds directory-only matching. Zero uses two hours.
	FallbackWindow time.Duration
}

// Collector reads OpenCode's aggregate per-session counters.
type Collector struct {
	config Config
	now    func() time.Time
}

var _ basecollector.Collector = (*Collector)(nil)

// New creates an OpenCode collector. It does not open the database until
// Collect is called.
func New(config Config) *Collector {
	return &Collector{
		config: config,
		now:    time.Now,
	}
}

func (c *Collector) Harnesses() []string { return []string{"opencode"} }

// DefaultDBPath resolves OpenCode's database without requiring it to exist.
// OPENCODE_DB takes precedence over XDG_DATA_HOME and the conventional home
// directory fallback.
func DefaultDBPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("OPENCODE_DB")); path != "" {
		return path, nil
	}
	if dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dataHome != "" {
		return filepath.Join(dataHome, "opencode", "opencode.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve OpenCode data directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db"), nil
}

// Collect returns the exact OpenCode session requested by target. Directory
// fallback is considered only when no authoritative session ID was supplied.
func (c *Collector) Collect(ctx context.Context, target basecollector.Target) (usage.Snapshot, error) {
	path := strings.TrimSpace(c.config.DBPath)
	if path == "" {
		var err error
		path, err = DefaultDBPath()
		if err != nil {
			return usage.Snapshot{}, err
		}
	}

	db, err := openReadOnly(ctx, path)
	if err != nil {
		return usage.Snapshot{}, err
	}
	defer db.Close()

	if err := validateSchema(ctx, db); err != nil {
		return usage.Snapshot{}, err
	}

	var record sessionRecord
	confidence := usage.ConfidenceExact
	if strings.TrimSpace(target.SessionID) != "" {
		record, err = findExact(ctx, db, target.SessionID)
		if err != nil {
			return usage.Snapshot{}, err
		}
	} else {
		if !c.config.AllowDirectoryFallback || strings.TrimSpace(target.CWD) == "" {
			return usage.Snapshot{}, fmt.Errorf("%w: OpenCode session", basecollector.ErrSessionNotFound)
		}
		window := c.config.FallbackWindow
		if window <= 0 {
			window = defaultFallbackWindow
		}
		now := c.now()
		record, err = findByDirectory(ctx, db, filepath.Clean(target.CWD), now.Add(-window).UnixMilli(), now.Add(maxFutureClockSkew).UnixMilli())
		if err != nil {
			return usage.Snapshot{}, err
		}
		confidence = usage.ConfidenceEstimated
	}

	provider, model, err := parseModel(record.Model)
	if err != nil {
		return usage.Snapshot{}, fmt.Errorf("%w: invalid OpenCode model metadata", basecollector.ErrMalformed)
	}

	inclusiveOutput, ok := checkedAdd(record.Output, record.Reasoning)
	if !ok {
		return usage.Snapshot{}, fmt.Errorf("%w: OpenCode output token count overflows", basecollector.ErrMalformed)
	}
	tokens, err := usage.NewTokens(
		record.Input,
		record.CacheRead,
		record.CacheWrite,
		inclusiveOutput,
		record.Reasoning,
	)
	if err != nil {
		return usage.Snapshot{}, fmt.Errorf("%w: invalid OpenCode token counts", basecollector.ErrMalformed)
	}

	snapshot := usage.Snapshot{
		SchemaVersion: usage.SchemaVersion,
		PaneID:        target.PaneID,
		WorkspaceID:   target.WorkspaceID,
		Harness:       "opencode",
		SessionID:     record.ID,
		Model:         model,
		Provider:      provider,
		State:         target.State,
		Tokens:        tokens,
		Source:        sourceName,
		Confidence:    confidence,
		CollectedAt:   c.now().UTC(),
	}
	if err := snapshot.Validate(); err != nil {
		return usage.Snapshot{}, fmt.Errorf("%w: invalid OpenCode snapshot", basecollector.ErrMalformed)
	}
	return snapshot, nil
}

func openReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenCode database path: %w", err)
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(absPath)}
	query := uri.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout))
	query.Add("_pragma", "query_only(1)")
	uri.RawQuery = query.Encode()

	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, fmt.Errorf("open OpenCode usage database: %w", err)
	}
	// PRAGMA state is connection-local. Keeping one connection ensures every
	// query uses the read-only/query-only connection initialized by the DSN.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open OpenCode usage database: %w", err)
	}

	var queryOnly int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
		db.Close()
		if err != nil {
			return nil, fmt.Errorf("verify OpenCode read-only connection: %w", err)
		}
		return nil, errors.New("verify OpenCode read-only connection: query_only is disabled")
	}
	return db, nil
}

func validateSchema(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info("session")`)
	if err != nil {
		return fmt.Errorf("%w: inspect OpenCode session schema", basecollector.ErrMalformed)
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
			return fmt.Errorf("%w: inspect OpenCode session schema", basecollector.ErrMalformed)
		}
		columns[strings.ToLower(name)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: inspect OpenCode session schema", basecollector.ErrMalformed)
	}

	var missing []string
	for _, name := range requiredSessionColumns {
		if _, ok := columns[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return fmt.Errorf("%w: unsupported OpenCode session schema (missing %s)", basecollector.ErrMalformed, strings.Join(missing, ", "))
	}
	return nil
}

const sessionColumns = `
	id,
	directory,
	parent_id,
	model,
	tokens_input,
	tokens_output,
	tokens_reasoning,
	tokens_cache_read,
	tokens_cache_write,
	time_updated`

type sessionRecord struct {
	ID         string
	Directory  string
	ParentID   sql.NullString
	Model      sql.NullString
	Input      uint64
	Output     uint64
	Reasoning  uint64
	CacheRead  uint64
	CacheWrite uint64
	UpdatedAt  int64
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(scanner rowScanner) (sessionRecord, error) {
	var (
		record                                                     sessionRecord
		input, output, reasoning, cacheRead, cacheWrite, updatedAt int64
	)
	if err := scanner.Scan(
		&record.ID,
		&record.Directory,
		&record.ParentID,
		&record.Model,
		&input,
		&output,
		&reasoning,
		&cacheRead,
		&cacheWrite,
		&updatedAt,
	); err != nil {
		return sessionRecord{}, err
	}

	counts := []struct {
		name  string
		value int64
	}{
		{"tokens_input", input},
		{"tokens_output", output},
		{"tokens_reasoning", reasoning},
		{"tokens_cache_read", cacheRead},
		{"tokens_cache_write", cacheWrite},
	}
	for _, count := range counts {
		if count.value < 0 {
			return sessionRecord{}, fmt.Errorf("%s is negative", count.name)
		}
	}
	record.Input = uint64(input)
	record.Output = uint64(output)
	record.Reasoning = uint64(reasoning)
	record.CacheRead = uint64(cacheRead)
	record.CacheWrite = uint64(cacheWrite)
	record.UpdatedAt = updatedAt
	return record, nil
}

func findExact(ctx context.Context, db *sql.DB, sessionID string) (sessionRecord, error) {
	row := db.QueryRowContext(
		ctx,
		`SELECT `+sessionColumns+` FROM "session" WHERE id = ? LIMIT 1`,
		sessionID,
	)
	record, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionRecord{}, fmt.Errorf("%w: OpenCode session", basecollector.ErrSessionNotFound)
	}
	if err != nil {
		return sessionRecord{}, fmt.Errorf("%w: read OpenCode session counters", basecollector.ErrMalformed)
	}
	return record, nil
}

func findByDirectory(ctx context.Context, db *sql.DB, directory string, earliest, latest int64) (sessionRecord, error) {
	rows, err := db.QueryContext(
		ctx,
		`SELECT `+sessionColumns+` FROM "session" WHERE directory = ? AND (parent_id IS NULL OR parent_id = '') AND time_updated >= ? AND time_updated <= ? ORDER BY time_updated DESC, id ASC LIMIT 2`,
		directory,
		earliest,
		latest,
	)
	if err != nil {
		return sessionRecord{}, fmt.Errorf("%w: query OpenCode directory fallback", basecollector.ErrMalformed)
	}
	defer rows.Close()

	var matches []sessionRecord
	for rows.Next() {
		record, err := scanSession(rows)
		if err != nil {
			return sessionRecord{}, fmt.Errorf("%w: read OpenCode session counters", basecollector.ErrMalformed)
		}
		matches = append(matches, record)
	}
	if err := rows.Err(); err != nil {
		return sessionRecord{}, fmt.Errorf("%w: query OpenCode directory fallback", basecollector.ErrMalformed)
	}
	if len(matches) == 0 {
		return sessionRecord{}, fmt.Errorf("%w: OpenCode directory fallback", basecollector.ErrSessionNotFound)
	}
	if len(matches) > 1 {
		return sessionRecord{}, fmt.Errorf("%w: OpenCode directory fallback has multiple recent sessions", basecollector.ErrAmbiguous)
	}
	return matches[0], nil
}

func parseModel(value sql.NullString) (provider, model string, err error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" || strings.TrimSpace(value.String) == "null" {
		return "", "", nil
	}

	var decoded any
	if err := json.Unmarshal([]byte(value.String), &decoded); err != nil {
		return "", "", err
	}
	switch typed := decoded.(type) {
	case string:
		return "", strings.TrimSpace(typed), nil
	case map[string]any:
		provider, err = modelString(typed, "providerID", "providerId", "provider_id", "provider")
		if err != nil {
			return "", "", err
		}
		model, err = modelString(typed, "modelID", "modelId", "model_id", "model", "id")
		if err != nil {
			return "", "", err
		}
		return provider, model, nil
	default:
		return "", "", errors.New("model metadata must be an object or string")
	}
}

func modelString(values map[string]any, keys ...string) (string, error) {
	for _, key := range keys {
		value, ok := values[key]
		if !ok || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string", key)
		}
		return strings.TrimSpace(text), nil
	}
	return "", nil
}

func checkedAdd(left, right uint64) (uint64, bool) {
	result := left + right
	return result, result >= left
}
