package operation

import (
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

const MaxDescriptionBytes = 6144

const (
	headingOperate  = "# What you can operate"
	headingBehaves  = "# Running on its own"
	headingWithheld = "# Other Aura tools not enabled for this persona"

	introSentence        = "You run inside Aura and can operate Aura itself. Each line is a group: name, summary, verbs. "
	searchSentence       = "Call " + tools.OperationSearchToolName + " with a group or with words to load the operations you need, then call them. "
	listedSentence       = "Call the operation tools you have been given to run them. "
	attendedAsks         = "A verb marked ^ always needs the owner's approval card; a verb marked ? asks the owner first; other verbs run without a card unless this conversation carries untrusted input. "
	attendedTracked      = "A verb marked ^ always needs the owner's approval card; other verbs run without a card unless this conversation carries untrusted input. "
	unattendedAsksHead   = "Nobody can approve on this turn: verbs marked ^ or ? are refused here; give the owner the "
	unattendedTrackHead  = "Nobody can approve on this turn: verbs marked ^ are refused here; give the owner the "
	unattendedTail       = " for them. Other verbs run unless this conversation carries untrusted input, when changes are refused too. "
	chatForm             = "chat command /<group> <verb>"
	terminalForm         = "command aura <group> <verb>"
	trustGrantSentence   = "A verb marked ^ that grants trust asks with a warning card, or is refused, when this conversation carries untrusted input, at most one card per owner message"
	trustGrantSearch     = "; a search result states what applies now. "
	trustGrantListed     = ". "
	ownerHostChat        = "A verb marked (owner) only the owner can run: give them the chat command /<group> <verb> in a direct chat. A verb marked (host) only runs on the host: give them aura <group> <verb>. "
	ownerHostTerminal    = "A verb marked (owner) or (host) is not yours to run: give the owner the command aura <group> <verb> to run it in a terminal on the host. "
	honestySentence      = "Do not say you lack a capability listed here, and do not say you did something you did not call."
	sessionFullNew       = " Loaded operations stay for this session; when a search says the session is full, the owner can start a new one with /new."
	sessionFullElsewhere = " Loaded operations stay for this session; when a search says the session is full, tell the owner so they can continue in a new session."
)

type DescriptionInput struct {
	Turn     Turn
	Search   bool
	Listed   []string
	Withheld []string
}

func (d *Discovery) preamble(in *DescriptionInput) string {
	var text strings.Builder
	text.WriteString(introSentence)
	if in.Search {
		text.WriteString(searchSentence)
	} else {
		text.WriteString(listedSentence)
	}
	form := terminalForm
	ownerHost := ownerHostTerminal
	if d.chatCommands {
		form, ownerHost = chatForm, ownerHostChat
	}
	switch {
	case in.Turn.Kind == TurnUnattended && in.Turn.ContentTaintTracked:
		text.WriteString(unattendedTrackHead + form + unattendedTail)
	case in.Turn.Kind == TurnUnattended:
		text.WriteString(unattendedAsksHead + form + unattendedTail)
	case in.Turn.ContentTaintTracked:
		text.WriteString(attendedTracked)
	default:
		text.WriteString(attendedAsks)
	}
	text.WriteString(trustGrantSentence)
	if in.Search {
		text.WriteString(trustGrantSearch)
	} else {
		text.WriteString(trustGrantListed)
	}
	text.WriteString(ownerHost)
	text.WriteString(honestySentence)
	if in.Search {
		text.WriteString(d.sessionFull(in.Turn.Origin))
	}
	return text.String()
}

func (d *Discovery) sessionFull(origin turn.Origin) string {
	switch {
	case origin == turn.OriginTerminal:
		return sessionFullNew
	case d.chatCommands && (origin.OnChannel(principal.ChannelTelegram) || origin.OnChannel(principal.ChannelDiscord)):
		return sessionFullNew
	}
	return sessionFullElsewhere
}

func (d *Discovery) Describe(in *DescriptionInput) (string, error) {
	if in == nil {
		return "", nilArgument("description input")
	}
	if !in.Turn.Kind.valid() {
		return "", NewError(ErrorCodeInvalidArgument, "operation: the description needs a known turn kind")
	}
	groups := d.renderGroups(in)
	if len(groups) == 0 {
		return "", nil
	}
	var text strings.Builder
	text.WriteString(headingOperate + "\n" + d.preamble(in) + "\n")
	for i := range groups {
		text.WriteString(groups[i].line + "\n")
	}
	if behaviors := d.behaviorLines(groups); len(behaviors) > 0 {
		text.WriteString(headingBehaves + "\n")
		for _, line := range behaviors {
			text.WriteString(line + "\n")
		}
	}
	if withheld := uniqueSorted(in.Withheld); len(withheld) > 0 {
		text.WriteString(headingWithheld + "\n" + strings.Join(withheld, ", ") + "\n")
	}
	described := strings.TrimSuffix(text.String(), "\n")
	if len(described) > MaxDescriptionBytes {
		return "", NewError(ErrorCodeInvalid, "operation: the capability description is over its byte bound")
	}
	return described, nil
}

func uniqueSorted(names []string) []string {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	return slices.Compact(sorted)
}

type renderedGroup struct {
	name string
	line string
}

func (d *Discovery) renderGroups(in *DescriptionInput) []renderedGroup {
	marking := markerTurn(in.Turn.ContentTaintTracked)
	var rendered []renderedGroup
	for _, group := range d.registry.groups {
		var verbs []string
		for _, id := range d.registry.members[group.Name] {
			found := d.registry.entries[id]
			if !d.available(found.descriptor) {
				continue
			}
			approval := found.approval(marking)
			if !in.Search && !d.listed(found, approval, in.Listed) {
				continue
			}
			verbs = append(verbs, verbOf(group.Name, id)+verbSuffix(approval))
		}
		if len(verbs) > 0 {
			rendered = append(rendered, renderedGroup{name: group.Name, line: "- " + group.Name + ": " + group.Summary + ": " + strings.Join(verbs, ", ")})
		}
	}
	return rendered
}

func (d *Discovery) listed(found *entry, approval Approval, listed []string) bool {
	if approval == ApprovalOwner || approval == ApprovalHost {
		return true
	}
	descriptor := found.descriptor
	switch {
	case descriptor.ModuleTool != "":
		return slices.Contains(listed, descriptor.ModuleTool)
	case descriptor.GeneratesTool():
		return slices.Contains(listed, ToolName(descriptor.ID))
	}
	return false
}

func verbOf(group, id string) string {
	return strings.ReplaceAll(strings.TrimPrefix(id, group+"."), ".", " ")
}

func verbSuffix(approval Approval) string {
	switch approval {
	case ApprovalOwner:
		return " (owner)"
	case ApprovalHost:
		return " (host)"
	case ApprovalNone, ApprovalUntrustedInput, ApprovalAsks, ApprovalAlways, ApprovalRefusedHere:
		return approval.marker()
	}
	return ""
}

func (d *Discovery) behaviorLines(groups []renderedGroup) []string {
	lines := make([]string, 0, len(d.behaviors))
	for i := range d.behaviors {
		behavior := &d.behaviors[i]
		line := "- " + behavior.ID + ": " + behavior.Summary
		if behavior.Group != "" && slices.ContainsFunc(groups, func(g renderedGroup) bool { return g.name == behavior.Group }) {
			line += " (" + behavior.Group + " group)"
		}
		lines = append(lines, line+".")
	}
	return lines
}
