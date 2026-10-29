package discord

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

type replyReference struct {
	ChannelID string            `json:"channel_id"`
	MessageID string            `json:"message_id"`
	GuildID   string            `json:"guild_id,omitempty"`
	ThreadID  string            `json:"thread_id,omitempty"`
	Artifacts []artifactSummary `json:"artifacts,omitempty"`
}

func normalizeMessage(cfg *config.Discord, owners *principal.Resolver, selfID, instance string, msg *messagePayload) (*runtimeingress.IngressEnvelope, bool, error) {
	if cfg == nil || msg == nil {
		return nil, false, Errorf(ErrorCodeInvalidArgument, "config and message must not be nil")
	}
	if msg.ID == "" || msg.ChannelID == "" || msg.Author.ID == "" {
		return nil, false, Errorf(ErrorCodeProtocolInvalid, "message is missing id, channel, or author")
	}
	if msg.Author.Bot || msg.Author.ID == selfID {
		return nil, false, nil
	}
	if msg.Type != 0 && msg.Type != 19 {
		return nil, false, nil
	}
	if msg.Content == "" {
		return nil, false, nil
	}
	principalID, class, admitted := resolveAuthor(cfg, owners, msg.Author.ID)
	if !admitted {
		return nil, false, nil
	}
	var conversationID string
	if msg.GuildID != "" {
		if len(cfg.AllowedChannelIDs) > 0 && !slices.Contains(cfg.AllowedChannelIDs, msg.ChannelID) {
			return nil, false, nil
		}
		if len(cfg.AllowedGuildIDs) > 0 && !slices.Contains(cfg.AllowedGuildIDs, msg.GuildID) {
			return nil, false, nil
		}
		mentioned := slices.ContainsFunc(msg.Mentions, func(user userPayload) bool { return user.ID == selfID })
		if !mentioned {
			return nil, false, nil
		}
		conversationID = "channel:" + msg.ChannelID
	} else {
		if !cfg.AcceptDMs {
			return nil, false, nil
		}
		conversationID = "dm:" + msg.Author.ID
	}
	reference, err := json.Marshal(replyReference{
		ChannelID: msg.ChannelID,
		MessageID: msg.ID,
		GuildID:   msg.GuildID,
	})
	if err != nil {
		return nil, false, Errorf(ErrorCodeProtocolInvalid, "reply reference is not serializable")
	}
	return &runtimeingress.IngressEnvelope{
		Source:         "discord:" + instance,
		ExternalID:     "message:" + msg.ID,
		PrincipalID:    principalID,
		PrincipalClass: string(class),
		ConversationID: conversationID,
		ReplyContext:   reference,
		Parts:          []runtimeingress.InputPart{{Text: msg.Content}},
		ReceivedAt:     time.Now().UTC(),
	}, true, nil
}

func resolveAuthor(cfg *config.Discord, owners *principal.Resolver, authorID string) (string, principal.Class, bool) {
	return owners.Resolve(principal.ChannelDiscord, authorID, slices.Contains(cfg.AllowedUserIDs, authorID))
}
