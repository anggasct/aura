package telegram

import (
	"context"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/command"
)

const (
	menuScopeDefault = "default"
	menuScopeChat    = "chat"
)

func wireCommandName(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}

func restoreCommandName(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return text
	}
	wire := strings.ToLower(fields[0][1:])
	for _, name := range command.MessengerNames() {
		if name != wire && wireCommandName(name) == wire {
			start := strings.Index(text, fields[0])
			return text[:start] + "/" + name + text[start+len(fields[0]):]
		}
	}
	return text
}

func menuCommands(audiences ...command.Audience) []botCommand {
	bound := command.MessengerNames()
	var menu []botCommand
	catalog := command.Catalog()
	for i := range catalog {
		def := &catalog[i]
		if slices.Contains(bound, def.Name) && slices.Contains(audiences, def.Audience) {
			menu = append(menu, botCommand{Command: wireCommandName(def.Name), Description: def.Summary})
		}
	}
	return menu
}

func (a *Adapter) publishCommandMenu(ctx context.Context) {
	if a.commands == nil {
		return
	}
	token, ok := a.botToken()
	if !ok {
		a.logger.WarnContext(ctx, "telegram command menu token unavailable", "component", "telegram", "account", a.account.Name)
		return
	}
	requests := []setMyCommandsRequest{{
		Commands: menuCommands(command.AudienceAny, command.AudienceGuests),
		Scope:    botCommandScope{Type: menuScopeDefault},
	}}
	if ownerChat, known := a.ownerChatID(); known {
		requests = append(requests, setMyCommandsRequest{
			Commands: menuCommands(command.AudienceOwner, command.AudienceAny),
			Scope:    botCommandScope{Type: menuScopeChat, ChatID: ownerChat},
		})
	}
	for i := range requests {
		call, cancel := context.WithTimeout(ctx, sendTimeout)
		err := a.bot.setMyCommands(call, token, &requests[i])
		cancel()
		if err != nil {
			a.logger.WarnContext(ctx, "telegram command menu not registered", "component", "telegram", "account", a.account.Name, "scope", requests[i].Scope.Type, "error", err)
		}
	}
}
