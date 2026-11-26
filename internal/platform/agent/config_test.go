package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	auratools "github.com/anggasct/aura/internal/platform/tools"
)

func TestValidKnowledgeCollectionID(t *testing.T) {
	for id, want := range map[string]bool{"ab": true, "hr-docs": true, "a1-b2": true, "a": false, "Ab": false, "-ab": false, "ab_": false, strings.Repeat("a", 48): true, strings.Repeat("a", 49): false, "": false} {
		if got := ValidKnowledgeCollectionID(id); got != want {
			t.Errorf("ValidKnowledgeCollectionID(%q) = %t, want %t", id, got, want)
		}
	}
}

func definitionConfig(edit func(*Persona)) Config {
	persona := Persona{ID: "desk"}
	edit(&persona)
	return Config{Definitions: []Persona{persona}}
}

func TestAgentConfigValidate(t *testing.T) {
	manyCollections := make([]string, MaxPersonaCollections+1)
	for i := range manyCollections {
		manyCollections[i] = fmt.Sprintf("docs-%d", i)
	}
	for _, test := range []struct {
		name   string
		config Config
		want   string
	}{
		{"default is valid", Default(), ""},
		{"a plain definition is valid", definitionConfig(func(*Persona) {}), ""},
		{"a fully set definition is valid", definitionConfig(func(p *Persona) {
			p.Name = "Desk"
			p.Audience = AudienceTrusted
			p.Pairing = true
			p.GroupContext = GroupContextShared
			p.PrincipalTools = []string{"read_file"}
			p.PublicTools = []string{"read_file"}
			p.Workflows = []string{"book_visit"}
			p.Plugins = []string{"weather"}
			p.Knowledge = []string{"docs", "faq"}
			p.PrincipalKnowledge = []string{"docs"}
			p.PublicKnowledge = []string{"faq"}
			p.ApprovalTimeout = configkit.Duration(time.Minute)
			p.Messages = map[string]string{MessagePairingCode: "code " + PairingCodePlaceholder, MessageUnavailable: "away"}
		}), ""},
		{"name with surrounding whitespace", definitionConfig(func(p *Persona) { p.Name = " Desk" }), "definitions[0].name: persona_invalid: must not have surrounding whitespace"},
		{"name with a control character", definitionConfig(func(p *Persona) { p.Name = "De\tsk" }), "definitions[0].name: persona_invalid: must be a single line without control characters"},
		{"name over the length limit", definitionConfig(func(p *Persona) { p.Name = strings.Repeat("n", MaxPersonaNameRunes+1) }), "definitions[0].name: persona_invalid: must be at most 64 characters"},
		{"name at the length limit", definitionConfig(func(p *Persona) { p.Name = strings.Repeat("n", MaxPersonaNameRunes) }), ""},
		{"name user", definitionConfig(func(p *Persona) { p.Name = "User" }), "definitions[0].name: persona_invalid: must not be \"user\""},
		{"unknown audience", definitionConfig(func(p *Persona) { p.Audience = "everyone" }), "definitions[0].audience: persona_invalid: must be \"owner\", \"trusted\" or \"public\""},
		{"pairing on the default audience", definitionConfig(func(p *Persona) { p.Pairing = true }), "definitions[0].pairing: persona_invalid: requires audience \"trusted\""},
		{"pairing on public", definitionConfig(func(p *Persona) { p.Audience, p.Pairing = AudiencePublic, true }), "definitions[0].pairing: persona_invalid: requires audience \"trusted\""},
		{"shared group context on the default audience", definitionConfig(func(p *Persona) { p.GroupContext = GroupContextShared }), "definitions[0].group_context: persona_invalid: \"shared\" requires audience \"trusted\""},
		{"shared group context on public", definitionConfig(func(p *Persona) { p.Audience, p.GroupContext = AudiencePublic, GroupContextShared }), "definitions[0].group_context: persona_invalid: \"shared\" requires audience \"trusted\""},
		{"unknown group context", definitionConfig(func(p *Persona) { p.GroupContext = "everyone" }), "definitions[0].group_context: persona_invalid: must be \"per_principal\" or \"shared\""},
		{"duplicate principal tool", definitionConfig(func(p *Persona) { p.PrincipalTools = []string{"read_file", "read_file"} }), "definitions[0].principal_tools: persona_invalid: must not list a tool twice"},
		{"duplicate public tool", definitionConfig(func(p *Persona) { p.PublicTools = []string{"read_file", "list_dir", "read_file"} }), "definitions[0].public_tools: persona_invalid: must not list a tool twice"},
		{"empty workflow id", definitionConfig(func(p *Persona) { p.Workflows = []string{" "} }), "definitions[0].workflows: persona_invalid: entries must be non-empty workflow ids"},
		{"padded workflow id", definitionConfig(func(p *Persona) { p.Workflows = []string{" book_visit"} }), "definitions[0].workflows: persona_invalid: entries must be non-empty workflow ids"},
		{"duplicate workflow", definitionConfig(func(p *Persona) { p.Workflows = []string{"book_visit", "book_visit"} }), "definitions[0].workflows: persona_invalid: must not list a workflow twice"},
		{"plugin id with a path shape", definitionConfig(func(p *Persona) { p.Plugins = []string{"../x"} }), "definitions[0].plugins: persona_invalid: entries must be plugin ids"},
		{"duplicate plugin", definitionConfig(func(p *Persona) { p.Plugins = []string{"weather", "weather"} }), "definitions[0].plugins: persona_invalid: must not list a plugin twice"},
		{"too many knowledge collections", definitionConfig(func(p *Persona) { p.Knowledge = manyCollections }), "definitions[0].knowledge: persona_invalid: must list at most 32 collections"},
		{"invalid knowledge collection id", definitionConfig(func(p *Persona) { p.Knowledge = []string{"Docs"} }), "definitions[0].knowledge: persona_invalid: entries must match ^[a-z][a-z0-9-]{1,47}$"},
		{"duplicate knowledge collection", definitionConfig(func(p *Persona) { p.Knowledge = []string{"docs", "docs"} }), "definitions[0].knowledge: persona_invalid: must not list a collection twice"},
		{"principal knowledge outside the owner list", definitionConfig(func(p *Persona) {
			p.Knowledge, p.PrincipalKnowledge = []string{"docs"}, []string{"faq"}
		}), "definitions[0].principal_knowledge: persona_invalid: lists \"faq\", which is not in agents.definitions[0].knowledge"},
		{"public knowledge outside the owner list", definitionConfig(func(p *Persona) { p.PublicKnowledge = []string{"faq"} }), "definitions[0].public_knowledge: persona_invalid: lists \"faq\", which is not in agents.definitions[0].knowledge"},
		{"approval timeout under the minimum", definitionConfig(func(p *Persona) { p.ApprovalTimeout = configkit.Duration(59 * time.Second) }), "definitions[0].approval_timeout: config_invalid: must be between 1m and 24h"},
		{"approval timeout over the maximum", definitionConfig(func(p *Persona) { p.ApprovalTimeout = configkit.Duration(24*time.Hour + 1) }), "definitions[0].approval_timeout: config_invalid: must be between 1m and 24h"},
		{"approval timeout at the maximum", definitionConfig(func(p *Persona) { p.ApprovalTimeout = configkit.Duration(24 * time.Hour) }), ""},
		{"unknown message key", definitionConfig(func(p *Persona) { p.Messages = map[string]string{"hello": "hi"} }), "definitions[0].messages: persona_invalid: unknown key \"hello\""},
		{"empty message", definitionConfig(func(p *Persona) { p.Messages = map[string]string{MessageUnavailable: " "} }), "definitions[0].messages.unavailable: persona_invalid: must not be empty"},
		{"message with a control character", definitionConfig(func(p *Persona) { p.Messages = map[string]string{MessageUnavailable: "a\nb"} }), "definitions[0].messages.unavailable: persona_invalid: must be a single line without control characters"},
		{"message over the length limit", definitionConfig(func(p *Persona) {
			p.Messages = map[string]string{MessageUnavailable: strings.Repeat("m", MaxPersonaMessageRunes+1)}
		}), "definitions[0].messages.unavailable: persona_invalid: must be at most 500 characters"},
		{"message at the length limit", definitionConfig(func(p *Persona) {
			p.Messages = map[string]string{MessageUnavailable: strings.Repeat("m", MaxPersonaMessageRunes)}
		}), ""},
		{"pairing message without the placeholder", definitionConfig(func(p *Persona) { p.Messages = map[string]string{MessagePairingCode: "here is your code"} }), "definitions[0].messages.pairing_code: persona_invalid: must contain {code}"},
		{"second definition is reported with its index", Config{Definitions: []Persona{{ID: "a"}, {ID: "b", Audience: "everyone"}}}, "definitions[1].audience: persona_invalid: must be \"owner\", \"trusted\" or \"public\""},
		{"a name problem and an access problem are both reported", Config{Definitions: []Persona{{ID: "a", Name: "user"}, {ID: "b", Audience: "everyone"}}}, "definitions[0].name: persona_invalid: must not be \"user\"\ndefinitions[1].audience: persona_invalid: must be \"owner\", \"trusted\" or \"public\""},
		{"each check stops at the first problem of a definition", definitionConfig(func(p *Persona) { p.Audience, p.Pairing, p.Plugins = "everyone", true, []string{"Bad"} }), "definitions[0].audience: persona_invalid: must be \"owner\", \"trusted\" or \"public\""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != test.want {
				t.Fatalf("Validate() = %q, want %q", got, test.want)
			}
			if strings.Contains(test.want, "persona_invalid:") && !fault.HasCode(err, ErrorCodePersonaInvalid) {
				t.Fatalf("Validate() = %v, want the code %s", err, ErrorCodePersonaInvalid)
			}
			if strings.Contains(test.want, "config_invalid:") && !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Fatalf("Validate() = %v, want the code %s", err, configkit.ErrorCodeInvalid)
			}
		})
	}
}

