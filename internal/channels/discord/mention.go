package discord

import (
	"regexp"
	"strings"
	"unicode"
)

const maxMentionDisplayRunes = 32

var userMentionPattern = regexp.MustCompile(`<@!?(\d+)>`)

func (u *userPayload) displayName() string {
	if u.Member != nil && strings.TrimSpace(u.Member.Nick) != "" {
		return u.Member.Nick
	}
	if strings.TrimSpace(u.GlobalName) != "" {
		return u.GlobalName
	}
	return u.Username
}

func (a *Adapter) modelText(msg *messagePayload) string {
	return renderMentions(msg.Content, msg.Mentions, a.selfID, a.selfName)
}

func renderMentions(content string, mentions []userPayload, selfID, selfName string) string {
	names := make(map[string]string, len(mentions)+1)
	if name := sanitizeDisplayName(selfName); selfID != "" && name != "" {
		names[selfID] = name
	}
	for i := range mentions {
		if name := sanitizeDisplayName(mentions[i].displayName()); mentions[i].ID != "" && name != "" {
			names[mentions[i].ID] = name
		}
	}
	if len(names) == 0 {
		return content
	}
	return userMentionPattern.ReplaceAllStringFunc(content, func(token string) string {
		match := userMentionPattern.FindStringSubmatch(token)
		if name, ok := names[match[1]]; ok {
			return "@" + name
		}
		return token
	})
}

func sanitizeDisplayName(name string) string {
	visible := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		default:
			return r
		}
	}, name)
	runes := []rune(strings.Join(strings.Fields(visible), " "))
	if len(runes) > maxMentionDisplayRunes {
		runes = runes[:maxMentionDisplayRunes]
	}
	return strings.TrimSpace(string(runes))
}
