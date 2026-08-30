package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	basecollector "github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const (
	maxMetadataRecords = 16
	maxMetadataBytes   = 16 << 20
	maxLabelBytes      = 256
	maxFutureClockSkew = 5 * time.Minute
)

type sessionMetadata struct {
	ID            string
	CWD           string
	CLIVersion    string
	ModelProvider string
	ParentID      string
	ActivityAt    time.Time
}

type resolvedRollout struct {
	path     string
	metadata sessionMetadata
}

func listRollouts(ctx context.Context, sessionsDir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(sessionsDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") {
			paths = append(paths, path)
		}
		return nil
	})
	if err == nil {
		return paths, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fs.ErrNotExist
	}
	return nil, errors.New("codex sessions directory could not be read")
}

func (c *Collector) resolve(ctx context.Context, paths []string, target basecollector.Target) (resolvedRollout, usage.Confidence, error) {
	requestedID := strings.TrimSpace(target.SessionID)
	if requestedID != "" {
		if safeLabel(requestedID) == "" {
			return resolvedRollout{}, "", basecollector.ErrSessionNotFound
		}
		matches, err := matchMetadata(ctx, paths, func(metadata sessionMetadata) bool {
			return metadata.ID == requestedID
		})
		if err != nil {
			return resolvedRollout{}, "", err
		}
		switch len(matches) {
		case 0:
			// A supplied ID is authoritative. Never silently replace it with a
			// cwd-based match, even when fallback matching is enabled.
			return resolvedRollout{}, "", basecollector.ErrSessionNotFound
		case 1:
			return matches[0], usage.ConfidenceExact, nil
		default:
			return resolvedRollout{}, "", basecollector.ErrAmbiguous
		}
	}

	if !c.allowCWDRecencyFallback || strings.TrimSpace(target.CWD) == "" {
		return resolvedRollout{}, "", basecollector.ErrSessionNotFound
	}

	now := c.now()
	matches, err := matchMetadata(ctx, paths, func(metadata sessionMetadata) bool {
		if metadata.ID == "" || metadata.ParentID != "" || metadata.ActivityAt.IsZero() || !sameCWD(metadata.CWD, target.CWD) {
			return false
		}
		age := now.Sub(metadata.ActivityAt)
		return age >= -maxFutureClockSkew && age <= c.recencyWindow
	})
	if err != nil {
		return resolvedRollout{}, "", err
	}
	switch len(matches) {
	case 0:
		return resolvedRollout{}, "", basecollector.ErrSessionNotFound
	case 1:
		return matches[0], usage.ConfidenceEstimated, nil
	default:
		return resolvedRollout{}, "", basecollector.ErrAmbiguous
	}
}

func matchMetadata(ctx context.Context, paths []string, matches func(sessionMetadata) bool) ([]resolvedRollout, error) {
	result := make([]resolvedRollout, 0, 1)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		metadata, err := readSessionMetadata(path)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			// One corrupt or concurrently replaced unrelated rollout must not
			// make every active Codex pane unavailable.
			continue
		}
		if matches(metadata) {
			result = append(result, resolvedRollout{path: path, metadata: metadata})
		}
	}
	return result, nil
}

var errNoSessionMetadata = errors.New("no session metadata")

func readSessionMetadata(path string) (sessionMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return sessionMetadata{}, errors.New("codex rollout could not be read")
	}
	defer file.Close()

	decoder := json.NewDecoder(io.LimitReader(file, maxMetadataBytes))
	for range maxMetadataRecords {
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				ID            string `json:"id"`
				CWD           string `json:"cwd"`
				CLIVersion    string `json:"cli_version"`
				ModelProvider string `json:"model_provider"`
				ParentThread  string `json:"parent_thread_id"`
				ForkedFrom    string `json:"forked_from_id"`
				Timestamp     string `json:"timestamp"`
			} `json:"payload"`
		}
		if err := decoder.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				return sessionMetadata{}, errNoSessionMetadata
			}
			return sessionMetadata{}, malformed(err)
		}
		if record.Type != "session_meta" {
			continue
		}

		metadata := sessionMetadata{
			ID:            strings.TrimSpace(record.Payload.ID),
			CWD:           record.Payload.CWD,
			CLIVersion:    record.Payload.CLIVersion,
			ModelProvider: record.Payload.ModelProvider,
			ParentID:      firstNonEmpty(record.Payload.ParentThread, record.Payload.ForkedFrom),
		}
		if info, err := file.Stat(); err == nil {
			metadata.ActivityAt = info.ModTime()
		} else {
			return sessionMetadata{}, errors.New("codex rollout could not be read")
		}
		if safeLabel(metadata.ID) == "" {
			return sessionMetadata{}, errNoSessionMetadata
		}
		return metadata, nil
	}
	return sessionMetadata{}, errNoSessionMetadata
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func sameCWD(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func safeLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxLabelBytes {
		return ""
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return ""
		}
	}
	return value
}
