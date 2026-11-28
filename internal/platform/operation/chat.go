package operation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	flagPrefix    = "--"
	chatNoChanges = "nothing to do"
	chatDone      = "done"
)

func ChatGroups(catalogs []Catalog) []command.Definition {
	var definitions []command.Definition
	for i := range catalogs {
		catalog := &catalogs[i]
		projected := slices.ContainsFunc(catalog.Descriptors, func(d Descriptor) bool { return d.Projects(SurfaceChat) })
		if !projected {
			continue
		}
		definitions = append(definitions, command.Definition{
			Name:     catalog.Group.Name,
			Usage:    "<verb> [arguments]",
			Summary:  catalog.Group.Summary,
			Audience: command.AudienceOwner,
			Kind:     command.KindOperation,
			MinArgs:  1,
			MaxArgs:  command.Variadic,
		})
	}
	return definitions
}

type ChatDeps struct {
	Registry     *Registry
	Capabilities []string
}

type Chat struct {
	registry     *Registry
	capabilities []string
}

func NewChat(deps *ChatDeps) (*Chat, error) {
	if deps == nil || deps.Registry == nil {
		return nil, NewError(ErrorCodeInvalidArgument, "operation: the chat projection needs a registry")
	}
	return &Chat{registry: deps.Registry, capabilities: slices.Clone(deps.Capabilities)}, nil
}

func capabilitiesMet(descriptor *Descriptor, active []string) bool {
	for _, required := range descriptor.RequiredCapabilities {
		if !slices.Contains(active, required) {
			return false
		}
	}
	return true
}

func (c *Chat) members(group string) []*entry {
	var found []*entry
	for _, id := range c.registry.members[group] {
		entry := c.registry.entries[id]
		if capabilitiesMet(entry.descriptor, c.capabilities) {
			found = append(found, entry)
		}
	}
	return found
}

func verbPath(group, id string) []string {
	return strings.Split(strings.TrimPrefix(id, group+"."), ".")
}

func matchVerb(group string, members []*entry, tokens []string) (found *entry, rest []string) {
	longest := 0
	for _, candidate := range members {
		path := verbPath(group, candidate.descriptor.ID)
		if len(path) > len(tokens) || len(path) <= longest || !slices.Equal(path, tokens[:len(path)]) {
			continue
		}
		found, rest, longest = candidate, tokens[len(path):], len(path)
	}
	return found, rest
}

func chatRefusal(code command.ErrorCode, lines ...string) command.OperationPlan {
	return command.OperationPlan{Refusal: &command.Result{Lines: lines, Diagnostic: true, Code: code}}
}

func (c *Chat) verbList(group string, members []*entry) string {
	var verbs []string
	for _, candidate := range members {
		if candidate.descriptor.Projects(SurfaceChat) {
			verbs = append(verbs, strings.Join(verbPath(group, candidate.descriptor.ID), " "))
		}
	}
	slices.Sort(verbs)
	return "usage: /" + group + " <" + strings.Join(verbs, "|") + ">"
}

func chatUsage(descriptor *Descriptor) string {
	return "usage: " + synopsis(descriptor, SurfaceChat)
}

func (c *Chat) Plan(group string, tokens []string) command.OperationPlan {
	members := c.members(group)
	if len(members) == 0 {
		return chatRefusal(command.ErrorCodeUnavailable, "operation: this group is not available")
	}
	found, rest := matchVerb(group, members, tokens)
	if found == nil {
		return chatRefusal(command.ErrorCodeUsage, c.verbList(group, members))
	}
	descriptor := found.descriptor
	if !descriptor.Projects(SurfaceChat) {
		return chatRefusal(command.ErrorCodeUnavailable, notExposedText(descriptor))
	}
	parsed, err := parseChatArguments(descriptor, rest)
	if err != nil {
		return chatRefusal(command.ErrorCodeUsage, safeText(err), chatUsage(descriptor))
	}
	if missing, ok := parsed.promptFor(); ok {
		return command.OperationPlan{Missing: &command.OperationPrompt{Text: missing.Prompt, Flag: missing.flag(), Usage: chatUsage(descriptor)}}
	}
	canonical, err := parsed.in.Canonical()
	if err != nil {
		return chatRefusal(command.ErrorCodeUsage, safeText(err), chatUsage(descriptor))
	}
	return command.OperationPlan{Operation: descriptor.ID, Input: canonical, Apply: parsed.apply}
}

func notExposedText(descriptor *Descriptor) string {
	text := string(ErrorCodeNotExposed)
	if descriptor.Projects(SurfaceCLI) {
		text += ": run it on the host with " + synopsis(descriptor, SurfaceCLI)
	}
	return text
}

type chatArguments struct {
	descriptor *Descriptor
	in         *Input
	apply      bool
}

