package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const (
	fileSnapshotCacheVersion = 1
	maxSnapshotCacheBytes    = 1 << 20
)

// SnapshotCache stores normalized snapshots. Cache reads are opt-in per
// collection so event-driven reconciliations can always bypass stale data.
type SnapshotCache interface {
	Load(collector.Target, time.Duration) (usage.Snapshot, bool)
	Store(collector.Target, usage.Snapshot) error
	Delete(collector.Target) error
}

// FileSnapshotCache is a small, pane-keyed cache shared by short-lived plugin
// processes. It persists only normalized snapshots, never transcript paths or
// source content.
type FileSnapshotCache struct {
	Directory string
	Now       func() time.Time
}

type fileSnapshotCacheEntry struct {
	Version   int            `json:"version"`
	TargetKey string         `json:"target_key"`
	StoredAt  time.Time      `json:"stored_at"`
	Snapshot  usage.Snapshot `json:"snapshot"`
}

// Load returns a snapshot only while it is younger than maxAge and still
// matches the live target. Corrupt, expired, and incompatible entries are
// ordinary cache misses.
func (c *FileSnapshotCache) Load(target collector.Target, maxAge time.Duration) (usage.Snapshot, bool) {
	if c == nil || maxAge <= 0 {
		return usage.Snapshot{}, false
	}
	path, key, ok := c.path(target)
	if !ok {
		return usage.Snapshot{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return usage.Snapshot{}, false
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxSnapshotCacheBytes+1))
	if err != nil || len(data) > maxSnapshotCacheBytes {
		removeCacheFileIfCurrent(file, path)
		return usage.Snapshot{}, false
	}
	var entry fileSnapshotCacheEntry
	if json.Unmarshal(data, &entry) != nil || entry.Version != fileSnapshotCacheVersion || entry.TargetKey != key || entry.StoredAt.IsZero() {
		removeCacheFileIfCurrent(file, path)
		return usage.Snapshot{}, false
	}

	now := c.now()
	age := now.Sub(entry.StoredAt)
	if age < 0 || age >= maxAge {
		removeCacheFileIfCurrent(file, path)
		return usage.Snapshot{}, false
	}
	if entry.Snapshot.Validate() != nil || !snapshotMatchesTarget(entry.Snapshot, target) || quotaIsStale(entry.Snapshot.Quota, now) {
		removeCacheFileIfCurrent(file, path)
		return usage.Snapshot{}, false
	}
	return entry.Snapshot, true
}

// Store atomically replaces the cache entry for target. Cache failures are
// returned to the caller, which may treat them as best effort.
func (c *FileSnapshotCache) Store(target collector.Target, snapshot usage.Snapshot) error {
	if c == nil {
		return errors.New("snapshot cache is not configured")
	}
	path, key, ok := c.path(target)
	if !ok {
		return errors.New("snapshot cache target is invalid")
	}
	if err := snapshot.Validate(); err != nil {
		return fmt.Errorf("cache invalid snapshot: %w", err)
	}
	if !snapshotMatchesTarget(snapshot, target) {
		return errors.New("snapshot does not match cache target")
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("create snapshot cache directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return errors.New("secure snapshot cache directory")
	}

	file, err := os.CreateTemp(directory, ".snapshot-*")
	if err != nil {
		return errors.New("create snapshot cache file")
	}
	temporaryPath := file.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return errors.New("secure snapshot cache file")
	}
	entry := fileSnapshotCacheEntry{
		Version:   fileSnapshotCacheVersion,
		TargetKey: key,
		StoredAt:  c.now().UTC(),
		Snapshot:  snapshot,
	}
	if err := json.NewEncoder(file).Encode(entry); err != nil {
		_ = file.Close()
		return errors.New("encode snapshot cache file")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("sync snapshot cache file")
	}
	if err := file.Close(); err != nil {
		return errors.New("close snapshot cache file")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.New("replace snapshot cache file")
	}
	removeTemporary = false
	if err := os.Chmod(path, 0o600); err != nil {
		return errors.New("secure snapshot cache file")
	}
	return nil
}

// Delete invalidates target's entry. Removing a missing entry succeeds.
func (c *FileSnapshotCache) Delete(target collector.Target) error {
	if c == nil {
		return nil
	}
	path, _, ok := c.path(target)
	if !ok {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("remove snapshot cache file")
	}
	return nil
}

func (c *FileSnapshotCache) path(target collector.Target) (path, key string, ok bool) {
	if c == nil || strings.TrimSpace(c.Directory) == "" {
		return "", "", false
	}
	harness := collector.CanonicalHarness(target.Harness)
	paneID := strings.TrimSpace(target.PaneID)
	if harness == "" || paneID == "" {
		return "", "", false
	}

	cwd := strings.TrimSpace(target.CWD)
	if cwd != "" {
		cwd = filepath.Clean(cwd)
	}
	digest := sha256.New()
	for _, value := range []string{harness, strings.TrimSpace(target.WorkspaceID), paneID, cwd} {
		_, _ = fmt.Fprintf(digest, "%d:", len(value))
		_, _ = io.WriteString(digest, value)
	}
	key = hex.EncodeToString(digest.Sum(nil))
	return filepath.Join(filepath.Clean(c.Directory), "snapshot-"+key+".json"), key, true
}

func removeCacheFileIfCurrent(file *os.File, path string) {
	opened, openErr := file.Stat()
	current, pathErr := os.Stat(path)
	if openErr == nil && pathErr == nil && os.SameFile(opened, current) {
		_ = os.Remove(path)
	}
}

func (c *FileSnapshotCache) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func snapshotMatchesTarget(snapshot usage.Snapshot, target collector.Target) bool {
	if collector.CanonicalHarness(snapshot.Harness) != collector.CanonicalHarness(target.Harness) {
		return false
	}
	if sessionID := strings.TrimSpace(target.SessionID); sessionID != "" && snapshot.SessionID != sessionID {
		return false
	}
	return true
}

func quotaIsStale(quota *usage.QuotaSnapshot, now time.Time) bool {
	if quota == nil {
		return false
	}
	age := now.Sub(quota.CollectedAt)
	if age < 0 {
		return true
	}
	var maxAge time.Duration
	switch quota.Source {
	case "codex-app-server":
		maxAge = time.Minute
	case "claude-statusline":
		maxAge = 5 * time.Minute
	}
	if maxAge > 0 && age >= maxAge {
		return true
	}
	for _, window := range quota.Windows {
		if window.ResetsAt != nil && !now.Before(*window.ResetsAt) {
			return true
		}
	}
	return false
}
