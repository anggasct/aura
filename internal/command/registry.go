package command

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/principal"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

type Registry struct {
	defs     []Definition
	byName   map[string]int
	bindings map[string]*bound
}

type bound struct {
	binding Binding
	names   map[string]bool
}

func New(bindings ...Binding) (*Registry, error) {
	return NewRegistry(Catalog(), bindings...)
}

func NewRegistry(defs []Definition, bindings ...Binding) (*Registry, error) {
	r := &Registry{
		defs:     make([]Definition, 0, len(defs)),
		byName:   make(map[string]int, len(defs)),
		bindings: make(map[string]*bound, len(bindings)),
	}
	for i := range defs {
		if err := r.add(&defs[i]); err != nil {
			return nil, err
		}
	}
	for i := range bindings {
		if err := r.bind(&bindings[i]); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) add(def *Definition) error {
	if err := validateDefinition(def); err != nil {
		return err
	}
	index := len(r.defs)
	for _, name := range append([]string{def.Name}, def.Aliases...) {
		if _, taken := r.byName[name]; taken {
			return fmt.Errorf("command: name %q is defined more than once", name)
		}
		r.byName[name] = index
	}
	copied := *def
	copied.Aliases = append([]string(nil), def.Aliases...)
	r.defs = append(r.defs, copied)
	return nil
}

func validateDefinition(def *Definition) error {
	if !namePattern.MatchString(def.Name) {
		return fmt.Errorf("command: name %q is not valid", def.Name)
	}
	for _, alias := range def.Aliases {
		if !namePattern.MatchString(alias) {
			return fmt.Errorf("command: alias %q of %q is not valid", alias, def.Name)
		}
	}
	switch def.Audience {
	case AudienceOwner, AudienceAny:
	default:
		return fmt.Errorf("command: %q has unknown audience %q", def.Name, def.Audience)
	}
	switch def.Kind {
	case KindLocal, KindTurn:
		if def.Control != ControlNone {
			return fmt.Errorf("command: %q is not terminal-only and must not carry a control", def.Name)
		}
	case KindTerminalOnly:
		if def.Control == ControlNone {
			return fmt.Errorf("command: terminal-only %q needs a control", def.Name)
		}
	default:
		return fmt.Errorf("command: %q has unknown kind %q", def.Name, def.Kind)
	}
	if def.MinArgs < 0 || (def.MaxArgs != Variadic && def.MaxArgs < def.MinArgs) {
		return fmt.Errorf("command: %q has an invalid arity", def.Name)
	}
	if def.Prompt != "" && def.MinArgs == 0 {
		return fmt.Errorf("command: %q prompts but takes no required argument", def.Name)
	}
	if strings.TrimSpace(def.Summary) == "" {
		return fmt.Errorf("command: %q needs a summary", def.Name)
	}
	return nil
}

func (r *Registry) bind(binding *Binding) error {
	if strings.TrimSpace(binding.Channel) == "" {
		return errors.New("command: binding channel must not be empty")
	}
	if _, dup := r.bindings[binding.Channel]; dup {
		return fmt.Errorf("command: channel %q is bound more than once", binding.Channel)
	}
	switch binding.Style {
	case StyleTerminal, StyleMessenger:
	default:
		return fmt.Errorf("command: channel %q has unknown style %q", binding.Channel, binding.Style)
	}
	names := make(map[string]bool, len(binding.Names))
	for _, name := range binding.Names {
		index, ok := r.byName[name]
		if !ok || r.defs[index].Name != name {
			return fmt.Errorf("command: channel %q binds unknown command %q", binding.Channel, name)
		}
		if names[name] {
			return fmt.Errorf("command: channel %q binds %q more than once", binding.Channel, name)
		}
		if r.defs[index].Kind == KindTerminalOnly && binding.Style != StyleTerminal {
			return fmt.Errorf("command: channel %q cannot bind terminal-only %q", binding.Channel, name)
		}
		names[name] = true
	}
	copied := *binding
	copied.Names = append([]string(nil), binding.Names...)
	copied.Notes = append([]Note(nil), binding.Notes...)
	r.bindings[binding.Channel] = &bound{binding: copied, names: names}
	return nil
}

func (r *Registry) Lookup(name string) (Definition, bool) {
	index, ok := r.byName[name]
	if !ok {
		return Definition{}, false
	}
	return r.defs[index], true
}

func (r *Registry) Bound(channel, name string) bool {
	b, ok := r.bindings[channel]
	if !ok {
		return false
	}
	def, ok := r.Lookup(name)
	return ok && b.names[def.Name]
}

func (r *Registry) Recognized(channel, name string) bool {
	if r.Bound(channel, name) {
		return true
	}
	def, ok := r.Lookup(name)
	return ok && def.Kind == KindTerminalOnly
}

func (r *Registry) MetricName(channel, name string) string {
	def, ok := r.Lookup(name)
	if !ok {
		return ""
	}
	if def.Kind == KindTerminalOnly {
		if b, bound := r.bindings[channel]; !bound || b.binding.Style != StyleTerminal {
			return NameTerminalOnly
		}
	}
	return def.Name
}

func (r *Registry) Help(channel string, class principal.Class) []string {
	b, ok := r.bindings[channel]
	if !ok {
		return []string{"commands:"}
	}
	var rows []Note
	for i := range r.defs {
		def := &r.defs[i]
		if !b.names[def.Name] || !permits(def.Audience, class) {
			continue
		}
		rows = append(rows, Note{Syntax: syntax(def), Summary: def.Summary})
	}
	rows = append(rows, b.binding.Notes...)
	width := 0
	for _, row := range rows {
		width = max(width, utf8.RuneCountInString(row.Syntax))
	}
	lines := make([]string, 0, len(rows)+1)
	lines = append(lines, "commands:")
	for _, row := range rows {
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(row.Syntax))
		lines = append(lines, "  "+row.Syntax+pad+"  "+row.Summary)
	}
	return lines
}

func syntax(def *Definition) string {
	names := make([]string, 0, len(def.Aliases)+1)
	for _, name := range append([]string{def.Name}, def.Aliases...) {
		names = append(names, "/"+name)
	}
	head := strings.Join(names, ", ")
	if def.Usage != "" {
		head += " " + def.Usage
	}
	return head
}

func permits(audience Audience, class principal.Class) bool {
	return audience == AudienceAny || class == principal.ClassOwner
}

func (r *Registry) binding(channel string) (*bound, bool) {
	b, ok := r.bindings[channel]
	return b, ok
}