func (a *chatArguments) promptFor() (*Param, bool) {
	var missing []*Param
	for _, param := range a.descriptor.paramsOn(SurfaceChat) {
		if _, set := a.in.values[param.Name]; param.Required && len(param.Default) == 0 && !set {
			missing = append(missing, param)
		}
	}
	if len(missing) != 1 || missing[0].Prompt == "" {
		return nil, false
	}
	return missing[0], true
}

func (p *Param) flag() string {
	if p.Position > 0 {
		return ""
	}
	return flagPrefix + kebab(p.Name)
}

func parseChatArguments(descriptor *Descriptor, tokens []string) (*chatArguments, error) {
	in, err := NewInput(descriptor, SurfaceChat)
	if err != nil {
		return nil, err
	}
	parsed := &chatArguments{descriptor: descriptor, in: in}
	flags := descriptor.Flags(SurfaceChat)
	var free []string
	literal := false
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		switch {
		case literal || !strings.HasPrefix(token, flagPrefix):
			free = append(free, token)
		case token == flagPrefix:
			literal = true
		default:
			consumed, err := parsed.flag(flags, tokens[i:])
			if err != nil {
				return nil, err
			}
			i += consumed
		}
	}
	if err := parsed.positionals(free); err != nil {
		return nil, err
	}
	return parsed, nil
}

func (a *chatArguments) flag(flags []Flag, tokens []string) (consumed int, err error) {
	name, value, attached := strings.Cut(strings.TrimPrefix(tokens[0], flagPrefix), "=")
	if name == flagApply && a.descriptor.previews() {
		if !attached {
			a.apply = true
			return 0, nil
		}
		if value != "true" && value != "false" {
			return 0, NewError(ErrorCodeInputInvalid, "the apply flag takes true or false")
		}
		a.apply = value == "true"
		return 0, nil
	}
	index := slices.IndexFunc(flags, func(candidate Flag) bool { return candidate.Name == name })
	if index < 0 {
		return 0, NewError(ErrorCodeInputInvalid, "the command has a flag this operation does not have")
	}
	flag := &flags[index]
	switch {
	case attached:
	case flag.Kind == KindBoolean:
		value = "true"
	case len(tokens) < 2:
		return 0, NewError(ErrorCodeInputInvalid, "flag --"+flag.Name+" needs a value")
	default:
		value, consumed = tokens[1], 1
	}
	return consumed, a.in.Set(flag.Param, value)
}

func (a *chatArguments) positionals(free []string) error {
	positionals := a.descriptor.Positionals(SurfaceChat)
	for i := range positionals {
		if i >= len(free) {
			return nil
		}
		if positionals[i].Rest {
			return a.in.Set(positionals[i].Param, strings.Join(free[i:], " "))
		}
		if err := a.in.Set(positionals[i].Param, free[i]); err != nil {
			return err
		}
	}
	if len(free) > len(positionals) {
		return NewError(ErrorCodeInputInvalid, "the command has more arguments than this operation takes")
	}
	return nil
}

func chatCaller(caller *command.Caller) Caller {
	return Caller{
		Surface:     SurfaceChat,
		PrincipalID: caller.PrincipalID,
		Class:       caller.Class,
		SessionID:   caller.SessionID,
		AgentID:     caller.AgentID,
		Channel:     caller.Channel,
		Direct:      caller.Direct,
	}
}

func (c *Chat) Run(ctx context.Context, run *command.OperationRun) command.OperationReply {
	if run == nil || run.Caller == nil || run.Plan == nil {
		return failedReply(nilArgument("chat run"))
	}
	found, ok := c.registry.entries[run.Plan.Operation]
	if !ok {
		return failedReply(NewError(ErrorCodeUnknown, "operation: no such operation"))
	}
	call := &Call{
		Caller:         chatCaller(run.Caller),
		Input:          slices.Clone(run.Plan.Input),
		IdempotencyKey: run.IdempotencyKey,
		RequestDigest:  Digest(run.Plan.Input),
	}
	pending := &chatCall{found: found, call: call, run: run}
	if run.Intent != nil {
		return c.resume(ctx, pending)
	}
	return c.invoke(ctx, pending)
}

type chatCall struct {
	found *entry
	call  *Call
	run   *command.OperationRun
}

func (c *Chat) invoke(ctx context.Context, pending *chatCall) command.OperationReply {
	descriptor, call, apply := pending.found.descriptor, pending.call, pending.run.Plan.Apply
	if descriptor.previews() {
		preview, err := c.registry.Preview(ctx, descriptor.ID, call)
		if err != nil {
			return failedReply(err)
		}
		if !apply || preview.Empty {
			return recordedReply(descriptor.ID, previewLines(&preview))
		}
		call.PreviewDigest = preview.Digest
	}
	result, info, err := c.registry.run(ctx, pending.found, call)
	if err != nil {
		return failedRun(err, info.notPerformed)
	}
	lines, err := descriptor.renderedLines(SurfaceChat, result.Data)
	if err != nil {
		reply := failedReply(err)
		reply.Fate = command.FatePerformed
		return reply
	}
	return recordedReply(descriptor.ID, lines)
}

