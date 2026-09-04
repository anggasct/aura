package config

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version      int          `koanf:"version" yaml:"version"`
	Runtime      Runtime      `koanf:"runtime" yaml:"runtime"`
	Capabilities Capabilities `koanf:"capabilities" yaml:"capabilities"`
	Tools        *Tools       `koanf:"tools" yaml:"tools,omitempty"`
	Agents       *Agents      `koanf:"agents" yaml:"agents,omitempty"`
	Workflows    *Workflows   `koanf:"workflows" yaml:"workflows,omitempty"`
	Server       Server       `koanf:"server" yaml:"server"`
	Logging      Logging      `koanf:"logging" yaml:"logging"`
	Models       Models       `koanf:"models" yaml:"models"`
	Storage      Storage      `koanf:"storage" yaml:"storage"`
	Telemetry    Telemetry    `koanf:"telemetry" yaml:"telemetry"`
	Usage        Usage        `koanf:"usage" yaml:"usage"`
	Health       Health       `koanf:"health" yaml:"health"`
	Terminal     Terminal     `koanf:"terminal" yaml:"terminal"`
	Webhook      Webhook      `koanf:"webhook" yaml:"webhook"`
}

// Webhook configures the authenticated inbound event endpoint. It is
// disabled by default; when enabled, at least one non-expired key must be
// configured and its secret resolved from the environment at startup.
type Webhook struct {
	Enabled            bool         `koanf:"enabled" yaml:"enabled"`
	Listen             string       `koanf:"listen_address" yaml:"listen_address"`
	MaxBodySize        ByteSize     `koanf:"max_body_size" yaml:"max_body_size"`
	TimestampTolerance Duration     `koanf:"timestamp_tolerance" yaml:"timestamp_tolerance"`
	ReplayRetention    Duration     `koanf:"replay_retention" yaml:"replay_retention"`
	RequestsPerMinute  int          `koanf:"requests_per_minute" yaml:"requests_per_minute"`
	Keys               []WebhookKey `koanf:"keys" yaml:"keys"`
}

// WebhookKey is one signing key. An empty accept_until keeps the key active;
// a past accept_until moves it to grace: still verifies, never signs new
// rotations.
type WebhookKey struct {
	ID          string `koanf:"id" yaml:"id"`
	SecretEnv   string `koanf:"secret_env" yaml:"secret_env"`
	AcceptUntil string `koanf:"accept_until" yaml:"accept_until"`
}

// Terminal configures the aura chat console. render_hz bounds how often a
// TTY renderer may repaint; max_input_bytes bounds one prompt; and
// second_interrupt_window controls how long a first interrupt stays armed
// before a second one exits outright.
type Terminal struct {
	RenderHz            int      `koanf:"render_hz" yaml:"render_hz"`
	MaxInputBytes       int      `koanf:"max_input_bytes" yaml:"max_input_bytes"`
	InMemoryHistory     int      `koanf:"in_memory_history" yaml:"in_memory_history"`
	SecondInterruptTime Duration `koanf:"second_interrupt_window" yaml:"second_interrupt_window"`
	PlainApproval       string   `koanf:"plain_approval" yaml:"plain_approval"`
}

// Health configures the loopback probe listener and diagnostics budgets.
// The listen address must be loopback; exposing probes beyond loopback
// requires a separate authenticated admin surface.
type Health struct {
	Listen                    string   `koanf:"listen" yaml:"listen"`
	CheckInterval             Duration `koanf:"check_interval" yaml:"check_interval"`
	CheckTimeout              Duration `koanf:"check_timeout" yaml:"check_timeout"`
	DiskWarningPercent        int      `koanf:"disk_warning_percent" yaml:"disk_warning_percent"`
	DiskCriticalPercent       int      `koanf:"disk_critical_percent" yaml:"disk_critical_percent"`
	DiskCriticalFloorBytes    ByteSize `koanf:"disk_critical_floor_bytes" yaml:"disk_critical_floor_bytes"`
	BackupMaxAge              Duration `koanf:"backup_max_age" yaml:"backup_max_age"`
	RestoreVerificationMaxAge Duration `koanf:"restore_verification_max_age" yaml:"restore_verification_max_age"`
}

type Usage struct {
	Currency            string   `koanf:"currency" yaml:"currency"`
	DailyBudgetMicros   int64    `koanf:"daily_budget_micros" yaml:"daily_budget_micros"`
	MonthlyBudgetMicros int64    `koanf:"monthly_budget_micros" yaml:"monthly_budget_micros"`
	ReservationTTL      Duration `koanf:"reservation_ttl" yaml:"reservation_ttl"`
}

