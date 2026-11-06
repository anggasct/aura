package plugin

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/plugin/auth"
	"github.com/anggasct/aura/internal/tools"
)

const (
	MinDigestPrefix = 12
	stagingPrefix   = ".staging-"
)

type Options struct {
	Registry Registry
	Auth     *auth.Registry
	Dir      string
	Now      func() time.Time
}

type Service struct {
	registry Registry
	auth     *auth.Registry
	dir      string
	now      func() time.Time
}

func NewService(opts *Options) (*Service, error) {
	if opts == nil || opts.Registry == nil || opts.Auth == nil {
		return nil, Errorf(ErrInvalidArgument, "a registry and an auth registry are required")
	}
	if !filepath.IsAbs(opts.Dir) {
		return nil, Errorf(ErrInvalidArgument, "the plugin directory must be absolute")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{registry: opts.Registry, auth: opts.Auth, dir: filepath.Clean(opts.Dir), now: now}, nil
}

type InstallResult struct {
	ID        string
	Version   string
	Digest    string
	State     string
	Unchanged bool
	Diff      Diff
	Accepted  bool
}

func (s *Service) openPlugins() (*os.Root, error) {
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return nil, Wrap(ErrStorage, err, "the plugin directory cannot be created")
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, Wrap(ErrStorage, err, "the plugin directory cannot be opened")
	}
	return root, nil
}

func bundleRel(id, version string) (string, error) {
	if !config.ValidPluginID(id) || !config.ValidPluginVersion(version) {
		return "", Errorf(ErrPathInvalid, "a plugin id or version is not valid")
	}
	return id + "/" + version, nil
}

func (s *Service) Install(ctx context.Context, source string) (*InstallResult, error) {
	if ctx == nil || source == "" {
		return nil, Errorf(ErrInvalidArgument, "a context and a source directory are required")
	}
	src, err := os.OpenRoot(source)
	if err != nil {
		return nil, Errorf(ErrPathInvalid, "the source directory cannot be opened")
	}
	defer func() { _ = src.Close() }()
	first, err := ReadManifest(src)
	if err != nil {
		return nil, err
	}
	plugins, err := s.openPlugins()
	if err != nil {
		return nil, err
	}
	defer func() { _ = plugins.Close() }()
	staging := stagingPrefix + rand.Text()
	if err := plugins.Mkdir(staging, dirMode); err != nil {
		return nil, Wrap(ErrStorage, err, "the staging directory cannot be created")
	}
	defer func() { _ = plugins.RemoveAll(staging) }()
	insp, err := s.stage(plugins, src, staging)
	if err != nil {
		return nil, err
	}
	if insp.Manifest.ID != first.ID || insp.Manifest.Version != first.Version {
		return nil, Errorf(ErrManifestInvalid, "the manifest changed while it was installed")
	}
	return s.register(ctx, plugins, staging, insp)
}

func (s *Service) stage(plugins, src *os.Root, staging string) (*Inspection, error) {
	stage, err := plugins.OpenRoot(staging)
	if err != nil {
		return nil, Wrap(ErrStorage, err, "the staging directory cannot be opened")
	}
	defer func() { _ = stage.Close() }()
	if err := copyTree(src, stage); err != nil {
		return nil, err
	}
	return Inspect(stage)
}

func (s *Service) register(ctx context.Context, plugins *os.Root, staging string, insp *Inspection) (*InstallResult, error) {
	manifest := insp.Manifest
	candidate, err := SurfaceOf(manifest, insp.BundleSHA256)
	if err != nil {
		return nil, err
	}
	manifestJSON, err := encodeStored(manifest, insp.BundleSHA256)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	result := &InstallResult{ID: manifest.ID, Version: manifest.Version, Digest: insp.Digest, State: StateQuarantined}
	record := InstallRecord{
		Plugin:  Record{ID: manifest.ID, State: StateQuarantined, Version: manifest.Version, UpdatedAt: now},
		Version: VersionRecord{ID: manifest.ID, Version: manifest.Version, Digest: insp.Digest, ManifestJSON: manifestJSON, InstalledAt: now},
	}
	existing, err := s.registry.Get(ctx, manifest.ID)
	switch {
	case err == nil:
		unchanged, err := s.planUpdate(ctx, &existing, insp, &candidate, &record, result)
		if err != nil {
			return nil, err
		}
		if unchanged {
			return result, nil
		}
	case isCode(err, ErrNotFound):
	default:
		return nil, err
	}
	final, err := bundleRel(manifest.ID, manifest.Version)
	if err != nil {
		return nil, err
	}
	if err := plugins.MkdirAll(manifest.ID, dirMode); err != nil {
		return nil, Wrap(ErrStorage, err, "the plugin directory cannot be created")
	}
	if err := plugins.RemoveAll(final); err != nil {
		return nil, Wrap(ErrStorage, err, "an unregistered bundle cannot be replaced")
	}
	if err := plugins.Rename(staging, final); err != nil {
		return nil, Wrap(ErrStorage, err, "the bundle cannot be placed")
	}
	if err := s.registry.Install(ctx, &record); err != nil {
		_ = plugins.RemoveAll(final)
		_ = plugins.Remove(manifest.ID)
		return nil, err
	}
	result.State = record.Plugin.State
	return result, nil
}

