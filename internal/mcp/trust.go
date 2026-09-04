package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/config"
)

type DiscoveredTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type ServerTrustContent struct {
	Name             string            `json:"name"`
	Transport        string            `json:"transport"`
	Command          string            `json:"command,omitempty"`
	Args             []string          `json:"args,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	URL              string            `json:"url,omitempty"`
	StartupTimeout   int64             `json:"startup_timeout"`
	RequestTimeout   int64             `json:"request_timeout"`
	ConnectTimeout   int64             `json:"connect_timeout"`
	MaxMessageSize   int64             `json:"max_message_size"`
	Restart          *TrustRestart     `json:"restart,omitempty"`
	Auth             *TrustAuth        `json:"auth,omitempty"`
	AllowRedirects   bool              `json:"allow_redirects"`
	CompatibilityAck bool              `json:"compatibility_ack"`
	Capabilities     []string          `json:"capabilities"`
	Tools            []ToolTrustInfo   `json:"tools"`
}

type TrustRestart struct {
	MaxAttempts int   `json:"max_attempts"`
	Window      int64 `json:"window"`
}

type TrustAuth struct {
	OAuthClientIDEnv     string `json:"oauth_client_id_env,omitempty"`
	OAuthClientSecretEnv string `json:"oauth_client_secret_env,omitempty"`
	OAuthTokenStore      string `json:"oauth_token_store,omitempty"`
	StaticHeader         string `json:"static_header,omitempty"`
	StaticCredentialRef  string `json:"static_credential_ref,omitempty"`
}

type ToolTrustInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SchemaHash  string `json:"schema_hash"`
}

func ComputeTrustDigest(serverCfg *config.MCPServer, tools []DiscoveredTool) (string, error) {
	if serverCfg == nil {
		return "", Errorf(ErrConfigInvalid, "server configuration is required")
	}

	sortedTools := slices.Clone(tools)
	slices.SortFunc(sortedTools, func(a, b DiscoveredTool) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})

	toolInfos := make([]ToolTrustInfo, 0, len(sortedTools))
	for _, t := range sortedTools {
		schemaSum := sha256.Sum256(t.InputSchema)
		toolInfos = append(toolInfos, ToolTrustInfo{
			Name:        t.Name,
			Description: t.Description,
			SchemaHash:  hex.EncodeToString(schemaSum[:]),
		})
	}

	sortedCaps := slices.Clone(serverCfg.Capabilities)
	slices.Sort(sortedCaps)
	if sortedCaps == nil {
		sortedCaps = []string{}
	}

	var restart *TrustRestart
	if serverCfg.Restart != nil {
		restart = &TrustRestart{
			MaxAttempts: serverCfg.Restart.MaxAttempts,
			Window:      int64(serverCfg.Restart.Window),
		}
	}

	var auth *TrustAuth
	if serverCfg.Auth != nil {
		auth = &TrustAuth{}
		if serverCfg.Auth.OAuth != nil {
			auth.OAuthClientIDEnv = serverCfg.Auth.OAuth.ClientIDEnv
			auth.OAuthClientSecretEnv = serverCfg.Auth.OAuth.ClientSecretEnv
			auth.OAuthTokenStore = serverCfg.Auth.OAuth.TokenStore
		}
		if serverCfg.Auth.Static != nil {
			auth.StaticHeader = serverCfg.Auth.Static.Header
			auth.StaticCredentialRef = serverCfg.Auth.Static.CredentialRef
		}
	}

	allowRedirects := serverCfg.AllowRedirects != nil && *serverCfg.AllowRedirects

	content := ServerTrustContent{
		Name:             serverCfg.Name,
		Transport:        serverCfg.Transport,
		Command:          serverCfg.Command,
		Args:             slices.Clone(serverCfg.Args),
		Environment:      copyStringMap(serverCfg.Environment),
		URL:              serverCfg.URL,
		StartupTimeout:   int64(serverCfg.StartupTimeout),
		RequestTimeout:   int64(serverCfg.RequestTimeout),
		ConnectTimeout:   int64(serverCfg.ConnectTimeout),
		MaxMessageSize:   int64(serverCfg.MaxMessageSize),
		Restart:          restart,
		Auth:             auth,
		AllowRedirects:   allowRedirects,
		CompatibilityAck: serverCfg.CompatibilityAck,
		Capabilities:     sortedCaps,
		Tools:            toolInfos,
	}

	payload, err := json.Marshal(content)
	if err != nil {
		return "", Wrap(ErrResultInvalid, err, "failed to marshal trust payload")
	}

	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func copyStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

type TrustDecision string

const (
	TrustDecisionApproved TrustDecision = "approved"
	TrustDecisionPending  TrustDecision = "pending"
	TrustDecisionDenied   TrustDecision = "denied"
)

type TrustRecord struct {
	ServerName   string        `json:"server_name"`
	Digest       string        `json:"digest"`
	Decision     TrustDecision `json:"decision"`
	Capabilities []string      `json:"capabilities"`
	Tools        []string      `json:"tools"`
	ReviewedAt   time.Time     `json:"reviewed_at"`
}

var ErrTrustNotFound = errors.New("mcp: trust record not found")

type TrustRegistry interface {
	GetTrust(ctx context.Context, serverName string) (*TrustRecord, error)
	SaveTrust(ctx context.Context, record *TrustRecord) error
	Approve(ctx context.Context, serverName, digest string) error
	IsTrusted(ctx context.Context, serverName, digest string) (bool, error)
}

type MemoryTrustRegistry struct {
	mu      sync.RWMutex
	records map[string]*TrustRecord
}

func NewMemoryTrustRegistry() *MemoryTrustRegistry {
	return &MemoryTrustRegistry{
		records: make(map[string]*TrustRecord),
	}
}

func (r *MemoryTrustRegistry) GetTrust(_ context.Context, serverName string) (*TrustRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rec, ok := r.records[serverName]
	if !ok || rec == nil {
		return nil, ErrTrustNotFound
	}
	recCopy := *rec
	recCopy.Capabilities = slices.Clone(rec.Capabilities)
	recCopy.Tools = slices.Clone(rec.Tools)
	return &recCopy, nil
}

func (r *MemoryTrustRegistry) SaveTrust(_ context.Context, record *TrustRecord) error {
	if record == nil {
		return Errorf(ErrConfigInvalid, "trust record must not be nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	recCopy := *record
	recCopy.Capabilities = slices.Clone(record.Capabilities)
	recCopy.Tools = slices.Clone(record.Tools)
	r.records[record.ServerName] = &recCopy
	return nil
}

func (r *MemoryTrustRegistry) Approve(_ context.Context, serverName, digest string) error {
	if serverName == "" {
		return Errorf(ErrConfigInvalid, "server name must not be empty")
	}
	if digest == "" {
		return Errorf(ErrConfigInvalid, "trust digest must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	rec := r.records[serverName]
	if rec == nil {
		rec = &TrustRecord{
			ServerName: serverName,
		}
	}
	rec.Digest = digest
	rec.Decision = TrustDecisionApproved
	rec.ReviewedAt = time.Now().UTC()
	r.records[serverName] = rec
	return nil
}

func (r *MemoryTrustRegistry) IsTrusted(_ context.Context, serverName, digest string) (bool, error) {
	if serverName == "" || digest == "" {
		return false, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	rec, ok := r.records[serverName]
	if !ok || rec == nil {
		return false, nil
	}
	return rec.Decision == TrustDecisionApproved && rec.Digest == digest, nil
}