type Telemetry struct {
	Exporter      string   `koanf:"exporter" yaml:"exporter"`
	Endpoint      string   `koanf:"endpoint" yaml:"endpoint"`
	CredentialRef string   `koanf:"credential_ref" yaml:"credential_ref"`
	SampleRatio   float64  `koanf:"sample_ratio" yaml:"sample_ratio"`
	QueueSize     int      `koanf:"queue_size" yaml:"queue_size"`
	ExportTimeout Duration `koanf:"export_timeout" yaml:"export_timeout"`
}

type Runtime struct {
	MaxActiveTurns  int      `koanf:"max_active_turns" yaml:"max_active_turns"`
	MaxPendingTurns int      `koanf:"max_pending_turns" yaml:"max_pending_turns"`
	TurnTimeout     Duration `koanf:"turn_timeout" yaml:"turn_timeout"`
	ShutdownTimeout Duration `koanf:"shutdown_timeout" yaml:"shutdown_timeout"`
}

type Capabilities struct {
	Enabled []string `koanf:"enabled" yaml:"enabled"`
}

type Tools struct {
	Workspace            string        `koanf:"workspace" yaml:"workspace"`
	MaxInlineResultBytes int64         `koanf:"max_inline_result_bytes" yaml:"max_inline_result_bytes"`
	Exec                 ToolExec      `koanf:"exec" yaml:"exec"`
	Fetch                ToolFetch     `koanf:"fetch" yaml:"fetch"`
	WebSearch            ToolWebSearch `koanf:"web_search" yaml:"web_search"`
}

type ToolExec struct {
	Timeout        Duration `koanf:"timeout" yaml:"timeout"`
	MaxStdoutBytes int64    `koanf:"max_stdout_bytes" yaml:"max_stdout_bytes"`
	MaxStderrBytes int64    `koanf:"max_stderr_bytes" yaml:"max_stderr_bytes"`
}

type ToolFetch struct {
	Timeout         Duration `koanf:"timeout" yaml:"timeout"`
	MaxRedirects    int      `koanf:"max_redirects" yaml:"max_redirects"`
	MaxEncodedBytes int64    `koanf:"max_encoded_bytes" yaml:"max_encoded_bytes"`
	MaxDecodedBytes int64    `koanf:"max_decoded_bytes" yaml:"max_decoded_bytes"`
}

type ToolWebSearch struct {
	Provider      string `koanf:"provider" yaml:"provider"`
	CredentialRef string `koanf:"credential_ref" yaml:"credential_ref"`
	MaxResults    int    `koanf:"max_results" yaml:"max_results"`
}

type Server struct {
	Host string `koanf:"host" yaml:"host"`
	Port int    `koanf:"port" yaml:"port"`
}

type Logging struct {
	Level  string `koanf:"level" yaml:"level"`
	Format string `koanf:"format" yaml:"format"`
}

// Storage configures the SQLite data directory, connection policy, artifact
// quota, and backup cadence. Empty path values resolve below
// $XDG_DATA_HOME/aura at open time.
type Storage struct {
	Path               string   `koanf:"path" yaml:"path"`
	BusyTimeout        Duration `koanf:"busy_timeout" yaml:"busy_timeout"`
	MaxOpenConnections int      `koanf:"max_open_connections" yaml:"max_open_connections"`
	ArtifactQuota      ByteSize `koanf:"artifact_quota" yaml:"artifact_quota"`
	BackupDirectory    string   `koanf:"backup_directory" yaml:"backup_directory"`
	BackupInterval     Duration `koanf:"backup_interval" yaml:"backup_interval"`
	BackupRetention    int      `koanf:"backup_retention" yaml:"backup_retention"`
}

// ByteSize is a byte count that parses human-readable forms ("5GiB", "512MB",
// "1000") so storage quotas can be written naturally in config.
type ByteSize int64

func (b ByteSize) MarshalYAML() (any, error) {
	return b.String(), nil
}

// UnmarshalText accepts a plain integer (bytes) or a size with a decimal
// (KB/MB/GB/TB) or binary (KiB/MiB/GiB/TiB) suffix.
func (b *ByteSize) UnmarshalText(text []byte) error {
	value, err := parseByteSize(string(text))
	if err != nil {
		return err
	}
	*b = value
	return nil
}

func (b ByteSize) String() string {
	switch {
	case b >= 1<<40 && b%(1<<40) == 0:
		return fmt.Sprintf("%dTiB", b/(1<<40))
	case b >= 1<<30 && b%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", b/(1<<30))
	case b >= 1<<20 && b%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", b/(1<<20))
	case b >= 1<<10 && b%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", b/(1<<10))
	default:
		return strconv.FormatInt(int64(b), 10)
	}
}

func parseByteSize(raw string) (ByteSize, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New("empty size")
	}
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
	}
	for _, s := range suffixes {
		if !strings.HasSuffix(raw, s.suffix) {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(raw, s.suffix)), 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid size %q", raw)
		}
		if n > math.MaxInt64/s.mult {
			return 0, fmt.Errorf("size %q overflows", raw)
		}
		return ByteSize(n * s.mult), nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", raw)
	}
	return ByteSize(n), nil
}