func previewLines(preview *Preview) []string {
	if len(preview.Lines) == 0 {
		return []string{chatNoChanges}
	}
	return slices.Clone(preview.Lines)
}

func (c *Chat) resume(ctx context.Context, pending *chatCall) command.OperationReply {
	run := pending.run
	if run.Recovery == nil || run.Recovery.Provider == nil || run.Recovery.Settle == nil {
		return failedReply(nilArgument("chat recovery"))
	}
	descriptor := pending.found.descriptor
	recovery := &toolbroker.RecoveryRun{
		Provider: run.Recovery.Provider,
		Settle: func(_ context.Context, data json.RawMessage) (effect.Outcome, error) {
			lines, err := descriptor.renderedLines(SurfaceChat, data)
			if err != nil {
				return effect.Outcome{}, err
			}
			record, err := chatRecord(descriptor.ID, lines)
			if err != nil {
				return effect.Outcome{}, err
			}
			return run.Recovery.Settle(lines, record)
		},
	}
	result, err := c.registry.Resume(ctx, run.Intent, pending.call, recovery)
	if err != nil {
		return failedReply(err)
	}
	var recorded chatOutput
	if err := json.Unmarshal(result.Data, &recorded); err != nil || len(recorded.Lines) == 0 {
		return failedReply(NewError(ErrorCodeFailed, "the recorded reply is unreadable"))
	}
	return command.OperationReply{Lines: recorded.Lines}
}

type chatOutput struct {
	Lines []string `json:"lines"`
}

func chatRecord(id string, lines []string) (json.RawMessage, error) {
	output, err := encodeJSON(chatOutput{Lines: lines})
	if err != nil {
		return nil, err
	}
	record, err := encodeJSON(toolbroker.ToolResult{ToolName: id, ToolVersion: tools.Version, Class: toolbroker.ResultOK, Output: output})
	if err != nil {
		return nil, err
	}
	return record, nil
}

func recordedReply(id string, lines []string) command.OperationReply {
	record, err := chatRecord(id, lines)
	if err != nil {
		return failedReply(err)
	}
	return command.OperationReply{Lines: lines, Record: record}
}

func failedRun(err error, notPerformed bool) command.OperationReply {
	reply := failedReply(err)
	if !notPerformed && !reply.InProgress {
		reply.Fate = command.FateAmbiguous
	}
	return reply
}

func failedReply(err error) command.OperationReply {
	if fault.HasCode(err, effect.ErrorCodeInProgress) {
		return command.OperationReply{InProgress: true}
	}
	text := safeText(err)
	if _, coded := fault.CodeOf[ErrorCode](err); !coded && fault.HasCode(err, effect.ErrorCodeUnknown) {
		text = string(ErrorCodeFailed) + ": the earlier attempt is unresolved; see aura effects list"
	}
	return command.OperationReply{Lines: []string{text}, Code: commandCodeOf(err), Fate: command.FateNotPerformed}
}

func commandCodeOf(err error) command.ErrorCode {
	code, coded := fault.CodeOf[ErrorCode](err)
	switch {
	case !coded:
		return command.ErrorCodeFailed
	case code == ErrorCodeInputInvalid || code == ErrorCodeInvalidArgument:
		return command.ErrorCodeUsage
	case code == ErrorCodeDenied:
		return command.ErrorCodeDenied
	case code == ErrorCodeNotExposed || code == ErrorCodeUnknown:
		return command.ErrorCodeUnavailable
	}
	return command.ErrorCodeFailed
}

func safeText(err error) string {
	var failure *fault.Error[ErrorCode]
	if interrupted(err) || !errors.As(err, &failure) {
		return string(ErrorCodeFailed)
	}
	if (failure.Code == ErrorCodeInputInvalid || failure.Code == ErrorCodeFailed) && failure.Detail != "" {
		return string(failure.Code) + ": " + failure.Detail
	}
	return string(failure.Code)
}

func (d *Descriptor) renderedLines(surface Surface, data json.RawMessage) ([]string, error) {
	var buffer bytes.Buffer
	if err := d.renderOn(surface, &buffer, data); err != nil {
		return nil, err
	}
	text := strings.TrimSuffix(buffer.String(), "\n")
	if text == "" {
		return []string{chatDone}, nil
	}
	return strings.Split(text, "\n"), nil
}
