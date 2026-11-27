package operation

import (
	"slices"
	"strings"
)

func allSurfaces() []Surface {
	return []Surface{SurfaceCLI, SurfaceChat, SurfaceAgent}
}

func (s Surface) valid() bool {
	return slices.Contains(allSurfaces(), s)
}

func (e Exposure) valid() bool {
	switch e {
	case ExposureAgent, ExposureOwner, ExposureHost:
		return true
	}
	return false
}

func (r Risk) valid() bool {
	switch r {
	case RiskRead, RiskChange, RiskTrustGrant, RiskDestructive:
		return true
	}
	return false
}

func (c CommitClass) valid() bool {
	switch c {
	case CommitTransactional, CommitExternalIdempotent, CommitExternal, CommitNone:
		return true
	}
	return false
}

func (k ParamKind) valid() bool {
	switch k {
	case KindString, KindInteger, KindBoolean, KindDuration, KindEnum, KindStrings, KindJSON, KindPath:
		return true
	}
	return false
}

func (k OutputKind) valid() bool {
	switch k {
	case OutputRecord, OutputTable, OutputText:
		return true
	}
	return false
}

func (r Risk) effectful() bool {
	return r != RiskRead
}

func exposureSurfaces(exposure Exposure) []Surface {
	switch exposure {
	case ExposureAgent:
		return allSurfaces()
	case ExposureOwner:
		return []Surface{SurfaceCLI, SurfaceChat}
	case ExposureHost:
		return []Surface{SurfaceCLI}
	}
	return nil
}

func onSurface(surfaces []Surface, surface Surface) bool {
	return len(surfaces) == 0 || slices.Contains(surfaces, surface)
}

func (d *Descriptor) Projects(surface Surface) bool {
	if d == nil || !slices.Contains(exposureSurfaces(d.Exposure), surface) {
		return false
	}
	return onSurface(d.Surfaces, surface)
}

func (d *Descriptor) GeneratesTool() bool {
	return d != nil && d.Exposure == ExposureAgent && d.ModuleTool == "" && d.Projects(SurfaceAgent)
}

func (p *Param) on(surface Surface) bool {
	if p.Kind == KindPath && surface == SurfaceChat {
		return false
	}
	return onSurface(p.Surfaces, surface)
}

func (e *EnumValue) on(surface Surface) bool {
	return onSurface(e.Surfaces, surface)
}

func (f *OutputField) on(surface Surface) bool {
	return onSurface(f.Surfaces, surface)
}

func (p *Param) surfaceList() []Surface {
	var surfaces []Surface
	for _, surface := range allSurfaces() {
		if p.on(surface) {
			surfaces = append(surfaces, surface)
		}
	}
	return surfaces
}

func (p *Param) enumOn(surface Surface) []string {
	var values []string
	for i := range p.Enum {
		if p.Enum[i].on(surface) {
			values = append(values, p.Enum[i].Value)
		}
	}
	return values
}

func (d *Descriptor) paramsOn(surface Surface) []*Param {
	var params []*Param
	for i := range d.Params {
		if d.Params[i].on(surface) {
			params = append(params, &d.Params[i])
		}
	}
	return params
}

func (d *Descriptor) OutputFieldsOn(surface Surface) []string {
	var names []string
	for i := range d.Output.Fields {
		if d.Output.Fields[i].on(surface) {
			names = append(names, d.Output.Fields[i].Name)
		}
	}
	return names
}

func ToolName(id string) string {
	replacer := strings.NewReplacer(".", "_", "-", "_")
	return toolPrefix + replacer.Replace(id)
}

const toolPrefix = "aura_"
