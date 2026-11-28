package operation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	AnnotationOperation = "aura.operation"
	rootGroup           = "aura"
	flagJSON            = "json"
	flagApply           = "apply"
)

type Invoker interface {
	Invoke(ctx context.Context, id string, call *Call) (Result, error)
	Preview(ctx context.Context, id string, call *Call) (Preview, error)
}

type Opener func(cmd *cobra.Command, descriptor *Descriptor, run func(ctx context.Context, invoker Invoker) error) error

type commandTree struct {
	open   Opener
	groups map[string]*cobra.Command
	leaves map[string]string
	roots  []*cobra.Command
}

func Commands(catalogs []Catalog, open Opener) ([]*cobra.Command, error) {
	if open == nil {
		return nil, nilArgument("opener")
	}
	if err := ValidateCatalogs(catalogs); err != nil {
		return nil, err
	}
	tree := &commandTree{open: open, groups: map[string]*cobra.Command{}, leaves: map[string]string{}}
	var defects []error
	for i := range catalogs {
		summary := catalogs[i].Group.Summary
		for j := range catalogs[i].Descriptors {
			descriptor := catalogs[i].Descriptors[j].clone()
			if !descriptor.Projects(SurfaceCLI) {
				continue
			}
			if err := tree.add(&descriptor, summary); err != nil {
				defects = append(defects, err)
			}
		}
	}
	if err := errors.Join(defects...); err != nil {
		return nil, err
	}
	return tree.roots, nil
}

func commandPath(id string) []string {
	segments := strings.Split(id, ".")
	if segments[0] == rootGroup {
		return segments[1:]
	}
	return segments
}

func (t *commandTree) add(descriptor *Descriptor, groupSummary string) error {
	path := commandPath(descriptor.ID)
	if len(path) == 0 {
		return NewError(ErrorCodeInvalid, descriptor.ID+": the id names no command")
	}
	for depth := 1; depth < len(path); depth++ {
		key := strings.Join(path[:depth], "/")
		if owner, leaf := t.leaves[key]; leaf {
			return NewError(ErrorCodeInvalid, fmt.Sprintf("%s: %s is a command and cannot also be a command group", descriptor.ID, owner))
		}
	}
	key := strings.Join(path, "/")
	if _, group := t.groups[key]; group {
		return NewError(ErrorCodeInvalid, descriptor.ID+": the id is a command group of another operation")
	}
	t.leaves[key] = descriptor.ID
	parent := t.group(path[:len(path)-1], groupSummary)
	command := t.leaf(descriptor, path[len(path)-1])
	if parent == nil {
		t.roots = append(t.roots, command)
		return nil
	}
	parent.AddCommand(command)
	return nil
}

func (t *commandTree) group(path []string, summary string) *cobra.Command {
	var parent *cobra.Command
	for depth := range path {
		key := strings.Join(path[:depth+1], "/")
		found, ok := t.groups[key]
		if !ok {
			short := path[depth] + " commands"
			if depth == 0 {
				short = summary
			}
			found = &cobra.Command{Use: path[depth], Short: short}
			t.groups[key] = found
			if parent == nil {
				t.roots = append(t.roots, found)
			} else {
				parent.AddCommand(found)
			}
		}
		parent = found
	}
	return parent
}

type leafCommand struct {
	descriptor  *Descriptor
	open        Opener
	flags       []Flag
	positionals []Flag
	previewing  bool
}

func (t *commandTree) leaf(descriptor *Descriptor, name string) *cobra.Command {
	leaf := &leafCommand{
		descriptor:  descriptor,
		open:        t.open,
		flags:       descriptor.Flags(SurfaceCLI),
		positionals: descriptor.Positionals(SurfaceCLI),
		previewing:  descriptor.previews(),
	}
	command := &cobra.Command{
		Use:         leaf.usage(name),
		Short:       descriptor.Summary,
		Long:        strings.TrimSpace(descriptor.Summary + "\n\n" + descriptor.DescriptionOn(SurfaceCLI)),
		Args:        leaf.arguments,
		RunE:        leaf.execute,
		Annotations: map[string]string{AnnotationOperation: descriptor.ID},
	}
	for i := range leaf.flags {
		leaf.declare(command, &leaf.flags[i])
	}
	command.Flags().Bool(flagJSON, false, "print the result as JSON")
	if leaf.previewing {
		command.Flags().Bool(flagApply, false, "apply the change; without it only the preview is printed")
	}
	return command
}

