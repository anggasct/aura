package discord

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

type replyReference struct {
	ChannelID string                  `json:"channel_id"`
	MessageID string                  `json:"message_id"`
	GuildID   string                  `json:"guild_id,omitempty"`
	ThreadID  string                  `json:"thread_id,omitempty"`
	Artifacts []media.ArtifactSummary `json:"artifacts,omitempty"`
}

func normalizeMessage(account *config.DiscordAccount, owners *principal.Resolver, selfID string, access authorAccess, msg *messagePayload) (*runtimeingress.IngressEnvelope, dropReason, error) {
	if account == nil || msg == nil {
		return nil, "", Errorf(ErrorCodeInvalidArgument, "account and message must not be nil")
	}
	if msg.ID == "" || msg.ChannelID == "" || msg.Author.ID == "" {
		return nil, "", Errorf(ErrorCodeProtocolInvalid, "message is missing id, channel, or author")
	}
	if reason := authorDrop(account, owners, selfID, access, msg); reason != "" {
		return nil, reason, nil
	}
	principalID, class, _ := resolveAuthor(account, owners, msg.Author.ID, access)
	var conversationID string
	if msg.GuildID != "" {
		if len(account.AllowedChannelIDs) > 0 && !slices.Contains(account.AllowedChannelIDs, msg.ChannelID) {
			return nil, dropChannelNotAllowed, nil
		}
		if len(account.AllowedGuildIDs) > 0 && !slices.Contains(account.AllowedGuildIDs, msg.GuildID) {
			return nil, dropGuildNotAllowed, nil
		}
		mentioned := slices.ContainsFunc(msg.Mentions, func(user userPayload) bool { return user.ID == selfID })
		if !mentioned {
			return nil, dropNotMentioned, nil
		}
		conversationID = "channel:" + msg.ChannelID
	} else {
		if !account.AcceptDMs {
			return nil, dropDMsDisabled, nil
		}
		conversationID = "dm:" + msg.Author.ID
	}
	reference, err := json.Marshal(replyReference{
		ChannelID: msg.ChannelID,
		MessageID: msg.ID,
		GuildID:   msg.GuildID,
	})
	if err != nil {
		return nil, "", Errorf(ErrorCodeProtocolInvalid, "reply reference is not serializable")
	}
	return &runtimeingress.IngressEnvelope{
		Source:         "discord:" + account.Name,
		ExternalID:     "message:" + msg.ID,
		PrincipalID:    principalID,
		PrincipalClass: string(class),
		ConversationID: conversationID,
		ReplyContext:   reference,
		Parts:          []runtimeingress.InputPart{{Text: msg.Content}},
		ReceivedAt:     time.Now().UTC(),
	}, "", nil
}

type authorAccess struct {
	servesPublic bool
	granted      func(authorID string) bool
}

func resolveAuthor(account *config.DiscordAccount, owners *principal.Resolver, authorID string, access authorAccess) (string, principal.Class, bool) {
	allowlisted := slices.Contains(account.AllowedUserIDs, authorID)
	if !allowlisted && access.granted != nil && authorID != "" && !owners.IsOwnerIdentity(principal.ChannelDiscord, authorID) {
		allowlisted = access.granted(authorID)
	}
	return owners.Resolve(principal.ChannelDiscord, authorID, allowlisted, access.servesPublic)
}
