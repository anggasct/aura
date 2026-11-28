package agent

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	sectionKey  = "agents"
	bindingsKey = "bindings"
)

const (
	MaxSoulBytes            = 32 << 10
	MaxPersonaNameRunes     = 64
	MaxPersonaMessageRunes  = 500
	PairingCodePlaceholder  = "{code}"
	WorkflowGoalPlaceholder = "{goal}"
)

const (
	AudienceOwner   = string(principal.ClassOwner)
	AudienceTrusted = string(principal.ClassTrusted)
	AudiencePublic  = string(principal.ClassPublic)
)

const (
	GroupContextPerPrincipal = "per_principal"
	GroupContextShared       = "shared"
)

const (
	BindingChannelDiscord  = "discord"
	BindingChannelTelegram = "telegram"
	BindingChannelEmail    = "email"
)

const (
	KnowledgeCollectionIDPattern = `^[a-z][a-z0-9-]{1,47}$`
	MaxPersonaCollections        = 32
)

var (
	knowledgeCollectionID = regexp.MustCompile(KnowledgeCollectionIDPattern)
	pluginID              = regexp.MustCompile(`^[a-z][a-z0-9-]{1,47}$`)
)

type Config struct {
	Definitions []Persona `json:"definitions"`
}

type PersonaLimits struct {
	TurnTimeout configkit.Duration `json:"turn_timeout"`
}

type Persona struct {
	ID              string             `json:"id"`
	Name            string             `json:"name,omitempty"`
	Description     string             `json:"description"`
	Instructions    string             `json:"instructions"`
	SoulFile        string             `json:"soul_file"`
	Soul            string             `json:"-"`
	Tools           []string           `json:"tools"`
	Capabilities    []string           `json:"capabilities"`
	ModelRoute      string             `json:"model_route"`
	MemoryScope     string             `json:"memory_scope,omitempty"`
	Limits          PersonaLimits      `json:"limits"`
	Audience        string             `json:"audience,omitempty"`
	Pairing         bool               `json:"pairing,omitempty"`
	PrincipalTools  []string           `json:"principal_tools,omitempty"`
	PublicTools     []string           `json:"public_tools,omitempty"`
	ApprovalTimeout configkit.Duration `json:"approval_timeout,omitempty"`
	Messages        map[string]string  `json:"messages,omitempty"`
	GroupContext    string             `json:"group_context,omitempty"`
	Evals           PersonaEvals       `json:"evals,omitzero"`
	Workflows       []string           `json:"workflows,omitempty"`
	Plugins         []string           `json:"plugins,omitempty"`

	Knowledge          []string `json:"knowledge,omitempty"`
	PrincipalKnowledge []string `json:"principal_knowledge,omitempty"`
	PublicKnowledge    []string `json:"public_knowledge,omitempty"`
}

type PersonaEvals struct {
	Dir      string               `json:"dir,omitempty"`
	Behavior *PersonaEvalBehavior `json:"behavior,omitempty"`
}

type PersonaEvalBehavior struct {
	Required            bool    `json:"required,omitempty"`
	MinScore            float64 `json:"min_score,omitempty"`
	JudgeRoute          string  `json:"judge_route,omitempty"`
	Schedule            string  `json:"schedule,omitempty"`
	EnforceOnRegression bool    `json:"enforce_on_regression,omitempty"`
}

type Binding struct {
	Persona   string `json:"persona"`
	Channel   string `json:"channel"`
	Account   string `json:"account"`
	GuildID   string `json:"guild_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	ChatID    string `json:"chat_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
}

type Bindings []Binding

func Default() Config {
	return Config{}
}

func ValidKnowledgeCollectionID(id string) bool {
	return knowledgeCollectionID.MatchString(id)
}

func ValidPluginID(id string) bool {
	return pluginID.MatchString(id)
}

func (c Config) Validate() error {
	return errors.Join(c.validateNames(), c.validateAccess())
}

