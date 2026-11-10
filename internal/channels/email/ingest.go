package email

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

type replyReference struct {
	MessageID  string                  `json:"message_id"`
	Sender     string                  `json:"sender"`
	Subject    string                  `json:"subject"`
	References []string                `json:"references,omitempty"`
	Artifacts  []media.ArtifactSummary `json:"artifacts,omitempty"`
}

const attachmentOnlyText = "[attachment]"

type admission struct {
	messageID string
	subject   string
	principal string
	class     principal.Class
	persona   string
	session   string
	reply     json.RawMessage
}

func (a *Adapter) ingest(ctx context.Context, session Session, info MessageInfo) error {
	if info.Size > a.cfg.MaxAttachmentBytes+messageOverhead {
		a.drop(ctx, info.UID, dropMessageTooLarge, true)
		return nil
	}
	raw, err := session.Header(ctx, info.UID, maxHeaderBytes)
	if err != nil {
		return a.fetchFailure(ctx, info.UID, err)
	}
	if len(raw) > maxHeaderBytes {
		a.drop(ctx, info.UID, dropMessageTooLarge, true)
		return nil
	}
	header, ok := readHeader(raw)
	if !ok {
		a.drop(ctx, info.UID, dropMessageMalformed, false)
		return nil
	}
	if isAutomated(header) {
		a.drop(ctx, info.UID, dropAutomatedMail, false)
		return nil
	}
	sender, ok := senderOf(header)
	if !ok {
		a.drop(ctx, info.UID, dropSenderInvalid, false)
		return nil
	}
	if sender == a.account.Address {
		a.drop(ctx, info.UID, dropAutomatedMail, false)
		return nil
	}
	messageID, ok := messageIDOf(header)
	if !ok {
		a.drop(ctx, info.UID, dropSenderInvalid, false)
		return nil
	}
	if !authenticatedSender(&header.Header.Header, a.account.AuthservID, domainOf(sender)) {
		a.drop(ctx, info.UID, dropSenderUnauthenticated, true)
		return nil
	}
	found := admission{messageID: messageID, subject: subjectOf(header)}
	if reason := a.admit(ctx, sender, &found); reason != "" {
		a.drop(ctx, info.UID, reason, true)
		return nil
	}
	root := threadRoot(header, messageID)
	sessionID, err := a.threadSession(ctx, found.persona, root, found.principal)
	if err != nil {
		return &Error{Code: ErrorCodeIntakeFailed, Detail: "thread session is not resolvable", Cause: err}
	}
	found.session = sessionID
	reply, err := json.Marshal(replyReference{MessageID: messageID, Sender: sender, Subject: found.subject, References: referenceIDs(header)})
	if err != nil {
		return &Error{Code: ErrorCodeIntakeFailed, Detail: "reply reference is not encodable", Cause: err}
	}
	found.reply = reply
	return a.deliverToRuntime(ctx, session, info, &found)
}

func (a *Adapter) fetchFailure(ctx context.Context, uid uint32, err error) error {
	if code, ok := CodeOf(err); ok && code == ErrorCodeMessageUnavailable {
		a.drop(ctx, uid, dropMessageUnavailable, false)
		return nil
	}
	return err
}

func (a *Adapter) admit(ctx context.Context, sender string, found *admission) dropReason {
	allowlisted := slices.Contains(a.account.AllowedSenders, sender)
	principalID, class, known := a.owners.Resolve(principal.ChannelEmail, sender, allowlisted, a.servesPublic())
	if !known {
		return dropAuthorNotAllowed
	}
	persona, routed := a.routePersona(sender)
	if !routed {
		return dropPersonaUnrouted
	}
	definition, defined := a.personaDefinition(persona)
	if !defined || !definition.Admits(class) {
		return dropAudience
	}
	if reason := a.evalDrop(ctx, &definition, persona, class); reason != "" {
		return reason
	}
	if reason := a.budgetDrop(ctx, principalID, class); reason != "" {
		return reason
	}
	found.principal = principalID
	found.class = class
	found.persona = persona
	return ""
}

func (a *Adapter) routePersona(sender string) (string, bool) {
	if a.router == nil {
		return "", false
	}
	match, ok := a.router.Resolve(&auraagent.RouteScope{
		Channel: config.BindingChannelEmail,
		Account: a.account.Name,
		UserID:  sender,
	})
	if !ok {
		return "", false
	}
	return match.Persona, true
}

func (a *Adapter) personaDefinition(persona string) (auraagent.Definition, bool) {
	if a.router == nil {
		return auraagent.Definition{}, false
	}
	return a.router.Persona(persona)
}

