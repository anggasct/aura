package operation

import (
	"encoding/json"
	"slices"
	"strconv"
)

type Flag struct {
	Name     string
	Param    string
	Kind     ParamKind
	Summary  string
	Required bool
	Position int
	Rest     bool
	Repeated bool
	Values   []string
	Default  json.RawMessage
}

func (d *Descriptor) Flags(surface Surface) []Flag {
	var flags []Flag
	for _, param := range d.paramsOn(surface) {
		if param.Position > 0 {
			continue
		}
		flags = append(flags, flagOf(param, surface))
	}
	return flags
}

func (d *Descriptor) Positionals(surface Surface) []Flag {
	var positionals []Flag
	for _, param := range d.paramsOn(surface) {
		if param.Position > 0 {
			positionals = append(positionals, flagOf(param, surface))
		}
	}
	slices.SortFunc(positionals, func(a, b Flag) int { return a.Position - b.Position })
	return positionals
}

func flagOf(param *Param, surface Surface) Flag {
	flag := Flag{
		Name:     kebab(param.Name),
		Param:    param.Name,
		Kind:     param.Kind,
		Summary:  param.SummaryOn(surface),
		Required: param.Required,
		Position: param.Position,
		Rest:     param.Rest,
		Repeated: param.Kind == KindStrings,
		Default:  slices.Clone(param.Default),
	}
	if param.Kind == KindEnum {
		flag.Values = param.enumOn(surface)
	}
	return flag
}

type Input struct {
	descriptor *Descriptor
	surface    Surface
	values     map[string][]string
}

func NewInput(descriptor *Descriptor, surface Surface) (*Input, error) {
	if descriptor == nil {
		return nil, nilArgument("descriptor")
	}
	if !surface.valid() {
		return nil, NewError(ErrorCodeInvalidArgument, "operation: surface is unknown")
	}
	return &Input{descriptor: descriptor, surface: surface, values: map[string][]string{}}, nil
}

func (in *Input) Set(param string, values ...string) error {
	if in == nil {
		return nilArgument("input")
	}
	found := slices.ContainsFunc(in.descriptor.paramsOn(in.surface), func(candidate *Param) bool { return candidate.Name == param })
	if !found {
		return NewError(ErrorCodeInputInvalid, "parameter "+param+" does not exist on this surface")
	}
	in.values[param] = append(in.values[param], values...)
	return nil
}

func (in *Input) Canonical() (json.RawMessage, error) {
	if in == nil {
		return nil, nilArgument("input")
	}
	writer := newObjectWriter()
	for _, param := range in.descriptor.paramsOn(in.surface) {
		raw, set := in.values[param.Name]
		if !set {
			continue
		}
		encoded, err := param.fromText(raw)
		if err != nil {
			return nil, err
		}
		if err := writer.put(param.Name, encoded); err != nil {
			return nil, err
		}
	}
	return in.descriptor.Validate(in.surface, writer.bytes())
}

func (p *Param) fromText(values []string) ([]byte, error) {
	if p.Kind == KindStrings {
		return encodeJSON(values)
	}
	if len(values) != 1 {
		return nil, p.refusal("takes exactly one value")
	}
	text := values[0]
	switch p.Kind {
	case KindInteger:
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, p.refusal("must be an integer")
		}
		return []byte(strconv.FormatInt(parsed, 10)), nil
	case KindBoolean:
		parsed, err := strconv.ParseBool(text)
		if err != nil {
			return nil, p.refusal("must be true or false")
		}
		return encodeJSON(parsed)
	case KindJSON:
		if !json.Valid([]byte(text)) {
			return nil, p.refusal("must be JSON text")
		}
		return []byte(text), nil
	case KindString, KindDuration, KindEnum, KindStrings, KindPath:
		return encodeJSON(text)
	}
	return encodeJSON(text)
}