func (c Config) validateNames() error {
	for i := range c.Definitions {
		name := c.Definitions[i].Name
		if name == "" {
			continue
		}
		field := fmt.Sprintf("definitions[%d].name", i)
		switch {
		case strings.TrimSpace(name) != name:
			return configkit.Field(field, fault.New(ErrorCodePersonaInvalid, "must not have surrounding whitespace"))
		case strings.ContainsFunc(name, unicode.IsControl):
			return configkit.Field(field, fault.New(ErrorCodePersonaInvalid, "must be a single line without control characters"))
		case utf8.RuneCountInString(name) > MaxPersonaNameRunes:
			return configkit.Field(field, fault.New(ErrorCodePersonaInvalid, fmt.Sprintf("must be at most %d characters", MaxPersonaNameRunes)))
		case strings.EqualFold(name, "user"):
			return configkit.Field(field, fault.New(ErrorCodePersonaInvalid, "must not be \"user\""))
		}
	}
	return nil
}

func (c Config) validateAccess() error {
	for i := range c.Definitions {
		definition := &c.Definitions[i]
		field := fmt.Sprintf("definitions[%d]", i)
		switch definition.Audience {
		case "", AudienceOwner, AudienceTrusted, AudiencePublic:
		default:
			return configkit.Field(field+".audience", fault.New(ErrorCodePersonaInvalid, fmt.Sprintf("must be %q, %q or %q", AudienceOwner, AudienceTrusted, AudiencePublic)))
		}
		if definition.Pairing && definition.Audience != AudienceTrusted {
			return configkit.Field(field+".pairing", fault.New(ErrorCodePersonaInvalid, "requires audience \"trusted\""))
		}
		switch definition.GroupContext {
		case "", GroupContextPerPrincipal:
		case GroupContextShared:
			if definition.Audience != AudienceTrusted {
				return configkit.Field(field+".group_context", fault.New(ErrorCodePersonaInvalid, "\"shared\" requires audience \"trusted\""))
			}
		default:
			return configkit.Field(field+".group_context", fault.New(ErrorCodePersonaInvalid, fmt.Sprintf("must be %q or %q", GroupContextPerPrincipal, GroupContextShared)))
		}
		for _, list := range []struct {
			name  string
			tools []string
		}{{"principal_tools", definition.PrincipalTools}, {"public_tools", definition.PublicTools}} {
			sorted := slices.Clone(list.tools)
			slices.Sort(sorted)
			if len(slices.Compact(sorted)) != len(list.tools) {
				return configkit.Field(field+"."+list.name, fault.New(ErrorCodePersonaInvalid, "must not list a tool twice"))
			}
		}
		if path, detail := operationToolDefect(cmp.Or(definition.Audience, AudienceOwner), &toolLists{listed: definition.Tools, principal: definition.PrincipalTools, public: definition.PublicTools}); path != "" {
			return configkit.Field(field+"."+path, fault.New(ErrorCodePersonaInvalid, detail))
		}
		for _, id := range definition.Workflows {
			if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) {
				return configkit.Field(field+".workflows", fault.New(ErrorCodePersonaInvalid, "entries must be non-empty workflow ids"))
			}
		}
		if sorted := slices.Sorted(slices.Values(definition.Workflows)); len(slices.Compact(sorted)) != len(definition.Workflows) {
			return configkit.Field(field+".workflows", fault.New(ErrorCodePersonaInvalid, "must not list a workflow twice"))
		}
		if err := validatePluginList(field, definition.Plugins); err != nil {
			return err
		}
		if err := validateKnowledge(field, definition); err != nil {
			return err
		}
		if timeout := time.Duration(definition.ApprovalTimeout); timeout != 0 && (timeout < principal.MinApprovalTimeout || timeout > principal.MaxApprovalTimeout) {
			return configkit.Invalid(field+".approval_timeout", "must be between 1m and 24h")
		}
		if err := validateMessages(field, definition.Messages); err != nil {
			return err
		}
	}
	return nil
}

type toolLists struct {
	listed    []string
	principal []string
	public    []string
}

func operationToolDefect(audience string, lists *toolLists) (path, detail string) {
	switch {
	case slices.ContainsFunc(lists.principal, tools.IsOperationEntry):
		return "principal_tools", "operation tools are never available to principals other than the owner"
	case slices.ContainsFunc(lists.public, tools.IsOperationEntry):
		return "public_tools", "operation tools are never available to principals other than the owner"
	case audience != AudienceOwner && slices.ContainsFunc(lists.listed, tools.IsOperationEntry):
		return "tools", "operation tools need audience \"owner\""
	}
	return "", ""
}