func (a *Adapter) evalDrop(ctx context.Context, definition *auraagent.Definition, persona string, class principal.Class) dropReason {
	if class == principal.ClassOwner || a.access.Evals == nil || definition.Audience == config.AudienceOwner {
		return ""
	}
	evaluated, err := a.access.Evals.Evaluated(ctx, persona)
	if err != nil {
		a.logger.WarnContext(ctx, "persona eval lookup failed", "component", "email", "account", a.account.Name)
	}
	if err == nil && evaluated {
		return ""
	}
	return dropPersonaEvalMissing
}

func (a *Adapter) budgetDrop(ctx context.Context, principalID string, class principal.Class) dropReason {
	if a.access.Budgets == nil || class == principal.ClassOwner {
		return ""
	}
	refusal, err := a.access.Budgets.Admit(ctx, principalID, class)
	if err != nil {
		a.logger.WarnContext(ctx, "principal budget check failed", "component", "email", "account", a.account.Name)
		return dropBudgetCheckFailed
	}
	if refusal == "" {
		return ""
	}
	return dropReason(access.BudgetDropReason(refusal))
}

func (a *Adapter) deliverToRuntime(ctx context.Context, session Session, info MessageInfo, found *admission) error {
	raw, err := session.Message(ctx, info.UID, a.cfg.MaxAttachmentBytes+messageOverhead)
	if err != nil {
		return a.fetchFailure(ctx, info.UID, err)
	}
	if int64(len(raw)) > a.cfg.MaxAttachmentBytes+messageOverhead {
		a.drop(ctx, info.UID, dropMessageTooLarge, true)
		return nil
	}
	body, readable := readableBody(raw)
	if !readable {
		a.drop(ctx, info.UID, dropMessageMalformed, false)
		return nil
	}
	if inv, isForgetMe := a.forgetMeRequest(body); isForgetMe {
		a.forgetMe(ctx, &inv, found)
		return nil
	}
	if reason := a.handoffDrop(ctx, found); reason != "" {
		a.drop(ctx, info.UID, reason, true)
		return nil
	}
	stage, err := stageAttachments(raw, a.cfg.MaxAttachmentBytes)
	if err != nil {
		a.logger.WarnContext(ctx, "email attachment staging failed", "component", "email", "account", a.account.Name)
		a.drop(ctx, info.UID, dropAttachmentFailed, true)
		return nil
	}
	defer stage.discard()
	text := turnInput(found.subject, body)
	if text == "" && len(stage.items) == 0 {
		a.drop(ctx, info.UID, dropEmptyContent, false)
		return nil
	}
	if err := a.sessions.EnsureSession(ctx, found.session, found.principal, found.persona); err != nil {
		return &Error{Code: ErrorCodeIntakeFailed, Detail: "session is not ensurable", Cause: err}
	}
	summaries, images, err := a.storeAttachments(ctx, stage, found)
	if err == nil && len(summaries) > 0 {
		err = attachArtifacts(found, summaries)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.logger.WarnContext(ctx, "email attachment intake failed", "component", "email", "account", a.account.Name)
		a.drop(ctx, info.UID, dropAttachmentFailed, true)
		return nil
	}
	parts := []runtimeingress.InputPart{{Text: text}}
	if text == "" {
		parts[0].Text = attachmentOnlyText
	}
	for _, image := range images {
		parts = append(parts, runtimeingress.InputPart{Image: image})
	}
	envelope := &runtimeingress.IngressEnvelope{
		Source:         a.source(),
		AgentID:        found.persona,
		ExternalID:     a.source() + ":" + found.messageID,
		PrincipalID:    found.principal,
		PrincipalClass: string(found.class),
		ConversationID: found.session,
		ReplyContext:   found.reply,
		Parts:          parts,
		ReceivedAt:     time.Now().UTC(),
	}
	a.rememberSession(found)
	if _, err := a.sink.Accept(ctx, envelope); err != nil {
		if retryableIntake(err) {
			return &Error{Code: ErrorCodeIntakeFailed, Detail: "runtime did not accept the message", Cause: err}
		}
		a.drop(ctx, info.UID, dropIntakeRejected, true)
		return nil
	}
	if a.admitted != nil {
		a.admitted(ctx, found.class)
	}
	return nil
}

func retryableIntake(err error) bool {
	code, ok := turn.CodeOf(err)
	if !ok {
		return true
	}
	switch code {
	case turn.ErrorCodeRuntimeOverloaded, turn.ErrorCodeStorageUnavailable, turn.ErrorCodeRuntimeInternal:
		return true
	default:
		return false
	}
}

func (a *Adapter) threadSession(ctx context.Context, persona, root, principalID string) (string, error) {
	base := runtimeingress.SessionKey(persona, a.source(), "thread:"+digestOf(root))
	owner, found, err := a.sessions.SessionOwner(ctx, base)
	if err != nil {
		return "", err
	}
	if !found || owner == principalID {
		return base, nil
	}
	return base + "/principal:" + digestOf(principalID), nil
}

func readableBody(raw []byte) (string, bool) {
	body, err := messageBody(raw)
	return body, err == nil
}