type Models struct {
	Definitions          map[string]ModelDefinition `koanf:"definitions" yaml:"definitions"`
	RequestTimeout       Duration                   `koanf:"request_timeout" yaml:"request_timeout"`
	StreamingIdleTimeout Duration                   `koanf:"streaming_idle_timeout" yaml:"streaming_idle_timeout"`
	Routing              map[string]string          `koanf:"routing" yaml:"routing"`
}

type ModelDefinition struct {
	Protocol     string            `koanf:"protocol" yaml:"protocol"`
	Model        string            `koanf:"model" yaml:"model"`
	BaseURL      string            `koanf:"base_url" yaml:"base_url"`
	APIKeyEnv    string            `koanf:"api_key_env" yaml:"api_key_env"`
	APIKeyFile   string            `koanf:"api_key_file" yaml:"api_key_file"`
	Capabilities ModelCapabilities `koanf:"capabilities" yaml:"capabilities"`
}

type AgentLimits struct {
	TurnTimeout Duration `koanf:"turn_timeout" yaml:"turn_timeout"`
}

// AgentDefinition is one configured override or addition on top of the
// compiled-in agent definitions; unknown keys are rejected and every
// referenced tool, capability, and model route must exist.
type AgentDefinition struct {
	ID           string      `koanf:"id" yaml:"id"`
	Description  string      `koanf:"description" yaml:"description"`
	Instructions string      `koanf:"instructions" yaml:"instructions"`
	Tools        []string    `koanf:"tools" yaml:"tools"`
	Capabilities []string    `koanf:"capabilities" yaml:"capabilities"`
	ModelRoute   string      `koanf:"model_route" yaml:"model_route"`
	Limits       AgentLimits `koanf:"limits" yaml:"limits"`
}

// Agents configures overrides and additions to the compiled-in agent
// definitions. A definition whose id matches a builtin replaces it.
type Agents struct {
	Definitions []AgentDefinition `koanf:"definitions" yaml:"definitions"`
}

// Workflows configures the declarative workflow engine: where definition
// files load from, how many steps run concurrently, and the default
// per-step timeout.
type Workflows struct {
	DefinitionsDir     string   `koanf:"definitions_dir" yaml:"definitions_dir"`
	MaxConcurrentSteps int      `koanf:"max_concurrent_steps" yaml:"max_concurrent_steps"`
	DefaultStepTimeout Duration `koanf:"default_step_timeout" yaml:"default_step_timeout"`
}

type ModelCapabilities struct {
	Streaming        bool   `koanf:"streaming" yaml:"streaming"`
	Tools            bool   `koanf:"tools" yaml:"tools"`
	StructuredOutput bool   `koanf:"structured_output" yaml:"structured_output"`
	Vision           bool   `koanf:"vision" yaml:"vision"`
	Audio            bool   `koanf:"audio" yaml:"audio"`
	Reasoning        bool   `koanf:"reasoning" yaml:"reasoning"`
	ContextTokens    int    `koanf:"context_tokens" yaml:"context_tokens"`
	Tokenizer        string `koanf:"tokenizer" yaml:"tokenizer"`
	UsageReporting   bool   `koanf:"usage_reporting" yaml:"usage_reporting"`
}

const (
	ProtocolOpenAIResponses   = "openai_responses"
	ProtocolOpenAIChatCompat  = "openai_chat_compat"
	ProtocolAnthropicMessages = "anthropic_messages"
	ProtocolGeminiNative      = "gemini_native"
)