func validateMessages(field string, messages map[string]string) error {
	defaults := messageBundles[DefaultMessageLanguage]
	for _, key := range slices.Sorted(maps.Keys(messages)) {
		if _, known := defaults[key]; !known {
			return configkit.Field(field+".messages", fault.New(ErrorCodePersonaInvalid, fmt.Sprintf("unknown key %q", key)))
		}
		if bundleOnlyMessages[key] {
			return configkit.Field(field+".messages."+key, fault.New(ErrorCodePersonaInvalid, "cannot be overridden by a persona"))
		}
		if err := validateMessageValue(key, messages[key]); err != nil {
			return configkit.Field(field+".messages."+key, err)
		}
	}
	return nil
}

func validateMessageValue(key, value string) error {
	switch {
	case strings.TrimSpace(value) == "":
		return fault.New(ErrorCodePersonaInvalid, "must not be empty")
	case strings.ContainsFunc(value, unicode.IsControl):
		return fault.New(ErrorCodePersonaInvalid, "must be a single line without control characters")
	case utf8.RuneCountInString(value) > MaxPersonaMessageRunes:
		return fault.New(ErrorCodePersonaInvalid, fmt.Sprintf("must be at most %d characters", MaxPersonaMessageRunes))
	default:
		return validateMessagePlaceholders(key, value)
	}
}

func validatePluginList(field string, plugins []string) error {
	seen := make(map[string]struct{}, len(plugins))
	for _, id := range plugins {
		if !ValidPluginID(id) {
			return configkit.Field(field+".plugins", fault.New(ErrorCodePersonaInvalid, "entries must be plugin ids"))
		}
		if _, duplicate := seen[id]; duplicate {
			return configkit.Field(field+".plugins", fault.New(ErrorCodePersonaInvalid, "must not list a plugin twice"))
		}
		seen[id] = struct{}{}
	}
	return nil
}

func validateKnowledge(field string, definition *Persona) error {
	lists := []struct {
		name        string
		collections []string
	}{
		{"knowledge", definition.Knowledge},
		{"principal_knowledge", definition.PrincipalKnowledge},
		{"public_knowledge", definition.PublicKnowledge},
	}
	for _, list := range lists {
		if len(list.collections) > MaxPersonaCollections {
			return configkit.Field(field+"."+list.name, fault.New(ErrorCodePersonaInvalid, fmt.Sprintf("must list at most %d collections", MaxPersonaCollections)))
		}
		seen := make(map[string]struct{}, len(list.collections))
		for _, id := range list.collections {
			if !ValidKnowledgeCollectionID(id) {
				return configkit.Field(field+"."+list.name, fault.New(ErrorCodePersonaInvalid, "entries must match "+KnowledgeCollectionIDPattern))
			}
			if _, duplicate := seen[id]; duplicate {
				return configkit.Field(field+"."+list.name, fault.New(ErrorCodePersonaInvalid, "must not list a collection twice"))
			}
			seen[id] = struct{}{}
		}
	}
	for _, list := range lists[1:] {
		for _, id := range list.collections {
			if !slices.Contains(definition.Knowledge, id) {
				return configkit.Field(field+"."+list.name, fault.New(ErrorCodePersonaInvalid, fmt.Sprintf("lists %q, which is not in %s.%s.knowledge", id, sectionKey, field)))
			}
		}
	}
	return nil
}