func (l *leafCommand) usage(name string) string {
	parts := make([]string, 0, 1+len(l.positionals))
	parts = append(parts, name)
	for i := range l.positionals {
		label := "<" + l.positionals[i].Name + ">"
		if l.positionals[i].Rest {
			label = "<" + l.positionals[i].Name + "...>"
		}
		if !l.positionals[i].Required {
			label = "[" + label + "]"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, " ")
}

func (l *leafCommand) declare(command *cobra.Command, flag *Flag) {
	summary := flag.Summary
	if len(flag.Values) > 0 {
		summary += " (" + strings.Join(flag.Values, ", ") + ")"
	}
	if len(flag.Default) > 0 {
		summary += " (default " + string(flag.Default) + ")"
	}
	switch {
	case flag.Kind == KindBoolean:
		command.Flags().Bool(flag.Name, false, summary)
	case flag.Repeated:
		command.Flags().StringArray(flag.Name, nil, summary)
	case flag.Kind == KindDuration:
		command.Flags().Var(&typedText{kind: "duration"}, flag.Name, summary)
	case flag.Kind == KindInteger:
		command.Flags().Var(&typedText{kind: "int"}, flag.Name, summary)
	default:
		command.Flags().String(flag.Name, "", summary)
	}
}

type typedText struct {
	kind string
	text string
}

func (t *typedText) String() string { return t.text }

func (t *typedText) Set(text string) error {
	t.text = text
	return nil
}

func (t *typedText) Type() string { return t.kind }

func (l *leafCommand) arguments(cmd *cobra.Command, args []string) error {
	required := 0
	rest := false
	for i := range l.positionals {
		if l.positionals[i].Required {
			required++
		}
		rest = rest || l.positionals[i].Rest
	}
	switch {
	case len(args) < required:
		return clikit.NewUsageError(fmt.Errorf("%s needs %d argument(s), got %d", cmd.CommandPath(), required, len(args)))
	case !rest && len(args) > len(l.positionals):
		return clikit.NewUsageError(fmt.Errorf("%s: unexpected argument %q", cmd.CommandPath(), args[len(l.positionals)]))
	}
	return nil
}

func (l *leafCommand) input(cmd *cobra.Command, args []string) ([]byte, error) {
	in, err := NewInput(l.descriptor, SurfaceCLI)
	if err != nil {
		return nil, err
	}
	for i := range l.positionals {
		positional := &l.positionals[i]
		if positional.Rest && i < len(args) {
			if err := in.Set(positional.Param, strings.Join(args[i:], " ")); err != nil {
				return nil, clikit.NewUsageError(err)
			}
			break
		}
		if i < len(args) {
			if err := in.Set(positional.Param, args[i]); err != nil {
				return nil, clikit.NewUsageError(err)
			}
		}
	}
	for i := range l.flags {
		if err := l.setFlag(cmd, &l.flags[i], in); err != nil {
			return nil, clikit.NewUsageError(err)
		}
	}
	canonical, err := in.Canonical()
	if err != nil {
		return nil, usageOr(err)
	}
	return canonical, nil
}

func (l *leafCommand) setFlag(cmd *cobra.Command, flag *Flag, in *Input) error {
	if !cmd.Flags().Changed(flag.Name) {
		return nil
	}
	switch {
	case flag.Kind == KindBoolean:
		value, err := cmd.Flags().GetBool(flag.Name)
		if err != nil {
			return err
		}
		return in.Set(flag.Param, strconv.FormatBool(value))
	case flag.Repeated:
		values, err := cmd.Flags().GetStringArray(flag.Name)
		if err != nil {
			return err
		}
		return in.Set(flag.Param, values...)
	}
	if flag.Kind == KindDuration || flag.Kind == KindInteger {
		return in.Set(flag.Param, cmd.Flags().Lookup(flag.Name).Value.String())
	}
	value, err := cmd.Flags().GetString(flag.Name)
	if err != nil {
		return err
	}
	return in.Set(flag.Param, value)
}

func usageOr(err error) error {
	if fault.HasCode(err, ErrorCodeInputInvalid) {
		return clikit.NewUsageError(err)
	}
	return err
}

type commandRun struct {
	cmd       *cobra.Command
	invoker   Invoker
	canonical []byte
	asJSON    bool
	apply     bool
}

func (l *leafCommand) execute(cmd *cobra.Command, args []string) error {
	canonical, err := l.input(cmd, args)
	if err != nil {
		return err
	}
	asJSON, err := cmd.Flags().GetBool(flagJSON)
	if err != nil {
		return err
	}
	apply := false
	if l.previewing {
		if apply, err = cmd.Flags().GetBool(flagApply); err != nil {
			return err
		}
	}
	return l.open(cmd, l.descriptor, func(ctx context.Context, invoker Invoker) error {
		return l.invoke(ctx, &commandRun{cmd: cmd, invoker: invoker, canonical: canonical, asJSON: asJSON, apply: apply})
	})
}

func (l *leafCommand) invoke(ctx context.Context, run *commandRun) error {
	call := &Call{
		Caller: Caller{Surface: SurfaceCLI, PrincipalID: principal.OwnerID, Class: principal.ClassOwner},
		Input:  slices.Clone(run.canonical),
		Terminal: &Terminal{
			In:  run.cmd.InOrStdin(),
			Out: run.cmd.OutOrStdout(),
		},
	}
	if l.previewing {
		preview, err := run.invoker.Preview(ctx, l.descriptor.ID, call)
		if err != nil {
			return usageOr(err)
		}
		if !run.apply || preview.Empty {
			return renderPreview(run.cmd.OutOrStdout(), &preview, run.asJSON)
		}
		call.PreviewDigest = preview.Digest
	}
	result, err := run.invoker.Invoke(ctx, l.descriptor.ID, call)
	if err != nil {
		return usageOr(err)
	}
	if run.asJSON {
		return writeIndented(run.cmd.OutOrStdout(), result.Data)
	}
	return l.descriptor.renderOn(SurfaceCLI, run.cmd.OutOrStdout(), result.Data)
}
