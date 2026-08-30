package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	basecollector "github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const quotaResultFixture = `{
  "rateLimits": {
    "primary": {"usedPercent": 42, "windowDurationMins": 300, "resetsAt": 1788177600},
    "secondary": {"usedPercent": 73, "windowDurationMins": 10080, "resetsAt": null},
    "individualLimit": {"limit": "100", "used": "80", "remainingPercent": 20, "resetsAt": 1788264000}
  }
}`

func TestParseQuotaResultUsesBackwardCompatibleRateLimits(t *testing.T) {
	result := []byte(`{
      "rateLimits": {
        "primary": {"usedPercent": 42, "windowDurationMins": 300, "resetsAt": 1788177600},
        "secondary": {"usedPercent": 73, "windowDurationMins": 10080, "resetsAt": null},
        "individualLimit": {"remainingPercent": 20, "resetsAt": 1788264000}
      },
      "rateLimitsByLimitId": {
        "codex": {"primary": {"usedPercent": 99, "windowDurationMins": 60}}
      }
    }`)

	quota, err := parseQuotaResult(result, collectedAt)
	if err != nil {
		t.Fatal(err)
	}
	if quota.Source != quotaSource || !quota.CollectedAt.Equal(collectedAt) {
		t.Fatalf("quota metadata = source %q, collected %v", quota.Source, quota.CollectedAt)
	}
	if len(quota.Windows) != 3 {
		t.Fatalf("windows = %#v, want three", quota.Windows)
	}
	want := []usage.QuotaWindow{
		{Label: "5h", UsedPercent: 42, ResetsAt: timePointer(time.Unix(1788177600, 0).UTC())},
		{Label: "7d", UsedPercent: 73},
		{Label: "spend", UsedPercent: 80, ResetsAt: timePointer(time.Unix(1788264000, 0).UTC())},
	}
	if !reflect.DeepEqual(quota.Windows, want) {
		t.Fatalf("windows = %#v, want %#v", quota.Windows, want)
	}
}

func TestParseQuotaResultIgnoresNewMultiBucketView(t *testing.T) {
	_, err := parseQuotaResult([]byte(`{
      "rateLimitsByLimitId": {
        "codex": {"primary": {"usedPercent": 10, "windowDurationMins": 300}}
      }
    }`), collectedAt)
	if err == nil {
		t.Fatal("parseQuotaResult() accepted response without result.rateLimits")
	}
}

func TestParseQuotaResultRejectsInvalidPercentagesAndReset(t *testing.T) {
	tests := map[string]string{
		"rate window over 100": `{"rateLimits":{"primary":{"usedPercent":101,"windowDurationMins":300}}}`,
		"remaining over 100":   `{"rateLimits":{"individualLimit":{"remainingPercent":101}}}`,
		"reset outside JSON":   `{"rateLimits":{"primary":{"usedPercent":10,"windowDurationMins":300,"resetsAt":9223372036854775807}}}`,
	}
	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseQuotaResult([]byte(fixture), collectedAt); err == nil {
				t.Fatal("parseQuotaResult() error = nil")
			}
		})
	}
}

func TestDurationLabel(t *testing.T) {
	minutes := func(value int64) *int64 { return &value }
	tests := []struct {
		name     string
		minutes  *int64
		fallback string
		want     string
	}{
		{name: "five hours", minutes: minutes(300), want: "5h"},
		{name: "seven days", minutes: minutes(10080), want: "7d"},
		{name: "compact minutes", minutes: minutes(90), want: "90m"},
		{name: "missing duration", fallback: "primary", want: "primary"},
		{name: "invalid duration", minutes: minutes(0), fallback: "secondary", want: "secondary"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := durationLabel(test.minutes, test.fallback); got != test.want {
				t.Fatalf("durationLabel() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCollectAccountLimitsRequiresOptIn(t *testing.T) {
	var calls atomic.Int32
	item := New(Config{
		Home: fixtureHome(t),
		Now:  func() time.Time { return collectedAt },
		QuotaLoader: func(context.Context) ([]byte, error) {
			calls.Add(1)
			return []byte(quotaResultFixture), nil
		},
		CachePath: filepath.Join(t.TempDir(), "quota.json"),
	})

	snapshot, err := item.Collect(context.Background(), basecollector.Target{Harness: "codex", SessionID: primarySession})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Quota != nil || calls.Load() != 0 {
		t.Fatalf("quota = %#v, loader calls = %d; want nil and zero", snapshot.Quota, calls.Load())
	}
}

func TestCollectAccountLimitsIsBestEffort(t *testing.T) {
	var calls atomic.Int32
	item := New(Config{
		Home:                fixtureHome(t),
		EnableAccountLimits: true,
		Now:                 func() time.Time { return collectedAt },
		QuotaLoader: func(context.Context) ([]byte, error) {
			calls.Add(1)
			return nil, errors.New("private auth and path detail")
		},
		CachePath: filepath.Join(t.TempDir(), "quota.json"),
	})

	snapshot, err := item.Collect(context.Background(), basecollector.Target{Harness: "codex", SessionID: primarySession})
	if err != nil {
		t.Fatalf("Collect() quota failure escaped: %v", err)
	}
	if snapshot.Quota != nil || snapshot.Tokens.Total != 300 {
		t.Fatalf("snapshot = %#v, want local usage without quota", snapshot)
	}
	// A single account-level failure should not be retried once per pane in the
	// same refresh cycle.
	snapshot, err = item.Collect(context.Background(), basecollector.Target{Harness: "codex", SessionID: minimalSession})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Quota != nil || calls.Load() != 1 {
		t.Fatalf("second quota = %#v, loader calls = %d; want nil and one", snapshot.Quota, calls.Load())
	}
}

func TestCollectAccountLimitsHonorsBoundedTimeout(t *testing.T) {
	item := New(Config{
		Home:                fixtureHome(t),
		EnableAccountLimits: true,
		Now:                 func() time.Time { return collectedAt },
		QuotaTimeout:        20 * time.Millisecond,
		QuotaLoader: func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		CachePath: filepath.Join(t.TempDir(), "quota.json"),
	})

	started := time.Now()
	snapshot, err := item.Collect(context.Background(), basecollector.Target{Harness: "codex", SessionID: primarySession})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Collect() took %v, want bounded quota retrieval", elapsed)
	}
	if snapshot.Quota != nil {
		t.Fatalf("Quota = %#v, want nil after timeout", snapshot.Quota)
	}
}

func TestQuotaCacheReusedAcrossCollectors(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "nested", "quota.json")
	var calls atomic.Int32
	loader := func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte(quotaResultFixture), nil
	}
	first := New(Config{
		Home:                fixtureHome(t),
		EnableAccountLimits: true,
		Now:                 func() time.Time { return collectedAt },
		QuotaLoader:         loader,
		CachePath:           cachePath,
	})
	second := New(Config{
		Home:                fixtureHome(t),
		EnableAccountLimits: true,
		Now:                 func() time.Time { return collectedAt.Add(30 * time.Second) },
		QuotaLoader:         loader,
		CachePath:           cachePath,
	})

	target := basecollector.Target{Harness: "codex", SessionID: primarySession}
	firstSnapshot, err := first.Collect(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	secondSnapshot, err := second.Collect(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("loader calls = %d, want one", calls.Load())
	}
	if firstSnapshot.Quota == nil || secondSnapshot.Quota == nil || !secondSnapshot.Quota.CollectedAt.Equal(collectedAt) {
		t.Fatalf("cached quotas = first %#v, second %#v", firstSnapshot.Quota, secondSnapshot.Quota)
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("cache permissions = %o, want 600", permissions)
	}
}

func TestQuotaCacheMalformedOrExpiredFallsBackToLoader(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "malformed",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "expired",
			setup: func(t *testing.T, path string) {
				quota, err := parseQuotaResult([]byte(quotaResultFixture), collectedAt.Add(-quotaCacheTTL))
				if err != nil {
					t.Fatal(err)
				}
				if err := writeQuotaCache(context.Background(), path, *quota); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			cachePath := filepath.Join(directory, "quota.json")
			test.setup(t, cachePath)
			var calls atomic.Int32
			item := New(Config{
				Home:                fixtureHome(t),
				EnableAccountLimits: true,
				Now:                 func() time.Time { return collectedAt },
				QuotaLoader: func(context.Context) ([]byte, error) {
					calls.Add(1)
					return []byte(quotaResultFixture), nil
				},
				CachePath: cachePath,
			})
			snapshot, err := item.Collect(context.Background(), basecollector.Target{Harness: "codex", SessionID: primarySession})
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || snapshot.Quota == nil {
				t.Fatalf("loader calls = %d, quota = %#v; want fallback fetch", calls.Load(), snapshot.Quota)
			}
		})
	}
}

func TestQuotaCacheExpiresAtWindowReset(t *testing.T) {
	reset := collectedAt.Add(30 * time.Second)
	quota := usage.QuotaSnapshot{
		Windows:     []usage.QuotaWindow{{Label: "5h", UsedPercent: 90, ResetsAt: &reset}},
		Source:      quotaSource,
		CollectedAt: collectedAt,
	}
	path := filepath.Join(t.TempDir(), "quota.json")
	if err := writeQuotaCache(context.Background(), path, quota); err != nil {
		t.Fatal(err)
	}
	if _, err := readQuotaCache(context.Background(), path, reset); err == nil {
		t.Fatal("readQuotaCache() accepted a reset window")
	}
}

func TestDefaultQuotaCachePathScopesHomeAndBinary(t *testing.T) {
	first := defaultQuotaCachePath("/cache", "/profiles/one", "codex")
	if first == defaultQuotaCachePath("/cache", "/profiles/two", "codex") {
		t.Fatal("cache path did not vary by CODEX_HOME")
	}
	if first == defaultQuotaCachePath("/cache", "/profiles/one", "/other/codex") {
		t.Fatal("cache path did not vary by binary")
	}
	if strings.Contains(first, "profiles") {
		t.Fatalf("cache path leaked profile path: %q", first)
	}
}

func TestExchangeAppServerHandshake(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	clientOutput, serverOutput := io.Pipe()
	defer serverInput.Close()
	defer clientInput.Close()
	defer clientOutput.Close()
	defer serverOutput.Close()

	serverDone := make(chan error, 1)
	go func() {
		defer serverOutput.Close()
		decoder := json.NewDecoder(serverInput)
		encoder := json.NewEncoder(serverOutput)

		var initialize struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
			Params struct {
				ClientInfo struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"clientInfo"`
			} `json:"params"`
		}
		if err := decoder.Decode(&initialize); err != nil {
			serverDone <- err
			return
		}
		if initialize.Method != "initialize" || initialize.ID != 1 || initialize.Params.ClientInfo.Name != "token-usage" || initialize.Params.ClientInfo.Version != "0.1.0" {
			serverDone <- fmt.Errorf("unexpected initialize request: %#v", initialize)
			return
		}
		if err := encoder.Encode(map[string]any{"method": "account/updated", "params": map[string]any{}}); err != nil {
			serverDone <- err
			return
		}
		if err := encoder.Encode(map[string]any{"id": 1, "result": map[string]any{"userAgent": "fixture"}}); err != nil {
			serverDone <- err
			return
		}

		var initialized map[string]json.RawMessage
		if err := decoder.Decode(&initialized); err != nil {
			serverDone <- err
			return
		}
		if string(initialized["method"]) != `"initialized"` {
			serverDone <- fmt.Errorf("unexpected initialized notification: %s", initialized["method"])
			return
		}

		var readRequest struct {
			Method string          `json:"method"`
			ID     int             `json:"id"`
			Params json.RawMessage `json:"params"`
		}
		if err := decoder.Decode(&readRequest); err != nil {
			serverDone <- err
			return
		}
		if readRequest.Method != "account/rateLimits/read" || readRequest.ID != 2 || strings.TrimSpace(string(readRequest.Params)) != "null" {
			serverDone <- fmt.Errorf("unexpected rate-limit request: %#v", readRequest)
			return
		}
		if err := encoder.Encode(map[string]any{"method": "account/rateLimits/updated", "params": map[string]any{}}); err != nil {
			serverDone <- err
			return
		}
		if err := encoder.Encode(map[string]any{
			"id": 2,
			"result": map[string]any{
				"rateLimits": map[string]any{
					"primary": map[string]any{"usedPercent": 12, "windowDurationMins": 300},
				},
			},
		}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()

	type exchangeResult struct {
		data []byte
		err  error
	}
	exchangeDone := make(chan exchangeResult, 1)
	go func() {
		data, err := exchangeAppServer(context.Background(), clientInput, clientOutput)
		exchangeDone <- exchangeResult{data: data, err: err}
	}()

	select {
	case result := <-exchangeDone:
		if result.err != nil {
			t.Fatal(result.err)
		}
		quota, err := parseQuotaResult(result.data, collectedAt)
		if err != nil {
			t.Fatal(err)
		}
		if len(quota.Windows) != 1 || quota.Windows[0].Label != "5h" || quota.Windows[0].UsedPercent != 12 {
			t.Fatalf("quota = %#v", quota)
		}
	case <-time.After(2 * time.Second):
		clientInput.Close()
		clientOutput.Close()
		t.Fatal("app-server exchange did not complete")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestNewConfiguresQuotaBinary(t *testing.T) {
	item := New(Config{Home: fixtureHome(t), Binary: "codex-fixture"})
	if item.binary != "codex-fixture" {
		t.Fatalf("binary = %q, want codex-fixture", item.binary)
	}
	defaultItem := New(Config{Home: fixtureHome(t)})
	if defaultItem.binary != defaultCodexBinary {
		t.Fatalf("default binary = %q, want %q", defaultItem.binary, defaultCodexBinary)
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}