func TestValidateKnowledgeTools(t *testing.T) {
	catalog := []auratools.Definition{
		{Name: "read_file", Version: auratools.Version},
		{Name: "web_fetch", Version: auratools.Version, ExternalSink: true},
	}
	for _, test := range []struct {
		name   string
		config Config
		want   string
	}{
		{"principal tool that sends data out beside principal knowledge", definitionConfig(func(p *Persona) {
			p.Knowledge, p.PrincipalKnowledge, p.PrincipalTools = []string{"docs"}, []string{"docs"}, []string{"web_fetch"}
		}), "definitions[0].principal_tools: persona_invalid: lists \"web_fetch\", which sends data out, while agents.definitions[0].principal_knowledge is not empty"},
		{"public tool that is not a known tool beside public knowledge", definitionConfig(func(p *Persona) {
			p.Knowledge, p.PublicKnowledge, p.PublicTools = []string{"docs"}, []string{"docs"}, []string{"ghost_tool"}
		}), "definitions[0].public_tools: persona_invalid: lists \"ghost_tool\", which is not a known tool or sends data out, while agents.definitions[0].public_knowledge is not empty"},
		{"a local tool beside principal knowledge", definitionConfig(func(p *Persona) {
			p.Knowledge, p.PrincipalKnowledge, p.PrincipalTools = []string{"docs"}, []string{"docs"}, []string{"read_file"}
		}), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateKnowledgeTools(&test.config, catalog)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != test.want {
				t.Fatalf("ValidateKnowledgeTools() = %q, want %q", got, test.want)
			}
			if test.want != "" && !fault.HasCode(err, ErrorCodePersonaInvalid) {
				t.Fatalf("ValidateKnowledgeTools() = %v, want %s", err, ErrorCodePersonaInvalid)
			}
		})
	}
}

