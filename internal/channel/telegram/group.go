package telegram

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

const (
	chatTypeGroup      = "group"
	chatTypeSupergroup = "supergroup"

	entityMention     = "mention"
	entityTextMention = "text_mention"
	entityBotCommand  = "bot_command"
)

type destination struct {
	chatID   int64
	threadID int64
}

func (m *message) isGroup() bool {
	return m.Chat.Type == chatTypeGroup || m.Chat.Type == chatTypeSupergroup
}

func (m *message) topicID() int64 {
	if m.isGroup() && m.IsTopicMessage && m.ThreadID > 0 {
		return m.ThreadID
	}
	return 0
}

func (m *message) destination() destination {
	return destination{chatID: m.Chat.ID, threadID: m.topicID()}
}

func (a *Adapter) chatAllowed(chatID int64) bool {
	return slices.Contains(a.account.AllowedChatIDs, strconv.FormatInt(chatID, 10))
}

func (a *Adapter) selfIdentity() (botIdentity, bool) {
	self := a.identity.Load()
	if self == nil {
		return botIdentity{}, false
	}
	return *self, true
}

func (a *Adapter) identityLoop(ctx context.Context) {
	attempt := 0
	for {
		err := a.fetchIdentity(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.logger.WarnContext(ctx, "telegram bot identity lookup failed", "component", "telegram", "account", a.account.Name, "error", err)
			if !sleepContext(ctx, a.retryDelay(attempt, err)) {
				return
			}
			attempt++
			continue
		}
		attempt = 0
		select {
		case <-ctx.Done():
			return
		case <-a.identityRefresh:
		}
	}
}

func (a *Adapter) fetchIdentity(ctx context.Context) error {
	token, ok := a.botToken()
	if !ok {
		return errTokenUnavailable
	}
	call, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	self, err := a.bot.getMe(call, token)
	if err != nil {
		return err
	}
	a.identity.Store(&self)
	return nil
}

func (a *Adapter) refreshIdentity() {
	if len(a.account.AllowedChatIDs) == 0 {
		return
	}
	select {
	case a.identityRefresh <- struct{}{}:
	default:
	}
}

func (a *Adapter) groupAddressing(msg *message, self botIdentity) (string, bool) {
	units := utf16.Encode([]rune(msg.Text))
	var cuts []textCut
	for i := range msg.Entities {
		entity := &msg.Entities[i]
		if entity.Offset < 0 || entity.Length <= 0 || entity.Offset+entity.Length > len(units) {
			continue
		}
		span := string(utf16.Decode(units[entity.Offset : entity.Offset+entity.Length]))
		switch entity.Type {
		case entityMention:
			if strings.EqualFold(span, "@"+self.Username) {
				cuts = append(cuts, textCut{start: entity.Offset, end: entity.Offset + entity.Length, spacing: true})
			}
		case entityTextMention:
			if entity.User != nil && entity.User.ID == self.ID {
				cuts = append(cuts, textCut{start: entity.Offset, end: entity.Offset + entity.Length, spacing: true})
			}
		case entityBotCommand:
			command, target, found := strings.Cut(span, "@")
			if found && strings.EqualFold(target, self.Username) {
				start := entity.Offset + len(utf16.Encode([]rune(command)))
				cuts = append(cuts, textCut{start: start, end: entity.Offset + entity.Length})
			}
		}
	}
	addressed := len(cuts) > 0
	if reply := msg.ReplyTo; reply != nil && reply.From != nil && reply.From.ID == self.ID && reply.ForumTopicCreated == nil {
		addressed = true
	}
	if len(cuts) == 0 {
		return msg.Text, addressed
	}
	return strings.TrimSpace(applyCuts(units, cuts)), addressed
}

type textCut struct {
	start, end int
	spacing    bool
}

func applyCuts(units []uint16, cuts []textCut) string {
	slices.SortFunc(cuts, func(a, b textCut) int { return a.start - b.start })
	kept := make([]uint16, 0, len(units))
	position := 0
	for _, cut := range cuts {
		if cut.start < position {
			continue
		}
		kept = append(kept, units[position:cut.start]...)
		position = cut.end
		if cut.spacing && (cut.start == 0 || isSpaceUnit(units[cut.start-1])) {
			for position < len(units) && (units[position] == ' ' || units[position] == '\t') {
				position++
			}
		}
	}
	kept = append(kept, units[position:]...)
	return string(utf16.Decode(kept))
}

func isSpaceUnit(unit uint16) bool {
	return unit == ' ' || unit == '\t' || unit == '\n' || unit == '\r'
}

func (a *Adapter) groupConversation(persona string, msg *message, principalID string, class principal.Class) string {
	conversation := "chat:" + strconv.FormatInt(msg.Chat.ID, 10)
	if topic := msg.topicID(); topic > 0 {
		conversation += "/topic:" + strconv.FormatInt(topic, 10)
	}
	if class == principal.ClassOwner {
		return conversation
	}
	if definition, ok := a.personaDefinition(persona); ok && definition.GroupContext == config.GroupContextShared {
		return conversation
	}
	return conversation + "/principal:" + principalID
}