func ValidateKnowledgeTools(cfg *Config, catalog []tools.Definition) error {
	if cfg == nil {
		return nil
	}
	var problems []error
	for i := range cfg.Definitions {
		if err := rejectKnowledgeSinks(fmt.Sprintf("definitions[%d]", i), &cfg.Definitions[i], catalog); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func rejectKnowledgeSinks(field string, definition *Persona, catalog []tools.Definition) error {
	classes := []struct {
		toolsField     string
		toolList       []string
		knowledgeField string
		knowledge      []string
	}{
		{"principal_tools", definition.PrincipalTools, "principal_knowledge", definition.PrincipalKnowledge},
		{"public_tools", definition.PublicTools, "public_knowledge", definition.PublicKnowledge},
	}
	for _, class := range classes {
		if len(class.knowledge) == 0 {
			continue
		}
		for _, entry := range class.toolList {
			sink, known := isExternalSinkEntry(entry, catalog)
			if !sink {
				continue
			}
			reason := "which sends data out"
			if !known {
				reason = "which is not a known tool or sends data out"
			}
			return configkit.Field(field+"."+class.toolsField, fault.New(ErrorCodePersonaInvalid, fmt.Sprintf("lists %q, %s, while %s.%s.%s is not empty", entry, reason, sectionKey, field, class.knowledgeField)))
		}
	}
	return nil
}

func isExternalSinkEntry(entry string, catalog []tools.Definition) (sink, known bool) {
	if tools.IsMCPEntry(entry) || tools.IsPluginEntry(entry) {
		return true, true
	}
	for i := range catalog {
		if catalog[i].Name == entry {
			return catalog[i].IsExternalSink(), true
		}
	}
	return true, false
}

func ValidateBindings(bindings Bindings) error {
	var problems []error
	seen := make(map[Binding]int, len(bindings))
	for i := range bindings {
		binding := bindings[i]
		field := fmt.Sprintf("[%d]", i)
		if strings.TrimSpace(binding.Persona) == "" {
			problems = append(problems, configkit.Field(field+".persona", fault.New(ErrorCodeBindingInvalid, "must not be empty")))
		}
		switch binding.Channel {
		case BindingChannelDiscord:
			problems = append(problems, discordBindingProblems(field, &binding)...)
		case BindingChannelTelegram:
			problems = append(problems, telegramBindingProblems(field, &binding)...)
		default:
			problems = append(problems, configkit.Field(field+".channel", fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not supported; use %q or %q", binding.Channel, BindingChannelDiscord, BindingChannelTelegram))))
		}
		key := binding
		key.Persona = ""
		if first, ok := seen[key]; ok {
			problems = append(problems, configkit.Field(field, fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("has the same match fields as %s[%d]", bindingsKey, first))))
			continue
		}
		seen[key] = i
	}
	return errors.Join(problems...)
}

func discordBindingProblems(field string, binding *Binding) []error {
	var problems []error
	if binding.ChatID != "" {
		problems = append(problems, configkit.Field(field+".chat_id", fault.New(ErrorCodeBindingInvalid, "is only valid on telegram bindings")))
	}
	if binding.GuildID == "" && binding.ChannelID == "" && binding.ThreadID == "" && binding.UserID == "" {
		problems = append(problems, configkit.Field(field, fault.New(ErrorCodeBindingInvalid, "must set at least one of guild_id, channel_id, thread_id, user_id")))
	}
	for _, match := range []struct{ name, id string }{
		{"guild_id", binding.GuildID},
		{"channel_id", binding.ChannelID},
		{"thread_id", binding.ThreadID},
		{"user_id", binding.UserID},
	} {
		if match.id != "" && !principal.IsDecimalSnowflake(match.id) {
			problems = append(problems, configkit.Field(field+"."+match.name, fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a decimal snowflake", match.id))))
		}
	}
	return problems
}

func telegramBindingProblems(field string, binding *Binding) []error {
	var problems []error
	if binding.GuildID != "" || binding.ChannelID != "" {
		problems = append(problems, configkit.Field(field, fault.New(ErrorCodeBindingInvalid, "matches telegram chats by chat_id, thread_id and user_id only")))
	}
	if binding.ChatID == "" && binding.UserID == "" {
		problems = append(problems, configkit.Field(field, fault.New(ErrorCodeBindingInvalid, "must set chat_id or user_id")))
	}
	if binding.ChatID != "" && !principal.IsTelegramGroupChatID(binding.ChatID) {
		problems = append(problems, configkit.Field(field+".chat_id", fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a negative decimal telegram group chat id", binding.ChatID))))
	}
	if binding.ThreadID != "" {
		switch {
		case binding.ChatID == "":
			problems = append(problems, configkit.Field(field+".thread_id", fault.New(ErrorCodeBindingInvalid, "requires chat_id")))
		case !principal.IsTelegramTopicID(binding.ThreadID):
			problems = append(problems, configkit.Field(field+".thread_id", fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a positive decimal telegram topic id", binding.ThreadID))))
		}
	}
	if binding.UserID != "" && !principal.IsTelegramUserID(binding.UserID) {
		problems = append(problems, configkit.Field(field+".user_id", fault.New(ErrorCodeBindingInvalid, fmt.Sprintf("%q is not a positive decimal telegram user id", binding.UserID))))
	}
	return problems
}
