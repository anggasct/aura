package operation

import (
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/tools"
)

const MaxDescriptionBytes = 6144

const (
	headingOperate  = "# What you can operate"
	headingBehaves  = "# Running on its own"
	headingWithheld = "# Other Aura tools not enabled for this persona"

	introSentence       = "You run inside Aura and can operate Aura itself. Each line is a group: name, summary, verbs. "
	searchSentence      = "Call " + tools.OperationSearchToolName + " with a group or with words to load the operations you need, then call them. "
	listedSentence      = "Call the operation tools you have been given to run them. "
	attendedAsks        = "A verb marked ^ always needs the owner's approval card; a verb marked ? asks the owner first; other verbs run without a card unless this conversation carries untrusted input. "
	attendedTracked     = "A verb marked ^ always needs the owner's approval card; other verbs run without a card unless this conversation carries untrusted input. "
	unattendedAsks      = "Nobody can approve on this turn: verbs marked ^ or ? are refused here; give the owner the chat command /<group> <verb> for them. Other verbs run. "
	unattendedTracked   = "Nobody can approve on this turn: verbs marked ^ are refused here; give the owner the chat command /<group> <verb> for them. Other verbs run. "
	ownerHostSentence   = "A verb marked (owner) only the owner can run: give them the chat command /<group> <verb> in a direct chat. A verb marked (host) only runs on the host: give them aura <group> <verb>. "
	honestySentence     = "Do not say you lack a capability listed here, and do not say you did something you did not call."
	sessionFullSentence = " Loaded operations stay for this session; when a search says the session is full, the owner can start a new one with /new."
)

type DescriptionInput struct {
	Turn     Turn
	Search   bool
	Withheld []string
}

func preamble(in *DescriptionInput) string {
	var text strings.Builder
	text.WriteString(introSentence)
	if in.Search {
		text.WriteString(searchSentence)
	} else {
		text.WriteString(listedSentence)
	}
	switch {
	case in.Turn.Kind == TurnUnattended && in.Turn.ContentTaintTracked:
		text.WriteString(unattendedTracked)
	case in.Turn.Kind == TurnUnattended:
		text.WriteString(unattendedAsks)
	case in.Turn.ContentTaintTracked:
		text.WriteString(attendedTracked)
	default:
		text.WriteString(attendedAsks)
	}
	text.WriteString(ownerHostSentence)
	text.WriteString(honestySentence)
	if in.Search {
		text.WriteString(sessionFullSentence)
	}
	return text.String()
}

func (d *Discovery) Describe(in *DescriptionInput) (string, error) {
	if in == nil {
		return "", nilArgument("description input")
	}
	if !in.Turn.Kind.valid() {
		return "", NewError(ErrorCodeInvalidArgument, "operation: the description needs a known turn kind")
	}
	groups := d.renderGroups(in.Turn.ContentTaintTracked)
	if len(groups) == 0 {
		return "", nil
	}
	var text strings.Builder
	text.WriteString(headingOperate + "\n" + preamble(in) + "\n")
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

func (d *Discovery) renderGroups(tracked bool) []renderedGroup {
	marking := markerTurn(tracked)
	var rendered []renderedGroup
	for _, group := range d.registry.groups {
		var verbs []string
		for _, id := range d.registry.members[group.Name] {
			found := d.registry.entries[id]
			if !d.available(found.descriptor) {
				continue
			}
			verbs = append(verbs, verbOf(group.Name, id)+verbSuffix(found.approval(marking)))
		}
		if len(verbs) > 0 {
			rendered = append(rendered, renderedGroup{name: group.Name, line: "- " + group.Name + ": " + group.Summary + ": " + strings.Join(verbs, ", ")})
		}
	}
	return rendered
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
	var lines []string
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
