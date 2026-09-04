package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/herdr"
	"github.com/calebcav/token-usage/internal/sequence"
	"github.com/calebcav/token-usage/internal/usage"
)

const defaultParallelism = 4

type HerdrClient interface {
	Snapshot(context.Context) (herdr.Snapshot, error)
	ReportMetadata(context.Context, string, herdr.Metadata) error
}

type processInfoClient interface {
	ProcessInfo(context.Context, string) (herdr.ProcessInfo, error)
}

type agentFocusClient interface {
	FocusAgent(context.Context, string) error
}

type Service struct {
	Registry       *collector.Registry
	Herdr          HerdrClient
	MaxParallelism int
	NextSequence   func() uint64
}

type CollectOptions struct {
	PaneID         string
	Publish        bool
	IncludeWorking bool
}

func (s *Service) FocusPane(ctx context.Context, paneID string) error {
	if s == nil || s.Herdr == nil {
		return errors.New("herdr client is not configured")
	}
	client, ok := s.Herdr.(agentFocusClient)
	if !ok {
		return errors.New("herdr client does not support focusing agents")
	}
	return client.FocusAgent(ctx, paneID)
}

type Result struct {
	Target     collector.Target `json:"target"`
	Snapshot   *usage.Snapshot  `json:"snapshot,omitempty"`
	Error      string           `json:"error,omitempty"`
	PublishErr string           `json:"publish_error,omitempty"`
}

func (r Result) Err() error {
	var values []error
	if r.Error != "" {
		values = append(values, errors.New(r.Error))
	}
	if r.PublishErr != "" {
		values = append(values, errors.New(r.PublishErr))
	}
	return errors.Join(values...)
}

func (s *Service) Collect(ctx context.Context, options CollectOptions) ([]Result, error) {
	if s == nil || s.Registry == nil {
		return nil, errors.New("collector registry is not configured")
	}
	if s.Herdr == nil {
		return nil, errors.New("herdr client is not configured")
	}

	live, err := s.Herdr.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	targets := live.Targets()
	if discovered, ok := s.Herdr.(processInfoClient); ok && s.Registry.HasProcessMatchers() {
		targets, err = s.addProcessTargets(ctx, live, targets, discovered)
		if err != nil {
			return nil, err
		}
	}
	targets = filterTargets(targets, options)
	results := make([]Result, len(targets))
	if len(targets) == 0 {
		return results, nil
	}

	parallelism := s.MaxParallelism
	if parallelism <= 0 {
		parallelism = defaultParallelism
	}
	if parallelism > len(targets) {
		parallelism = len(targets)
	}

	jobs := make(chan int)
	sequenceNumber := sequence.MemoryNext()
	if s.NextSequence != nil {
		sequenceNumber = s.NextSequence()
	}
	var workers sync.WaitGroup
	workers.Add(parallelism)
	for range parallelism {
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index] = s.collectOne(ctx, targets[index], options.Publish, sequenceNumber)
			}
		}()
	}
	for index := range targets {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	return results, nil
}

func (s *Service) addProcessTargets(ctx context.Context, live herdr.Snapshot, targets []collector.Target, client processInfoClient) ([]collector.Target, error) {
	claimed := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		claimed[target.PaneID] = struct{}{}
	}
	for _, pane := range live.Panes {
		if pane.PaneID == "" {
			continue
		}
		if _, exists := claimed[pane.PaneID]; exists {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := client.ProcessInfo(ctx, pane.PaneID)
		if err != nil {
			continue
		}
		processes := make([]collector.Process, 0, len(info.ForegroundProcesses))
		processCWD := ""
		for _, process := range info.ForegroundProcesses {
			processes = append(processes, collector.Process{
				Name:  process.Name,
				Argv0: process.Argv0,
				Argv:  append([]string(nil), process.Argv...),
				CWD:   process.CWD,
			})
			processCWD = firstNonEmpty(processCWD, process.CWD)
		}
		harness, err := s.Registry.DetectHarness(processes)
		if err != nil || harness == "" {
			continue
		}
		targets = append(targets, collector.Target{
			PaneID:      pane.PaneID,
			WorkspaceID: pane.WorkspaceID,
			Harness:     harness,
			CWD:         firstNonEmpty(processCWD, pane.ForegroundCWD, pane.CWD),
			State:       firstNonEmpty(pane.AgentStatus, pane.Status, "unknown"),
			Confidence:  usage.ConfidenceEstimated,
		})
		claimed[pane.PaneID] = struct{}{}
	}
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].WorkspaceID != targets[j].WorkspaceID {
			return targets[i].WorkspaceID < targets[j].WorkspaceID
		}
		return targets[i].PaneID < targets[j].PaneID
	})
	return targets, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func filterTargets(targets []collector.Target, options CollectOptions) []collector.Target {
	filtered := make([]collector.Target, 0, len(targets))
	for _, target := range targets {
		if options.PaneID != "" && target.PaneID != options.PaneID {
			continue
		}
		if !options.IncludeWorking && strings.EqualFold(target.State, "working") {
			continue
		}
		filtered = append(filtered, target)
	}
	return filtered
}

