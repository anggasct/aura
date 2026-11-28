package operation

import (
	"database/sql"
	"errors"
	"log/slog"
	"maps"
	"slices"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

type Deps struct {
	DB        *sql.DB
	Effects   *effect.Executor
	Tools     ToolLookup
	Clock     durable.Clock
	Logger    *slog.Logger
	Telemetry *telemetry.OperationRecorder
	Files     *FileDeps
	Confirmer *Confirmer
}

type entry struct {
	descriptor *Descriptor
	binding    Binding
	definition *tools.Definition
}

type Registry struct {
	entries       map[string]*entry
	byTool        map[string]*entry
	ids           []string
	groups        []Group
	members       map[string][]string
	registrations []toolbroker.ToolRegistration
	receipts      receiptStore
	effects       *effect.Executor
	clock         durable.Clock
	logger        *slog.Logger
	recorder      *telemetry.OperationRecorder
	files         *FileDeps
	confirmer     *Confirmer
}

func NewRegistry(catalogs []Catalog, bindings []Binding, deps *Deps) (*Registry, error) {
	if deps == nil || deps.DB == nil || deps.Effects == nil || deps.Tools == nil {
		return nil, NewError(ErrorCodeInvalidArgument, "operation: the registry needs a database, an effect executor and the tool lookup")
	}
	if err := deps.Files.validate(); err != nil {
		return nil, err
	}
	bound := make(map[string]*Binding, len(bindings))
	v := &validation{tools: deps.Tools, checked: true, bindings: bound, confirmer: deps.Confirmer != nil}
	for i := range bindings {
		binding := &bindings[i]
		switch {
		case bound[binding.ID] != nil:
			v.reject(binding.ID, "the id has more than one binding")
		case binding.Handle == nil:
			v.reject(binding.ID, "the binding has no handler")
		default:
			bound[binding.ID] = binding
		}
	}
	v.catalogs(catalogs)
	if err := errors.Join(v.defects...); err != nil {
		return nil, err
	}
	registry := &Registry{
		entries:   map[string]*entry{},
		byTool:    map[string]*entry{},
		members:   map[string][]string{},
		receipts:  receiptStore{db: deps.DB},
		effects:   deps.Effects,
		clock:     deps.Clock,
		logger:    deps.Logger,
		recorder:  deps.Telemetry,
		files:     deps.Files,
		confirmer: deps.Confirmer,
	}
	if registry.clock == nil {
		registry.clock = durable.RealClock()
	}
	if registry.logger == nil {
		registry.logger = slog.Default()
	}
	for i := range catalogs {
		registry.groups = append(registry.groups, catalogs[i].Group)
		for j := range catalogs[i].Descriptors {
			descriptor := catalogs[i].Descriptors[j].clone()
			registry.entries[descriptor.ID] = &entry{descriptor: &descriptor, binding: *bound[descriptor.ID]}
			registry.members[catalogs[i].Group.Name] = append(registry.members[catalogs[i].Group.Name], descriptor.ID)
		}
	}
	registry.ids = slices.Sorted(maps.Keys(registry.entries))
	slices.SortFunc(registry.groups, func(a, b Group) int { return compareStrings(a.Name, b.Name) })
	if err := registry.buildTools(); err != nil {
		return nil, err
	}
	return registry, nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (d *Descriptor) clone() Descriptor {
	copied := *d
	copied.Surfaces = slices.Clone(d.Surfaces)
	copied.Display = slices.Clone(d.Display)
	copied.RequiredCapabilities = slices.Clone(d.RequiredCapabilities)
	copied.Needs = slices.Clone(d.Needs)
	copied.Texts = slices.Clone(d.Texts)
	copied.Output.Fields = slices.Clone(d.Output.Fields)
	for i := range copied.Output.Fields {
		copied.Output.Fields[i].Surfaces = slices.Clone(copied.Output.Fields[i].Surfaces)
	}
	copied.Output.Header = slices.Clone(d.Output.Header)
	for i := range copied.Output.Header {
		copied.Output.Header[i].Surfaces = slices.Clone(copied.Output.Header[i].Surfaces)
	}
	copied.Params = slices.Clone(d.Params)
	for i := range copied.Params {
		param := &copied.Params[i]
		param.Surfaces = slices.Clone(param.Surfaces)
		param.Texts = slices.Clone(param.Texts)
		param.Bounds = slices.Clone(param.Bounds)
		param.Default = slices.Clone(param.Default)
		param.Schema = slices.Clone(param.Schema)
		param.Enum = slices.Clone(param.Enum)
		for j := range param.Enum {
			param.Enum[j].Surfaces = slices.Clone(param.Enum[j].Surfaces)
		}
		if param.Minimum != nil {
			minimum := *param.Minimum
			param.Minimum = &minimum
		}
		if param.Maximum != nil {
			maximum := *param.Maximum
			param.Maximum = &maximum
		}
	}
	return copied
}

func (r *Registry) Descriptor(id string) (*Descriptor, bool) {
	found, ok := r.entries[id]
	if !ok {
		return nil, false
	}
	copied := found.descriptor.clone()
	return &copied, true
}

func (r *Registry) Descriptors() []*Descriptor {
	descriptors := make([]*Descriptor, 0, len(r.ids))
	for _, id := range r.ids {
		copied := r.entries[id].descriptor.clone()
		descriptors = append(descriptors, &copied)
	}
	return descriptors
}

func (r *Registry) Groups() []Group {
	return slices.Clone(r.groups)
}

func (r *Registry) Tools() []toolbroker.ToolRegistration {
	return slices.Clone(r.registrations)
}
