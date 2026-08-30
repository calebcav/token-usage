// Package external implements the versioned subprocess contract used to add a
// harness without recompiling the plugin. Commands are local and receive only a
// normalized target; transcript content and credentials are never forwarded.
package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/calebcav/token-usage/internal/collector"
	"github.com/calebcav/token-usage/internal/usage"
)

const (
	LegacyContractVersion = 1
	ContractVersion       = 2
	defaultTimeout        = 5 * time.Second
	defaultWaitDelay      = time.Second
	maxOutputBytes        = 1 << 20
)

type Command struct {
	Command      []string `json:"command"`
	ProcessNames []string `json:"process_names,omitempty"`
}

type Config struct {
	SchemaVersion int                `json:"schema_version"`
	Collectors    map[string]Command `json:"collectors"`
}

type Request struct {
	SchemaVersion int              `json:"schema_version"`
	Target        collector.Target `json:"target"`
}

type Runner interface {
	Run(context.Context, []string, []byte) ([]byte, []byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, argv []string, input []byte) ([]byte, []byte, error) {
	if len(argv) == 0 {
		return nil, nil, errors.New("external collector command is empty")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Collectors are bounded as a process tree, not just as one parent process.
	// This also prevents a grandchild that inherits stdout/stderr from keeping
	// Cmd.Wait blocked after the five-second collector deadline.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return killProcessGroup(command.Process.Pid)
	}
	command.WaitDelay = defaultWaitDelay
	command.Stdin = bytes.NewReader(input)
	command.Env = allowedEnvironment()
	var stdout limitedBuffer
	var stderr limitedBuffer
	stdout.limit = maxOutputBytes
	stderr.limit = maxOutputBytes
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if command.Process != nil {
		// A successful collector must not leave detached descendants behind.
		_ = killProcessGroup(command.Process.Pid)
	}
	if stdout.exceeded || stderr.exceeded {
		return stdout.Bytes(), stderr.Bytes(), errors.New("external collector output exceeds 1 MiB")
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func killProcessGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func allowedEnvironment() []string {
	allowed := map[string]struct{}{
		"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {}, "SHELL": {},
		"TMPDIR": {}, "TEMP": {}, "TMP": {}, "LANG": {}, "LC_ALL": {}, "LC_CTYPE": {}, "NO_COLOR": {},
		"XDG_CONFIG_HOME": {}, "XDG_DATA_HOME": {}, "XDG_CACHE_HOME": {}, "XDG_STATE_HOME": {},
		"CODEX_HOME": {}, "CLAUDE_CONFIG_DIR": {}, "OPENCODE_DB": {},
		"HERDR_PLUGIN_CONFIG_DIR": {}, "HERDR_PLUGIN_STATE_DIR": {},
	}
	result := make([]string, 0, len(allowed))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, ok := allowed[key]; ok {
			result = append(result, entry)
		}
	}
	sort.Strings(result)
	return result
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return written, nil
	}
	if len(data) > remaining {
		buffer.exceeded = true
		data = data[:remaining]
	}
	_, _ = buffer.Buffer.Write(data)
	return written, nil
}

type Collector struct {
	commands     map[string][]string
	processNames map[string]string
	runner       Runner
	timeout      time.Duration
	now          func() time.Time
	version      int
}

type Option func(*Collector)

func WithRunner(runner Runner) Option {
	return func(item *Collector) { item.runner = runner }
}

func WithTimeout(timeout time.Duration) Option {
	return func(item *Collector) { item.timeout = timeout }
}

func WithNow(now func() time.Time) Option {
	return func(item *Collector) { item.now = now }
}

func New(config Config, options ...Option) (*Collector, error) {
	if config.SchemaVersion != LegacyContractVersion && config.SchemaVersion != ContractVersion {
		return nil, fmt.Errorf("external collector config schema is %d; expected %d or %d", config.SchemaVersion, LegacyContractVersion, ContractVersion)
	}
	item := &Collector{
		commands:     make(map[string][]string, len(config.Collectors)),
		processNames: make(map[string]string),
		runner:       ExecRunner{},
		timeout:      defaultTimeout,
		now:          time.Now,
		version:      config.SchemaVersion,
	}
	for harness, entry := range config.Collectors {
		canonical := collector.CanonicalHarness(harness)
		if canonical == "" {
			return nil, errors.New("external collector harness is empty")
		}
		if len(entry.Command) == 0 || strings.TrimSpace(entry.Command[0]) == "" {
			return nil, fmt.Errorf("external collector %q has an empty command", canonical)
		}
		if _, exists := item.commands[canonical]; exists {
			return nil, fmt.Errorf("duplicate external collector for %q", canonical)
		}
		item.commands[canonical] = append([]string(nil), entry.Command...)
		for _, configuredName := range entry.ProcessNames {
			name := normalizeProcessName(configuredName)
			if name == "" {
				return nil, fmt.Errorf("external collector %q has an invalid process name", canonical)
			}
			if existing, exists := item.processNames[name]; exists && existing != canonical {
				return nil, fmt.Errorf("process name %q is assigned to both %q and %q", name, existing, canonical)
			}
			item.processNames[name] = canonical
		}
	}
	for _, option := range options {
		option(item)
	}
	if item.runner == nil {
		return nil, errors.New("external collector runner is nil")
	}
	if item.timeout <= 0 {
		return nil, errors.New("external collector timeout must be positive")
	}
	if item.now == nil {
		return nil, errors.New("external collector clock is nil")
	}
	return item, nil
}

func normalizeProcessName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = filepath.Base(value)
	if value == "" || value == "." || value == string(filepath.Separator) || len(value) > 128 {
		return ""
	}
	for _, char := range value {
		if char <= 0x20 || char == 0x7f {
			return ""
		}
	}
	return value
}