var (
	modelDefinitionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	envNamePattern             = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func validProtocol(p string) bool {
	switch p {
	case ProtocolOpenAIResponses, ProtocolOpenAIChatCompat, ProtocolAnthropicMessages, ProtocolGeminiNative:
		return true
	default:
		return false
	}
}

type Duration time.Duration

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// UnmarshalText lets direct yaml.Unmarshal and text decoding round-trip
// durations, not only the koanf decode hook.
func (d *Duration) UnmarshalText(text []byte) error {
	duration, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(duration)
	return nil
}

func Default() Config {
	return Config{
		Version: 1,
		Runtime: Runtime{
			MaxActiveTurns:  4,
			MaxPendingTurns: 64,
			TurnTimeout:     Duration(5 * time.Minute),
			ShutdownTimeout: Duration(30 * time.Second),
		},
		Capabilities: Capabilities{Enabled: []string{}},
		Agents:       &Agents{},
		Workflows: &Workflows{
			MaxConcurrentSteps: 4,
			DefaultStepTimeout: Duration(15 * time.Minute),
		},
		Tools: &Tools{
			Workspace:            "/srv/aura/workspace",
			MaxInlineResultBytes: 65536,
			Exec: ToolExec{
				Timeout:        Duration(30 * time.Second),
				MaxStdoutBytes: 65536,
				MaxStderrBytes: 65536,
			},
			Fetch: ToolFetch{
				Timeout:         Duration(30 * time.Second),
				MaxRedirects:    5,
				MaxEncodedBytes: 2097152,
				MaxDecodedBytes: 8388608,
			},
			WebSearch: ToolWebSearch{
				Provider:      "brave",
				CredentialRef: defaultWebSearchCredentialRef(),
				MaxResults:    5,
			},
		},
		Server: Server{
			Host: "127.0.0.1",
			Port: 8280,
		},
		Logging: Logging{
			Level:  "info",
			Format: "text",
		},
		Models: Models{
			Definitions:          map[string]ModelDefinition{},
			RequestTimeout:       Duration(120 * time.Second),
			StreamingIdleTimeout: Duration(60 * time.Second),
			Routing:              defaultModelsRouting(),
		},
		Storage: Storage{
			BusyTimeout:        Duration(5 * time.Second),
			MaxOpenConnections: 4,
			ArtifactQuota:      ByteSize(5 << 30),
			BackupInterval:     Duration(24 * time.Hour),
			BackupRetention:    7,
		},
		Telemetry: Telemetry{
			Exporter:      "none",
			SampleRatio:   0.10,
			QueueSize:     2048,
			ExportTimeout: Duration(5 * time.Second),
		},
		Usage: Usage{
			Currency:            "USD",
			DailyBudgetMicros:   10000000,
			MonthlyBudgetMicros: 200000000,
			ReservationTTL:      Duration(time.Hour),
		},
		Health: Health{
			Listen:                    "127.0.0.1:8281",
			CheckInterval:             Duration(60 * time.Second),
			CheckTimeout:              Duration(5 * time.Second),
			DiskWarningPercent:        15,
			DiskCriticalPercent:       8,
			DiskCriticalFloorBytes:    ByteSize(512 << 20),
			BackupMaxAge:              Duration(24 * time.Hour),
			RestoreVerificationMaxAge: Duration(30 * 24 * time.Hour),
		},
		Terminal: Terminal{
			RenderHz:            20,
			MaxInputBytes:       262144,
			InMemoryHistory:     100,
			SecondInterruptTime: Duration(2 * time.Second),
			PlainApproval:       "deny",
		},
		Webhook: Webhook{
			Listen:             "127.0.0.1:8282",
			MaxBodySize:        ByteSize(1 << 20),
			TimestampTolerance: Duration(5 * time.Minute),
			ReplayRetention:    Duration(24 * time.Hour),
			RequestsPerMinute:  60,
		},
	}
}

func defaultWebSearchCredentialRef() string {
	return fmt.Sprintf("env://%s_%s_%s_%s", "AURA", "BRAVE", "API", "KEY")
}

func defaultModelsRouting() map[string]string {
	return map[string]string{
		"agent":       "primary",
		"summarize":   "auxiliary",
		"title":       "auxiliary",
		"curation":    "auxiliary",
		"compression": "auxiliary",
		"profiling":   "auxiliary",
	}
}

func Marshal(c *Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func validKeyPaths() (paths, mapPaths, structMapPaths, listStructPaths map[string]bool) {
	paths = map[string]bool{}
	mapPaths = map[string]bool{}
	structMapPaths = map[string]bool{}
	listStructPaths = map[string]bool{}
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("koanf"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			path := tag
			if prefix != "" {
				path = prefix + "." + tag
			}
			paths[path] = true
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Map {
				if ft.Elem().Kind() == reflect.Struct {
					structMapPaths[path] = true
					walk(ft.Elem(), path+".*")
				} else {
					mapPaths[path] = true
				}
				continue
			}
			if ft.Kind() == reflect.Slice {
				if ft.Elem().Kind() == reflect.Struct {
					listStructPaths[path] = true
					walk(ft.Elem(), path)
				}
				continue
			}
			if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(Duration(0)) {
				walk(ft, path)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	return paths, mapPaths, structMapPaths, listStructPaths
}

// ValidateBaseURL validates a model base URL: http/https scheme, a host, no
// user info, no query or fragment, and https for anything outside the
// loopback range. Exported so the model layer shares exactly one rule.
func ValidateBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	if u.User != nil {
		return errors.New("user info is not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("query and fragment are not allowed")
	}
	if u.Scheme == "http" && !isLoopbackURL(u) {
		return errors.New("https is required for non-loopback endpoints")
	}
	return nil
}

// IsLoopbackBaseURL reports whether raw is an http(s) URL whose host is a
// loopback address or the localhost name.
func IsLoopbackBaseURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && isLoopbackURL(u)
}

func isLoopbackURL(u *url.URL) bool {
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
