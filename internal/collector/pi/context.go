package pi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const contextResolverTimeout = 5 * time.Second

type commandContextResolver struct {
	binary string
}

func contextResolver(configured ContextLimitResolver) ContextLimitResolver {
	if configured != nil {
		return configured
	}
	binary := strings.TrimSpace(os.Getenv("PI_BIN_PATH"))
	if binary == "" {
		binary = "pi"
	}
	return commandContextResolver{binary: binary}
}

func (c *Collector) resolveContextLimit(ctx context.Context, provider, model string) (uint64, bool) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" || c.contextResolver == nil {
		return 0, false
	}
	key := provider + "\x00" + model
	c.contextMu.Lock()
	defer c.contextMu.Unlock()
	if limit := c.contextLimits[key]; limit > 0 {
		return limit, true
	}
	limit, err := c.contextResolver.ResolveContextLimit(ctx, provider, model)
	if err != nil || limit == 0 {
		return 0, false
	}
	c.contextLimits[key] = limit
	return limit, true
}

func (r commandContextResolver) ResolveContextLimit(ctx context.Context, provider, model string) (uint64, error) {
	commandCtx, cancel := context.WithTimeout(ctx, contextResolverTimeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, r.binary, "--offline", "--list-models", model)
	command.Env = append(os.Environ(), "PI_OFFLINE=1")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if commandCtx.Err() != nil {
			return 0, commandCtx.Err()
		}
		return 0, fmt.Errorf("query Pi model catalog: %w", err)
	}
	return parseContextLimit(stdout.String(), provider, model)
}

func parseContextLimit(output, provider, model string) (uint64, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != provider || fields[1] != model {
			continue
		}
		return parseTokenCount(fields[2])
	}
	return 0, errors.New("model not found in Pi catalog")
}

func parseTokenCount(value string) (uint64, error) {
	value = strings.TrimSpace(strings.ToUpper(value))
	multiplier := uint64(1)
	if strings.HasSuffix(value, "K") {
		multiplier = 1_000
		value = strings.TrimSuffix(value, "K")
	} else if strings.HasSuffix(value, "M") {
		multiplier = 1_000_000
		value = strings.TrimSuffix(value, "M")
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New("invalid Pi context limit")
	}
	limit := parsed * float64(multiplier)
	if limit > float64(^uint64(0)) || limit != float64(uint64(limit)) {
		return 0, errors.New("invalid Pi context limit")
	}
	return uint64(limit), nil
}