func (item *Collector) MatchProcess(processes []collector.Process) (string, error) {
	var match string
	for _, process := range processes {
		candidates := []string{process.Name, process.Argv0}
		if len(process.Argv) > 0 {
			candidates = append(candidates, process.Argv[0])
		}
		for _, candidate := range candidates {
			harness := item.processNames[normalizeProcessName(candidate)]
			if harness == "" {
				continue
			}
			if match != "" && match != harness {
				return "", fmt.Errorf("%w: foreground processes match %s and %s", collector.ErrAmbiguous, match, harness)
			}
			match = harness
		}
	}
	return match, nil
}

func (item *Collector) HasProcessMatchers() bool {
	return item != nil && len(item.processNames) > 0
}

func Load(path string, options ...Option) (*Collector, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode external collector config: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode external collector config: %w", err)
	}
	return New(config, options...)
}

func DefaultConfigPath() string {
	if root := strings.TrimSpace(os.Getenv("HERDR_PLUGIN_CONFIG_DIR")); root != "" {
		return filepath.Join(root, "collectors.json")
	}
	if root, err := os.UserConfigDir(); err == nil && root != "" {
		return filepath.Join(root, "token-usage", "collectors.json")
	}
	return "collectors.json"
}

func (item *Collector) Harnesses() []string {
	harnesses := make([]string, 0, len(item.commands))
	for harness := range item.commands {
		harnesses = append(harnesses, harness)
	}
	sort.Strings(harnesses)
	return harnesses
}

func (item *Collector) Collect(ctx context.Context, target collector.Target) (usage.Snapshot, error) {
	harness := collector.CanonicalHarness(target.Harness)
	argv, ok := item.commands[harness]
	if !ok {
		return usage.Snapshot{}, fmt.Errorf("%w: %s", collector.ErrUnsupported, target.Harness)
	}

	request, err := json.Marshal(Request{SchemaVersion: item.version, Target: target})
	if err != nil {
		return usage.Snapshot{}, fmt.Errorf("encode external collector request: %w", err)
	}
	commandCtx, cancel := context.WithTimeout(ctx, item.timeout)
	defer cancel()
	stdout, stderr, err := item.runner.Run(commandCtx, argv, request)
	if err != nil {
		if contextErr := commandCtx.Err(); contextErr != nil {
			if errors.Is(contextErr, context.DeadlineExceeded) {
				return usage.Snapshot{}, fmt.Errorf("external %s collector timed out after %s: %w", harness, item.timeout, contextErr)
			}
			return usage.Snapshot{}, contextErr
		}
		detail := usage.SanitizeText(string(stderr), 512)
		if detail != "" {
			return usage.Snapshot{}, fmt.Errorf("external %s collector failed: %w: %s", harness, err, detail)
		}
		return usage.Snapshot{}, fmt.Errorf("external %s collector failed: %w", harness, err)
	}

	var response usage.Snapshot
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return usage.Snapshot{}, fmt.Errorf("%w: decode external %s collector response: %v", collector.ErrMalformed, harness, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return usage.Snapshot{}, fmt.Errorf("%w: decode external %s collector response: %v", collector.ErrMalformed, harness, err)
	}
	if response.SchemaVersion != item.version {
		return usage.Snapshot{}, fmt.Errorf("%w: external %s schema is %d; expected %d", collector.ErrMalformed, harness, response.SchemaVersion, item.version)
	}
	if item.version == LegacyContractVersion {
		var legacyEnvelope struct {
			Quota json.RawMessage `json:"quota"`
		}
		if json.Unmarshal(stdout, &legacyEnvelope) != nil || legacyEnvelope.Quota != nil {
			return usage.Snapshot{}, fmt.Errorf("%w: external %s schema %d does not support quota", collector.ErrMalformed, harness, item.version)
		}
	}
	response.SchemaVersion = usage.SchemaVersion
	if response.Harness == "" {
		response.Harness = harness
	}
	if collector.CanonicalHarness(response.Harness) != harness {
		return usage.Snapshot{}, fmt.Errorf("%w: external collector returned harness %q for %q", collector.ErrMalformed, response.Harness, harness)
	}
	response.Harness = harness
	if response.SessionID == "" {
		response.SessionID = target.SessionID
	}
	if target.SessionID != "" && response.SessionID != target.SessionID {
		return usage.Snapshot{}, fmt.Errorf("%w: external %s collector returned a different session", collector.ErrMalformed, harness)
	}
	if response.Confidence == "" {
		response.Confidence = target.Confidence
	}
	if target.Confidence == usage.ConfidenceEstimated {
		response.Confidence = usage.ConfidenceEstimated
	}
	if response.Source == "" {
		response.Source = "external-collector"
	}
	if response.CollectedAt.IsZero() {
		response.CollectedAt = item.now().UTC()
	}
	if err := response.Validate(); err != nil {
		return usage.Snapshot{}, fmt.Errorf("%w: invalid external %s collector response: %v", collector.ErrMalformed, harness, err)
	}
	return response, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("multiple JSON values are not allowed")
}