func (s *Service) collectOne(ctx context.Context, target collector.Target, publish bool, sequence uint64) Result {
	result := Result{Target: target}
	item, err := s.Registry.CollectorFor(target.Harness)
	if err == nil {
		var snapshot usage.Snapshot
		snapshot, err = item.Collect(ctx, target)
		if err == nil {
			snapshot.PaneID = target.PaneID
			snapshot.WorkspaceID = target.WorkspaceID
			snapshot.State = target.State
			if snapshot.Harness == "" {
				snapshot.Harness = target.Harness
			}
			if snapshot.Confidence == "" {
				snapshot.Confidence = target.Confidence
			}
			if validationErr := snapshot.Validate(); validationErr != nil {
				err = fmt.Errorf("invalid %s usage snapshot: %w", target.Harness, validationErr)
			} else {
				result.Snapshot = &snapshot
			}
		}
	}
	if err != nil {
		result.Error = usage.SanitizeText(err.Error(), 512)
	}

	if publish {
		metadata := metadataFor(result.Snapshot)
		metadata.Seq = sequence
		if reportErr := s.Herdr.ReportMetadata(ctx, target.PaneID, metadata); reportErr != nil {
			result.PublishErr = usage.SanitizeText(reportErr.Error(), 512)
		}
	}
	return result
}

func metadataFor(snapshot *usage.Snapshot) herdr.Metadata {
	keys := []string{"usage", "context", "limit", "model", "harness", "confidence"}
	tokens := make(map[string]*string, len(keys))
	for _, key := range keys {
		tokens[key] = nil
	}
	if snapshot == nil {
		return herdr.Metadata{Tokens: tokens}
	}

	usageValue := snapshot.CompactUsage()
	contextValue := snapshot.CompactContext()
	limitValue := snapshot.CompactLimit()
	modelValue := snapshot.Model
	harnessValue := snapshot.Harness
	confidenceValue := string(snapshot.Confidence)
	tokens["usage"] = &usageValue
	if contextValue != "" {
		tokens["context"] = &contextValue
	}
	if limitValue != "" {
		tokens["limit"] = &limitValue
	}
	if modelValue != "" {
		tokens["model"] = &modelValue
	}
	tokens["harness"] = &harnessValue
	tokens["confidence"] = &confidenceValue
	return herdr.Metadata{Tokens: tokens}
}

// UniqueSessions removes duplicate pane views of the same native session. It
// keeps the newest successfully collected snapshot and leaves failures with no
// stable session id as pane-scoped rows.
func UniqueSessions(results []Result) []Result {
	byKey := make(map[string]Result, len(results))
	order := make([]string, 0, len(results))
	for _, result := range results {
		key := result.Target.Harness + ":pane:" + result.Target.PaneID
		if result.Snapshot != nil && result.Snapshot.SessionID != "" {
			key = result.Snapshot.Harness + ":session:" + result.Snapshot.SessionID
		}
		current, exists := byKey[key]
		if !exists {
			order = append(order, key)
			byKey[key] = result
			continue
		}
		if result.Snapshot != nil && (current.Snapshot == nil || result.Snapshot.CollectedAt.After(current.Snapshot.CollectedAt)) {
			byKey[key] = result
		}
	}

	unique := make([]Result, 0, len(order))
	for _, key := range order {
		unique = append(unique, byKey[key])
	}
	sort.SliceStable(unique, func(i, j int) bool {
		if unique[i].Target.WorkspaceID != unique[j].Target.WorkspaceID {
			return unique[i].Target.WorkspaceID < unique[j].Target.WorkspaceID
		}
		return unique[i].Target.PaneID < unique[j].Target.PaneID
	})
	return unique
}