func (s *Service) planUpdate(ctx context.Context, existing *Record, insp *Inspection, candidate *Surface, record *InstallRecord, result *InstallResult) (bool, error) {
	manifest := insp.Manifest
	known, err := s.registry.Version(ctx, manifest.ID, manifest.Version)
	switch {
	case err == nil:
		if known.Digest != insp.Digest {
			return false, Errorf(ErrVersionConflict, "version %s is already installed with a different digest", manifest.Version)
		}
		result.State = existing.State
		result.Unchanged = true
		return true, nil
	case !isCode(err, ErrNotFound):
		return false, err
	}
	record.Plugin.State = existing.State
	record.Plugin.ApprovedVersion = existing.ApprovedVersion
	if existing.ApprovedVersion == "" {
		record.Plugin.State = StateQuarantined
		result.State = record.Plugin.State
		return false, nil
	}
	approved, err := s.approvedSurface(ctx, existing)
	if err != nil {
		return false, err
	}
	diff := Compare(&approved, candidate)
	result.Diff = diff
	switch {
	case diff.Quarantines():
		record.Plugin.State = StateQuarantined
	case diff.Widens():
	default:
		record.Plugin.ApprovedVersion = manifest.Version
		result.Accepted = true
		for _, removed := range diff.Removed {
			record.DropGrants = append(record.DropGrants, GrantKey{Surface: removed.Surface, Value: removed.Value})
		}
	}
	result.State = record.Plugin.State
	return false, nil
}

func (s *Service) approvedSurface(ctx context.Context, record *Record) (Surface, error) {
	row, err := s.registry.Version(ctx, record.ID, record.ApprovedVersion)
	if err != nil {
		return Surface{}, err
	}
	manifest, bundleSHA256, err := decodeStored(row.ManifestJSON)
	if err != nil {
		return Surface{}, err
	}
	return SurfaceOf(manifest, bundleSHA256)
}

func isCode(err error, code ErrorCode) bool {
	got, ok := CodeOf(err)
	return ok && got == code
}

func (s *Service) verify(plugins *os.Root, row *VersionRecord) error {
	rel, err := bundleRel(row.ID, row.Version)
	if err != nil {
		return err
	}
	bundle, err := plugins.OpenRoot(rel)
	if err != nil {
		return Errorf(ErrTampered, "the bundle of %s %s is missing", row.ID, row.Version)
	}
	defer func() { _ = bundle.Close() }()
	insp, err := Inspect(bundle)
	if err != nil {
		return Wrap(ErrTampered, err, fmt.Sprintf("the bundle of %s %s no longer validates", row.ID, row.Version))
	}
	if insp.Digest != row.Digest {
		return Errorf(ErrTampered, "the bundle of %s %s no longer matches its digest", row.ID, row.Version)
	}
	return nil
}

type BackendSummary struct {
	Kind      string
	Transport string
	Command   string
	Args      []string
	URL       string
}

type ReviewTool struct {
	ID          string
	Name        string
	Tier        string
	Description string
	Display     []string
}

type ReviewSecret struct {
	Name string
	Ref  string
}

type Review struct {
	ID              string
	Name            string
	Description     string
	Version         string
	ApprovedVersion string
	State           string
	Digest          string
	Backend         BackendSummary
	Auth            Auth
	Capabilities    []string
	Tools           []ReviewTool
	Egress          []string
	Secrets         []ReviewSecret
	Diff            *Diff
}

