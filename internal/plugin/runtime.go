package plugin

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/plugin/auth"
)

type ApprovedTool struct {
	Name        string
	Description string
	Tier        string
}

type Active struct {
	ID      string
	Version string
	Server  config.MCPServer
	Auth    auth.Strategy
	Tools   []ApprovedTool
}

type Held struct {
	ID     string
	Code   ErrorCode
	Detail string
}

type Plan struct {
	Active []Active
	Held   []Held
}

func (s *Service) Plan(ctx context.Context) (*Plan, error) {
	if ctx == nil {
		return nil, Errorf(ErrInvalidArgument, "a context is required")
	}
	records, err := s.registry.List(ctx)
	if err != nil {
		return nil, err
	}
	plan := &Plan{}
	if !slices.ContainsFunc(records, func(record Record) bool { return record.State == StateActive }) {
		return plan, nil
	}
	plugins, err := s.openPlugins()
	if err != nil {
		return nil, err
	}
	defer func() { _ = plugins.Close() }()
	for i := range records {
		record := &records[i]
		if record.State != StateActive {
			continue
		}
		active, err := s.activate(ctx, plugins, record)
		if err == nil {
			err = readiness(ctx, active)
		}
		if err != nil {
			plan.Held = append(plan.Held, holdFor(record, err))
			continue
		}
		plan.Active = append(plan.Active, *active)
	}
	return plan, nil
}

func holdFor(record *Record, err error) Held {
	code, ok := CodeOf(err)
	if !ok || (code != ErrTampered && code != ErrAuthRequired && code != ErrAuthFailed) {
		code = ErrSurfacePending
	}
	return Held{ID: record.ID, Code: code, Detail: err.Error()}
}

func (a *Active) AuthStatus(ctx context.Context) ErrorCode {
	code, _ := CodeOf(readiness(ctx, a))
	return code
}

func readiness(ctx context.Context, active *Active) error {
	return authError(active.Auth.Ready(ctx))
}

func authError(err error) error {
	if err == nil {
		return nil
	}
	if code, coded := auth.CodeOf(err); coded && code == auth.ErrFailed {
		return Errorf(ErrAuthFailed, "the authorization was rejected; run aura plugin login and restart")
	}
	return Errorf(ErrAuthRequired, "the plugin has no usable authorization; run aura plugin login and restart")
}

func (s *Service) activate(ctx context.Context, plugins *os.Root, record *Record) (*Active, error) {
	if record.ApprovedVersion == "" {
		return nil, Errorf(ErrSurfacePending, "the plugin has no approved version")
	}
	row, err := s.registry.Version(ctx, record.ID, record.Version)
	if err != nil {
		return nil, Errorf(ErrSurfacePending, "the installed version is not recorded")
	}
	manifest, bundleSHA256, err := decodeStored(row.ManifestJSON)
	if err != nil {
		return nil, Errorf(ErrSurfacePending, "the installed version is unreadable")
	}
	if err := s.verify(plugins, &row); err != nil {
		return nil, err
	}
	approved, err := s.approvedSurface(ctx, record)
	if err != nil {
		return nil, Errorf(ErrSurfacePending, "the approved version is unreadable")
	}
	current, err := SurfaceOf(manifest, bundleSHA256)
	if err != nil {
		return nil, err
	}
	if diff := Compare(&approved, &current); diff.Quarantines() {
		return nil, Errorf(ErrSurfacePending, "what the plugin runs changed since the approval")
	}
	grantRows, err := s.registry.Grants(ctx, record.ID)
	if err != nil {
		return nil, Errorf(ErrSurfacePending, "the approvals are unreadable")
	}
	granted := make(map[GrantKey]string, len(grantRows))
	for i := range grantRows {
		granted[grantRows[i].GrantKey] = grantRows[i].Fingerprint
	}
	if missing := requiredPending(manifest, granted); missing != "" {
		return nil, Errorf(ErrSurfacePending, "%s is waiting for review", missing)
	}
	spec := manifest.AuthSpec(current.Auth)
	strategy, err := s.auth.Strategy(&spec)
	if err != nil {
		return nil, Errorf(ErrSurfacePending, "the auth strategy cannot be built")
	}
	active := &Active{
		ID:      record.ID,
		Version: record.Version,
		Server:  manifest.ServerConfig(filepath.Join(s.dir, record.ID, record.Version)),
		Auth:    strategy,
	}
	normalized := manifest.normalized()
	for i := range normalized.Tools {
		tool := &normalized.Tools[i]
		if granted[GrantKey{SurfaceTool, tool.Name}] == ToolFingerprint(tool) {
			active.Tools = append(active.Tools, ApprovedTool{Name: tool.Name, Description: tool.Description, Tier: tool.Tier})
		}
	}
	return active, nil
}

func requiredPending(manifest *Manifest, granted map[GrantKey]string) string {
	for _, capability := range manifest.Capabilities {
		if granted[GrantKey{SurfaceCapability, capability}] != ValueFingerprint(SurfaceCapability, capability) {
			return "capability " + capability
		}
	}
	for i := range manifest.Secrets {
		if granted[GrantKey{SurfaceSecret, manifest.Secrets[i].Name}] != SecretFingerprint(&manifest.Secrets[i]) {
			return "secret " + manifest.Secrets[i].Name
		}
	}
	if manifest.Backend.Transport == config.MCPTransportStreamableHTTP {
		if !egressGranted(manifest.Backend.URL, granted) {
			return "egress for the backend url"
		}
		if manifest.Auth.Kind == AuthOAuth2 && !egressGranted(manifest.Auth.TokenURL, granted) {
			return "egress for the token url"
		}
	}
	return ""
}

func egressGranted(raw string, granted map[GrantKey]string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	for _, host := range []string{strings.ToLower(parsed.Host), strings.ToLower(parsed.Hostname())} {
		if granted[GrantKey{SurfaceEgress, host}] == ValueFingerprint(SurfaceEgress, host) {
			return true
		}
	}
	return false
}
