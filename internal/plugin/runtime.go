package plugin

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/config"
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
		active, held := s.activate(ctx, plugins, record)
		if held != nil {
			plan.Held = append(plan.Held, *held)
			continue
		}
		plan.Active = append(plan.Active, *active)
	}
	return plan, nil
}

func holdFor(record *Record, err error) *Held {
	code, ok := CodeOf(err)
	if !ok || code != ErrTampered {
		code = ErrSurfacePending
	}
	return &Held{ID: record.ID, Code: code, Detail: err.Error()}
}

func (s *Service) activate(ctx context.Context, plugins *os.Root, record *Record) (*Active, *Held) {
	if record.ApprovedVersion == "" {
		return nil, holdFor(record, Errorf(ErrSurfacePending, "the plugin has no approved version"))
	}
	row, err := s.registry.Version(ctx, record.ID, record.Version)
	if err != nil {
		return nil, holdFor(record, Errorf(ErrSurfacePending, "the installed version is not recorded"))
	}
	manifest, bundleSHA256, err := decodeStored(row.ManifestJSON)
	if err != nil {
		return nil, holdFor(record, Errorf(ErrSurfacePending, "the installed version is unreadable"))
	}
	if err := s.verify(plugins, &row); err != nil {
		return nil, holdFor(record, err)
	}
	approved, err := s.approvedSurface(ctx, record)
	if err != nil {
		return nil, holdFor(record, Errorf(ErrSurfacePending, "the approved version is unreadable"))
	}
	current, err := SurfaceOf(manifest, bundleSHA256)
	if err != nil {
		return nil, holdFor(record, err)
	}
	if diff := Compare(&approved, &current); diff.Quarantines() {
		return nil, holdFor(record, Errorf(ErrSurfacePending, "what the plugin runs changed since the approval"))
	}
	grantRows, err := s.registry.Grants(ctx, record.ID)
	if err != nil {
		return nil, holdFor(record, Errorf(ErrSurfacePending, "the approvals are unreadable"))
	}
	granted := make(map[GrantKey]string, len(grantRows))
	for i := range grantRows {
		granted[grantRows[i].GrantKey] = grantRows[i].Fingerprint
	}
	if missing := requiredPending(manifest, granted); missing != "" {
		return nil, holdFor(record, Errorf(ErrSurfacePending, "%s is waiting for review", missing))
	}
	active := &Active{
		ID:      record.ID,
		Version: record.Version,
		Server:  manifest.ServerConfig(filepath.Join(s.dir, record.ID, record.Version)),
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
		if parsed, err := url.Parse(manifest.Backend.URL); err == nil {
			for _, host := range []string{strings.ToLower(parsed.Host), strings.ToLower(parsed.Hostname())} {
				if granted[GrantKey{SurfaceEgress, host}] == ValueFingerprint(SurfaceEgress, host) {
					return ""
				}
			}
		}
		return "egress for the backend url"
	}
	return ""
}