func TestBindingsValidate(t *testing.T) {
	discord := Binding{Persona: "desk", Channel: BindingChannelDiscord, Account: "main", GuildID: "123"}
	telegram := Binding{Persona: "desk", Channel: BindingChannelTelegram, Account: "main", ChatID: "-100"}
	with := func(base Binding, edit func(*Binding)) Binding {
		edit(&base)
		return base
	}
	for _, test := range []struct {
		name     string
		bindings Bindings
		want     string
	}{
		{"none is valid", nil, ""},
		{"a discord guild binding is valid", Bindings{discord}, ""},
		{"a discord binding with every match field is valid", Bindings{with(discord, func(b *Binding) { b.ChannelID, b.ThreadID, b.UserID = "2", "3", "4" })}, ""},
		{"a telegram chat binding is valid", Bindings{telegram}, ""},
		{"a telegram topic binding is valid", Bindings{with(telegram, func(b *Binding) { b.ThreadID = "7" })}, ""},
		{"a telegram user binding is valid", Bindings{{Persona: "desk", Channel: BindingChannelTelegram, Account: "main", UserID: "42"}}, ""},
		{"an empty persona", Bindings{with(discord, func(b *Binding) { b.Persona = " " })}, "[0].persona: binding_invalid: must not be empty"},
		{"an unsupported channel", Bindings{with(discord, func(b *Binding) { b.Channel = BindingChannelEmail })}, "[0].channel: binding_invalid: \"email\" is not supported; use \"discord\" or \"telegram\""},
		{"an empty channel", Bindings{with(discord, func(b *Binding) { b.Channel = "" })}, "[0].channel: binding_invalid: \"\" is not supported; use \"discord\" or \"telegram\""},
		{"a discord binding with a chat id", Bindings{with(discord, func(b *Binding) { b.ChatID = "-1" })}, "[0].chat_id: binding_invalid: is only valid on telegram bindings"},
		{"a discord binding without match fields", Bindings{with(discord, func(b *Binding) { b.GuildID = "" })}, "[0]: binding_invalid: must set at least one of guild_id, channel_id, thread_id, user_id"},
		{"a discord guild id that is not a snowflake", Bindings{with(discord, func(b *Binding) { b.GuildID = "12a" })}, "[0].guild_id: binding_invalid: \"12a\" is not a decimal snowflake"},
		{"a discord channel id that is not a snowflake", Bindings{with(discord, func(b *Binding) { b.ChannelID = "-5" })}, "[0].channel_id: binding_invalid: \"-5\" is not a decimal snowflake"},
		{"a discord thread id that is not a snowflake", Bindings{with(discord, func(b *Binding) { b.ThreadID = "x" })}, "[0].thread_id: binding_invalid: \"x\" is not a decimal snowflake"},
		{"a discord user id that is not a snowflake", Bindings{with(discord, func(b *Binding) { b.UserID = "1.5" })}, "[0].user_id: binding_invalid: \"1.5\" is not a decimal snowflake"},
		{"a telegram binding with a guild id", Bindings{with(telegram, func(b *Binding) { b.GuildID = "1" })}, "[0]: binding_invalid: matches telegram chats by chat_id, thread_id and user_id only"},
		{"a telegram binding with a channel id", Bindings{with(telegram, func(b *Binding) { b.ChannelID = "1" })}, "[0]: binding_invalid: matches telegram chats by chat_id, thread_id and user_id only"},
		{"a telegram binding without a chat id or a user id", Bindings{with(telegram, func(b *Binding) { b.ChatID = "" })}, "[0]: binding_invalid: must set chat_id or user_id"},
		{"a telegram chat id that is not a group chat id", Bindings{with(telegram, func(b *Binding) { b.ChatID = "100" })}, "[0].chat_id: binding_invalid: \"100\" is not a negative decimal telegram group chat id"},
		{"a telegram thread id without a chat id", Bindings{{Persona: "desk", Channel: BindingChannelTelegram, Account: "main", UserID: "42", ThreadID: "7"}}, "[0].thread_id: binding_invalid: requires chat_id"},
		{"a telegram thread id that is not a topic id", Bindings{with(telegram, func(b *Binding) { b.ThreadID = "0" })}, "[0].thread_id: binding_invalid: \"0\" is not a positive decimal telegram topic id"},
		{"a telegram user id that is not a user id", Bindings{{Persona: "desk", Channel: BindingChannelTelegram, Account: "main", UserID: "-42"}}, "[0].user_id: binding_invalid: \"-42\" is not a positive decimal telegram user id"},
		{"two bindings with the same match fields", Bindings{discord, with(discord, func(b *Binding) { b.Persona = "other" })}, "[1]: binding_invalid: has the same match fields as bindings[0]"},
		{"the same match fields on another account are distinct", Bindings{discord, with(discord, func(b *Binding) { b.Account = "second" })}, ""},
		{"the same match fields on another channel are distinct", Bindings{with(telegram, func(b *Binding) { b.UserID = "9" }), {Persona: "desk", Channel: BindingChannelDiscord, Account: "main", UserID: "9"}}, ""},
		{"the second binding is reported with its index", Bindings{discord, with(telegram, func(b *Binding) { b.ChatID = "5" })}, "[1].chat_id: binding_invalid: \"5\" is not a negative decimal telegram group chat id"},
		{"every problem of a binding is reported", Bindings{{Persona: "", Channel: BindingChannelDiscord, Account: "main", GuildID: "x", ChatID: "-1"}}, "[0].persona: binding_invalid: must not be empty\n[0].chat_id: binding_invalid: is only valid on telegram bindings\n[0].guild_id: binding_invalid: \"x\" is not a decimal snowflake"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateBindings(test.bindings)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != test.want {
				t.Fatalf("ValidateBindings() = %q, want %q", got, test.want)
			}
			if test.want != "" && !fault.HasCode(err, ErrorCodeBindingInvalid) {
				t.Fatalf("ValidateBindings() = %v, want the code %s", err, ErrorCodeBindingInvalid)
			}
		})
	}
}
