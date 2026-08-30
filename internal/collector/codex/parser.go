package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	basecollector "github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const (
	reverseReadChunk = 64 << 10
	maxJSONLine      = 8 << 20
)

type rawTokenUsage struct {
	Input      *uint64 `json:"input_tokens"`
	CacheRead  *uint64 `json:"cached_input_tokens"`
	CacheWrite *uint64 `json:"cache_write_input_tokens"`
	Output     *uint64 `json:"output_tokens"`
	Reasoning  *uint64 `json:"reasoning_output_tokens"`
	Total      *uint64 `json:"total_tokens"`
}

type rawTokenInfo struct {
	TotalUsage         *rawTokenUsage `json:"total_token_usage"`
	LastUsage          *rawTokenUsage `json:"last_token_usage"`
	ModelContextWindow *uint64        `json:"model_context_window"`
}

type parsedRollout struct {
	tokens  usage.Tokens
	context *usage.ContextWindow
	model   string
}

func readRollout(ctx context.Context, path string) (parsedRollout, error) {
	file, err := os.Open(path)
	if err != nil {
		return parsedRollout{}, errors.New("codex rollout could not be read")
	}
	defer file.Close()

	var result parsedRollout
	foundUsage := false
	err = scanLinesReverse(ctx, file, func(line []byte) (bool, error) {
		var kind struct {
			Type    string `json:"type"`
			Payload struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(line, &kind); err != nil {
			// Rollouts are append-only. A partial final write must not hide the
			// preceding complete cumulative event.
			return false, nil
		}

		if !foundUsage && kind.Type == "event_msg" && kind.Payload.Type == "token_count" {
			var event struct {
				Payload struct {
					Info *rawTokenInfo `json:"info"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(line, &event); err != nil {
				return false, malformed(err)
			}
			// Codex can emit an initial token_count event with null info. It is
			// not a complete usage sample, so continue to an earlier event.
			if event.Payload.Info == nil || event.Payload.Info.TotalUsage == nil {
				return false, nil
			}

			tokens, err := normalizeTokenUsage(*event.Payload.Info.TotalUsage)
			if err != nil {
				return false, malformed(err)
			}
			result.tokens = tokens
			foundUsage = true

			if event.Payload.Info.LastUsage != nil && event.Payload.Info.ModelContextWindow != nil && *event.Payload.Info.ModelContextWindow > 0 {
				used, err := contextTokenCount(*event.Payload.Info.LastUsage)
				if err != nil {
					return false, malformed(err)
				}
				result.context = &usage.ContextWindow{
					Used:  used,
					Limit: *event.Payload.Info.ModelContextWindow,
				}
			}
		}

		if result.model == "" && kind.Type == "turn_context" {
			var turn struct {
				Payload struct {
					Model string `json:"model"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(line, &turn); err == nil {
				result.model = safeLabel(turn.Payload.Model)
			}
		}

		return foundUsage && result.model != "", nil
	})
	if err != nil {
		if errors.Is(err, basecollector.ErrMalformed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return parsedRollout{}, err
		}
		return parsedRollout{}, errors.New("codex rollout could not be read")
	}
	if !foundUsage {
		return parsedRollout{}, malformed(errors.New("no complete token_count event"))
	}
	return result, nil
}

func contextTokenCount(raw rawTokenUsage) (uint64, error) {
	// Codex's last-token record can represent a post-compaction estimate whose
	// component counters are intentionally incomplete. Its explicit total is
	// the authoritative context occupancy in that case.
	if raw.Total != nil {
		return *raw.Total, nil
	}
	tokens, err := normalizeTokenUsage(raw)
	if err != nil {
		return 0, err
	}
	return tokens.Total, nil
}

func normalizeTokenUsage(raw rawTokenUsage) (usage.Tokens, error) {
	input := value(raw.Input)
	cacheRead := value(raw.CacheRead)
	cacheWrite := value(raw.CacheWrite)
	output := value(raw.Output)
	reasoning := value(raw.Reasoning)

	if cacheRead > input || cacheWrite > input-cacheRead {
		return usage.Tokens{}, errors.New("cached input exceeds inclusive input")
	}
	freshInput := input - cacheRead - cacheWrite

	if raw.Total != nil {
		if input > ^uint64(0)-output {
			return usage.Tokens{}, errors.New("codex total token count overflows uint64")
		}
		if *raw.Total != input+output {
			return usage.Tokens{}, errors.New("codex total token count is inconsistent")
		}
	}

	return usage.NewTokens(freshInput, cacheRead, cacheWrite, output, reasoning)
}

func value(number *uint64) uint64 {
	if number == nil {
		return 0
	}
	return *number
}

func malformed(err error) error {
	return fmt.Errorf("%w: Codex rollout metadata or usage counters: %v", basecollector.ErrMalformed, err)
}

// scanLinesReverse reads bounded chunks from the end. Oversized records are
// discarded without retaining their contents, allowing a small token_count
// event before a very large prompt/tool record to remain discoverable.
func scanLinesReverse(ctx context.Context, file *os.File, visit func([]byte) (bool, error)) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	position := info.Size()
	pending := make([]byte, 0, reverseReadChunk)
	discarding := false

	complete := func() (bool, error) {
		if discarding || len(bytes.TrimSpace(pending)) == 0 {
			pending = pending[:0]
			discarding = false
			return false, nil
		}
		line := pending
		pending = pending[:0]
		return visit(line)
	}

	for position > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		readSize := int64(reverseReadChunk)
		if position < readSize {
			readSize = position
		}
		position -= readSize
		block := make([]byte, int(readSize))
		if _, err := file.ReadAt(block, position); err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		end := len(block)
		for end > 0 {
			newline := bytes.LastIndexByte(block[:end], '\n')
			start := newline + 1
			fragment := block[start:end]
			if !discarding {
				if len(fragment) > maxJSONLine-len(pending) {
					pending = pending[:0]
					discarding = true
				} else if len(fragment) > 0 {
					joined := make([]byte, 0, len(fragment)+len(pending))
					joined = append(joined, fragment...)
					joined = append(joined, pending...)
					pending = joined
				}
			}

			if newline < 0 {
				break
			}
			stop, err := complete()
			if err != nil || stop {
				return err
			}
			end = newline
		}
	}

	stop, err := complete()
	if err != nil || stop {
		return err
	}
	return nil
}
