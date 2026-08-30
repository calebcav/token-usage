package collector

import (
	"context"
	"errors"
	"testing"

	"github.com/calebcav/token-usage/internal/usage"
)

type stubCollector struct{ harnesses []string }

func (s stubCollector) Harnesses() []string { return s.harnesses }
func (s stubCollector) Collect(context.Context, Target) (usage.Snapshot, error) {
	return usage.Snapshot{}, nil
}

func TestRegistryCanonicalizesHarnesses(t *testing.T) {
	registry, err := NewRegistry(stubCollector{harnesses: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.CollectorFor("claude-code"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRejectsDuplicateHarness(t *testing.T) {
	_, err := NewRegistry(
		stubCollector{harnesses: []string{"codex"}},
		stubCollector{harnesses: []string{"codex-cli"}},
	)
	if err == nil {
		t.Fatal("expected duplicate collector error")
	}
}

func TestRegistryReturnsUnsupported(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.CollectorFor("gemini")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}
