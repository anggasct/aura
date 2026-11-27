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
)

type Deps struct {
	DB        *sql.DB
	Effects   *effect.Executor
	Tools     ToolLookup
	Clock     durable.Clock
	Logger    *slog.Logger
	Telemetry *telemetry.OperationRecorder
}

type entry struct {
	descriptor *Descriptor
	binding    Binding
}

type Registry struct {
	entries       map[string]*entry
	byTool        map[string]*entry
	ids           []string
	groups        []Group
	registrations []toolbroker.ToolRegistration
	receipts      receiptStore
	effects       *effect.Executor
	clock         durable.Clock
	logger        *slog.Logger
	recorder      *telemetry.OperationRecorder
}

func NewRegistry(catalogs []Catalog, bindings []Binding, deps *Deps) (*Registry, error) {
	if deps == nil || deps.DB == nil || deps.Effects == nil || deps.Tools == nil {
		return nil, NewError(ErrorCodeInvalidArgument, "operation: the registry needs a database, an effect executor and the tool lookup")
	}
	bound := make(map[string]*Binding, len(bindings))
	v := &validation{tools: deps.Tools, checked: true, bindings: bound}
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
		entries:  map[string]*entry{},
		byTool:   map[string]*entry{},
		receipts: receiptStore{db: deps.DB},
		effects:  deps.Effects,
		clock:    deps.Clock,
		logger:   deps.Logger,
		recorder: deps.Telemetry,
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
	copied.Output.Fields = slices.Clone(d.Output.Fields)
	for i := range copied.Output.Fields {
		copied.Output.Fields[i].Surfaces = slices.Clone(copied.Output.Fields[i].Surfaces)
	}
	copied.Params = slices.Clone(d.Params)
	for i := range copied.Params {
		param := &copied.Params[i]
		param.Surfaces = slices.Clone(param.Surfaces)
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