func (s *Service) Review(ctx context.Context, id string) (*Review, error) {
	record, row, manifest, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	plugins, err := s.openPlugins()
	if err != nil {
		return nil, err
	}
	defer func() { _ = plugins.Close() }()
	if err := s.verify(plugins, &row); err != nil {
		return nil, err
	}
	review := &Review{
		ID: record.ID, Name: manifest.Name, Description: manifest.Description,
		Version: record.Version, ApprovedVersion: record.ApprovedVersion, State: record.State, Digest: row.Digest,
		Backend: BackendSummary{
			Kind: manifest.Backend.Kind, Transport: manifest.Backend.Transport, Command: manifest.Backend.Command,
			Args: slices.Clone(manifest.Backend.Args), URL: manifest.Backend.URL,
		},
		Auth:         manifest.Auth,
		Capabilities: sortedCopy(manifest.Capabilities),
		Egress:       sortedCopy(manifest.Egress),
	}
	normalized := manifest.normalized()
	for i := range normalized.Tools {
		tool := &normalized.Tools[i]
		review.Tools = append(review.Tools, ReviewTool{ID: tools.PluginToolID(record.ID, tool.Name), Name: tool.Name, Tier: tool.Tier, Description: tool.Description, Display: slices.Clone(tool.Display)})
	}
	for i := range normalized.Secrets {
		review.Secrets = append(review.Secrets, ReviewSecret{Name: normalized.Secrets[i].Name, Ref: normalized.Secrets[i].Ref})
	}
	if record.ApprovedVersion != "" {
		approved, err := s.approvedSurface(ctx, &record)
		if err != nil {
			return nil, err
		}
		_, bundleSHA256, err := decodeStored(row.ManifestJSON)
		if err != nil {
			return nil, err
		}
		candidate, err := SurfaceOf(manifest, bundleSHA256)
		if err != nil {
			return nil, err
		}
		diff := Compare(&approved, &candidate)
		review.Diff = &diff
	}
	return review, nil
}

func (s *Service) load(ctx context.Context, id string) (Record, VersionRecord, *Manifest, error) {
	if ctx == nil || !config.ValidPluginID(id) {
		return Record{}, VersionRecord{}, nil, Errorf(ErrInvalidArgument, "a valid plugin id is required")
	}
	record, err := s.registry.Get(ctx, id)
	if err != nil {
		return Record{}, VersionRecord{}, nil, err
	}
	row, err := s.registry.Version(ctx, id, record.Version)
	if err != nil {
		return Record{}, VersionRecord{}, nil, err
	}
	manifest, _, err := decodeStored(row.ManifestJSON)
	if err != nil {
		return Record{}, VersionRecord{}, nil, err
	}
	return record, row, manifest, nil
}

type ApproveResult struct {
	ID      string
	Version string
	Digest  string
	Tools   []string
}

func validDigestPrefix(prefix string) bool {
	if len(prefix) < MinDigestPrefix || len(prefix) > DigestHexLength {
		return false
	}
	return !strings.ContainsFunc(prefix, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) })
}

func (s *Service) Approve(ctx context.Context, id, digestPrefix string) (*ApproveResult, error) {
	if !validDigestPrefix(digestPrefix) {
		return nil, Errorf(ErrInvalidArgument, "the digest must be at least %d lowercase hex characters", MinDigestPrefix)
	}
	record, row, manifest, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(row.Digest, digestPrefix) {
		return nil, Errorf(ErrDigestMismatch, "the digest does not match the installed version")
	}
	plugins, err := s.openPlugins()
	if err != nil {
		return nil, err
	}
	defer func() { _ = plugins.Close() }()
	if err := s.verify(plugins, &row); err != nil {
		return nil, err
	}
	_, bundleSHA256, err := decodeStored(row.ManifestJSON)
	if err != nil {
		return nil, err
	}
	surface, err := SurfaceOf(manifest, bundleSHA256)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	grants := surface.Grants()
	approval := ApprovalRecord{ID: record.ID, Version: record.Version, Digest: row.Digest, At: now}
	for _, grant := range grants {
		approval.Grants = append(approval.Grants, GrantRecord{ID: record.ID, GrantKey: grant.GrantKey, Fingerprint: grant.Fingerprint, ApprovedAt: now, ApprovedBy: ApprovedByOwner})
	}
	if err := s.registry.Approve(ctx, &approval); err != nil {
		return nil, err
	}
	result := &ApproveResult{ID: record.ID, Version: record.Version, Digest: row.Digest}
	for _, name := range sortedKeys(surface.Tools) {
		result.Tools = append(result.Tools, tools.PluginToolID(record.ID, name))
	}
	return result, nil
}

func (s *Service) Disable(ctx context.Context, id string) error {
	if ctx == nil || !config.ValidPluginID(id) {
		return Errorf(ErrInvalidArgument, "a valid plugin id is required")
	}
	return s.registry.SetState(ctx, id, StateDisabled, s.now().UTC())
}

func (s *Service) Login(ctx context.Context, id string, opts *auth.LoginOptions) error {
	if ctx == nil || !config.ValidPluginID(id) || opts == nil {
		return Errorf(ErrInvalidArgument, "a valid plugin id and login options are required")
	}
	record, err := s.registry.Get(ctx, id)
	if err != nil {
		return err
	}
	if record.State != StateActive {
		return Errorf(ErrSurfacePending, "the plugin must be approved and active before it can log in")
	}
	plugins, err := s.openPlugins()
	if err != nil {
		return err
	}
	defer func() { _ = plugins.Close() }()
	active, err := s.activate(ctx, plugins, &record)
	if err != nil {
		return err
	}
	interactive, ok := active.Auth.(auth.Interactive)
	if !ok {
		return Errorf(ErrInvalidArgument, "the plugin's auth needs no login")
	}
	return interactive.Login(ctx, opts)
}

func (s *Service) Logout(ctx context.Context, id string) error {
	if ctx == nil || !config.ValidPluginID(id) {
		return Errorf(ErrInvalidArgument, "a valid plugin id is required")
	}
	if _, err := s.registry.Get(ctx, id); err != nil {
		return err
	}
	if err := s.auth.Erase(id); err != nil {
		return Wrap(ErrStorage, err, "the plugin secrets cannot be erased")
	}
	return nil
}

type RemovePlan struct {
	ID              string
	State           string
	Version         string
	ApprovedVersion string
	Applied         bool
}

func (s *Service) Remove(ctx context.Context, id string, apply bool) (*RemovePlan, error) {
	if ctx == nil || !config.ValidPluginID(id) {
		return nil, Errorf(ErrInvalidArgument, "a valid plugin id is required")
	}
	record, err := s.registry.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	plan := &RemovePlan{ID: record.ID, State: record.State, Version: record.Version, ApprovedVersion: record.ApprovedVersion}
	if !apply {
		return plan, nil
	}
	if err := s.registry.Remove(ctx, id); err != nil {
		return nil, err
	}
	plugins, err := s.openPlugins()
	if err != nil {
		return nil, err
	}
	defer func() { _ = plugins.Close() }()
	if err := plugins.RemoveAll(id); err != nil {
		return nil, Wrap(ErrStorage, err, "the bundle cannot be removed")
	}
	if err := s.auth.Erase(id); err != nil {
		return nil, Wrap(ErrStorage, err, "the plugin secrets cannot be erased")
	}
	plan.Applied = true
	return plan, nil
}

type Summary struct {
	ID              string
	Name            string
	Version         string
	ApprovedVersion string
	State           string
	DigestPrefix    string
}

func (s *Service) List(ctx context.Context) ([]Summary, error) {
	if ctx == nil {
		return nil, Errorf(ErrInvalidArgument, "a context is required")
	}
	records, err := s.registry.List(ctx)
	if err != nil {
		return nil, err
	}
	summaries := make([]Summary, 0, len(records))
	for i := range records {
		record := &records[i]
		summary := Summary{ID: record.ID, Version: record.Version, ApprovedVersion: record.ApprovedVersion, State: record.State}
		row, err := s.registry.Version(ctx, record.ID, record.Version)
		if err == nil {
			summary.DigestPrefix = row.Digest[:min(MinDigestPrefix, len(row.Digest))]
			if manifest, _, decodeErr := decodeStored(row.ManifestJSON); decodeErr == nil {
				summary.Name = manifest.Name
			}
		} else if !isCode(err, ErrNotFound) {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}
