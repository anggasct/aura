package config

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/anggasct/aura/internal/platform/secret"
)

const MaxSoulBytes = 32 << 10

const MaxPersonaNameRunes = 64

const MaxPersonaMessageRunes = 500

const PairingCodePlaceholder = "{code}"

const (
	AudienceOwner   = "owner"
	AudienceTrusted = "trusted"
	AudiencePublic  = "public"
)

const (
	GroupContextPerPrincipal = "per_principal"
	GroupContextShared       = "shared"
)

const (
	MinApprovalTimeout       = time.Minute
	MaxApprovalTimeout       = 24 * time.Hour
	MaxStandingGrantTTL      = 720 * time.Hour
	MinRetention             = 24 * time.Hour
	MessagePairingCode       = "pairing_code"
	MessagePairingPending    = "pairing_pending"
	MessagePairingLocked     = "pairing_locked"
	MessageWaitingForOwner   = "waiting_for_owner"
	MessageApprovalTimeout   = "approval_timeout"
	MessageApprovalDenied    = "approval_denied"
	MessageBudgetExhausted   = "budget_exhausted"
	MessageRateLimited       = "rate_limited"
	MessageNotPermitted      = "not_permitted"
	MessageHandoffOpen       = "handoff_open"
	MessageHandoffClosed     = "handoff_closed"
	MessageForgetmeConfirm   = "forgetme_confirm"
	MessageForgetmeDone      = "forgetme_done"
	MessageUnavailable       = "unavailable"
	MessageWorkflowSucceeded = "workflow_succeeded"
	MessageWorkflowFailed    = "workflow_failed"
	MessageWorkflowCancelled = "workflow_cancelled"
	WorkflowGoalPlaceholder  = "{goal}"
)

var personaMessageDefaults = map[string]string{
	MessagePairingCode:       "To talk with me, ask the owner to approve this pairing code: " + PairingCodePlaceholder,
	MessagePairingPending:    "A pairing request is already pending. Please wait for the owner.",
	MessagePairingLocked:     "Too many pairing attempts. Please try again later.",
	MessageWaitingForOwner:   "One moment, I am checking with the owner.",
	MessageApprovalTimeout:   "The owner did not respond in time, so the request was cancelled. Please try again later.",
	MessageApprovalDenied:    "The owner declined this request.",
	MessageBudgetExhausted:   "Your daily quota is used up. Please try again tomorrow.",
	MessageRateLimited:       "You are sending messages too quickly. Please wait a moment.",
	MessageNotPermitted:      "That is not available here.",
	MessageHandoffOpen:       "I have passed this conversation to the owner, who will reply here.",
	MessageHandoffClosed:     "The owner has handed the conversation back to me.",
	MessageForgetmeConfirm:   "This erases your conversations and data with me. Send /forgetme confirm to proceed.",
	MessageForgetmeDone:      "Your data has been erased.",
	MessageUnavailable:       "This assistant is not available right now.",
	MessageWorkflowSucceeded: "Done: " + WorkflowGoalPlaceholder,
	MessageWorkflowFailed:    "Sorry, " + WorkflowGoalPlaceholder + " could not be completed.",
	MessageWorkflowCancelled: WorkflowGoalPlaceholder + " was cancelled.",
}

func PersonaMessageDefaults() map[string]string {
	return maps.Clone(personaMessageDefaults)
}

func validatePersonaAccess(cfg *Config) error {
	if cfg.Agents == nil {
		return nil
	}
	for i := range cfg.Agents.Definitions {
		definition := &cfg.Agents.Definitions[i]
		field := fmt.Sprintf("agents.definitions[%d]", i)
		switch definition.Audience {
		case "", AudienceOwner, AudienceTrusted, AudiencePublic:
		default:
			return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.audience must be %q, %q or %q", field, AudienceOwner, AudienceTrusted, AudiencePublic)}
		}
		if definition.Pairing && definition.Audience != AudienceTrusted {
			return &Error{Code: ErrorCodePersonaInvalid, Detail: field + ".pairing requires audience \"trusted\""}
		}
		switch definition.GroupContext {
		case "", GroupContextPerPrincipal:
		case GroupContextShared:
			if definition.Audience != AudienceTrusted {
				return &Error{Code: ErrorCodePersonaInvalid, Detail: field + ".group_context \"shared\" requires audience \"trusted\""}
			}
		default:
			return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.group_context must be %q or %q", field, GroupContextPerPrincipal, GroupContextShared)}
		}
		for _, list := range []struct {
			name  string
			tools []string
		}{{"principal_tools", definition.PrincipalTools}, {"public_tools", definition.PublicTools}} {
			sorted := slices.Clone(list.tools)
			slices.Sort(sorted)
			if len(slices.Compact(sorted)) != len(list.tools) {
				return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.%s must not list a tool twice", field, list.name)}
			}
		}
		for _, id := range definition.Workflows {
			if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) {
				return &Error{Code: ErrorCodePersonaInvalid, Detail: field + ".workflows entries must be non-empty workflow ids"}
			}
		}
		if sorted := slices.Sorted(slices.Values(definition.Workflows)); len(slices.Compact(sorted)) != len(definition.Workflows) {
			return &Error{Code: ErrorCodePersonaInvalid, Detail: field + ".workflows must not list a workflow twice"}
		}
		if err := validatePluginList(field, definition.Plugins); err != nil {
			return err
		}
		if err := validatePersonaKnowledge(field, definition); err != nil {
			return err
		}
		if timeout := time.Duration(definition.ApprovalTimeout); timeout != 0 && (timeout < MinApprovalTimeout || timeout > MaxApprovalTimeout) {
			return &Error{Code: ErrorCodeConfigInvalid, Detail: field + ".approval_timeout must be between 1m and 24h"}
		}
		for _, key := range slices.Sorted(maps.Keys(definition.Messages)) {
			if _, known := personaMessageDefaults[key]; !known {
				return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.messages: unknown key %q", field, key)}
			}
			value := definition.Messages[key]
			switch {
			case strings.TrimSpace(value) == "":
				return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.messages.%s must not be empty", field, key)}
			case strings.ContainsFunc(value, unicode.IsControl):
				return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.messages.%s must be a single line without control characters", field, key)}
			case utf8.RuneCountInString(value) > MaxPersonaMessageRunes:
				return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.messages.%s must be at most %d characters", field, key, MaxPersonaMessageRunes)}
			case key == MessagePairingCode && !strings.Contains(value, PairingCodePlaceholder):
				return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.messages.%s must contain %s", field, key, PairingCodePlaceholder)}
			}
		}
	}
	return nil
}

func validateAccess(access *Access) error {
	if access == nil {
		return nil
	}
	var problems []error
	if timeout := time.Duration(access.ApprovalTimeout); timeout < MinApprovalTimeout || timeout > MaxApprovalTimeout {
		problems = append(problems, errors.New("access.approval_timeout must be between 1m and 24h"))
	}
	if ttl := time.Duration(access.StandingGrantTTL); ttl <= 0 || ttl > MaxStandingGrantTTL {
		problems = append(problems, errors.New("access.standing_grant_ttl must be positive and at most 720h"))
	}
	if timeout := time.Duration(access.HandoffTimeout); timeout < MinApprovalTimeout || timeout > MaxStandingGrantTTL {
		problems = append(problems, errors.New("access.handoff_timeout must be between 1m and 720h"))
	}
	for _, retention := range []struct {
		class string
		ttl   Duration
	}{{AudienceTrusted, access.Retention.Trusted}, {AudiencePublic, access.Retention.Public}} {
		if time.Duration(retention.ttl) < MinRetention {
			problems = append(problems, fmt.Errorf("access.retention.%s must be at least 24h", retention.class))
		}
	}
	for _, budget := range []struct {
		class  string
		limits PrincipalBudget
	}{{AudienceTrusted, access.Budgets.Trusted}, {AudiencePublic, access.Budgets.Public}} {
		if budget.limits.MaxTokensPerDay < 1 {
			problems = append(problems, fmt.Errorf("access.budgets.%s.max_tokens_per_day must be at least 1", budget.class))
		}
		if budget.limits.MaxTurnsPerMinute < 1 {
			problems = append(problems, fmt.Errorf("access.budgets.%s.max_turns_per_minute must be at least 1", budget.class))
		}
	}
	if err := errors.Join(problems...); err != nil {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: err.Error()}
	}
	return nil
}

func validateAccessShapes(doc *yamlv3.Node) error {
	accessNode := mappingValue(doc, "access")
	if accessNode == nil {
		return nil
	}
	if accessNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("access must be a mapping at line %d", accessNode.Line)
	}
	for i := 0; i+1 < len(accessNode.Content); i += 2 {
		keyNode := accessNode.Content[i]
		valueNode := accessNode.Content[i+1]
		switch keyNode.Value {
		case "approval_timeout", "standing_grant_ttl", "handoff_timeout":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("access.%s must be a duration string at line %d", keyNode.Value, valueNode.Line)
			}
		case "budgets":
			if err := validateBudgetShapes(valueNode); err != nil {
				return err
			}
		case "retention":
			if err := validateRetentionShapes(valueNode); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRetentionShapes(retention *yamlv3.Node) error {
	if retention.Kind != yamlv3.MappingNode {
		return fmt.Errorf("access.retention must be a mapping at line %d", retention.Line)
	}
	for i := 0; i+1 < len(retention.Content); i += 2 {
		if value := retention.Content[i+1]; value.Kind != yamlv3.ScalarNode || value.Tag != "!!str" {
			return fmt.Errorf("access.retention.%s must be a duration string at line %d", retention.Content[i].Value, value.Line)
		}
	}
	return nil
}

func validateBudgetShapes(budgets *yamlv3.Node) error {
	if budgets.Kind != yamlv3.MappingNode {
		return fmt.Errorf("access.budgets must be a mapping at line %d", budgets.Line)
	}
	for i := 0; i+1 < len(budgets.Content); i += 2 {
		class, limits := budgets.Content[i].Value, budgets.Content[i+1]
		if limits.Kind != yamlv3.MappingNode {
			return fmt.Errorf("access.budgets.%s must be a mapping at line %d", class, limits.Line)
		}
		for j := 0; j+1 < len(limits.Content); j += 2 {
			if value := limits.Content[j+1]; value.Kind != yamlv3.ScalarNode || value.Tag != "!!int" {
				return fmt.Errorf("access.budgets.%s.%s must be an integer at line %d", class, limits.Content[j].Value, value.Line)
			}
		}
	}
	return nil
}

func applyAccessDefaults(cfg *Config, doc *yamlv3.Node) {
	defaults := *Default().Access
	if cfg.Access == nil {
		cfg.Access = &defaults
		return
	}
	if cfg.Access.ApprovalTimeout == 0 && !configValuePresent(doc, "access", "approval_timeout") && !envValuePresent("access.approval_timeout") {
		cfg.Access.ApprovalTimeout = defaults.ApprovalTimeout
	}
	if cfg.Access.StandingGrantTTL == 0 && !configValuePresent(doc, "access", "standing_grant_ttl") && !envValuePresent("access.standing_grant_ttl") {
		cfg.Access.StandingGrantTTL = defaults.StandingGrantTTL
	}
	if cfg.Access.HandoffTimeout == 0 && !configValuePresent(doc, "access", "handoff_timeout") && !envValuePresent("access.handoff_timeout") {
		cfg.Access.HandoffTimeout = defaults.HandoffTimeout
	}
	applyBudgetDefaults(&cfg.Access.Budgets.Trusted, defaults.Budgets.Trusted, doc, AudienceTrusted)
	applyBudgetDefaults(&cfg.Access.Budgets.Public, defaults.Budgets.Public, doc, AudiencePublic)
	if cfg.Access.Retention.Trusted == 0 && !configValuePresent(doc, "access", "retention", AudienceTrusted) && !envValuePresent("access.retention.trusted") {
		cfg.Access.Retention.Trusted = defaults.Retention.Trusted
	}
	if cfg.Access.Retention.Public == 0 && !configValuePresent(doc, "access", "retention", AudiencePublic) && !envValuePresent("access.retention.public") {
		cfg.Access.Retention.Public = defaults.Retention.Public
	}
}

func applyBudgetDefaults(budget *PrincipalBudget, defaults PrincipalBudget, doc *yamlv3.Node, class string) {
	if budget.MaxTokensPerDay == 0 && !configValuePresent(doc, "access", "budgets", class, "max_tokens_per_day") && !envValuePresent("access.budgets."+class+".max_tokens_per_day") {
		budget.MaxTokensPerDay = defaults.MaxTokensPerDay
	}
	if budget.MaxTurnsPerMinute == 0 && !configValuePresent(doc, "access", "budgets", class, "max_turns_per_minute") && !envValuePresent("access.budgets."+class+".max_turns_per_minute") {
		budget.MaxTurnsPerMinute = defaults.MaxTurnsPerMinute
	}
}

func validatePersonaNames(cfg *Config) error {
	if cfg.Agents == nil {
		return nil
	}
	for i := range cfg.Agents.Definitions {
		name := cfg.Agents.Definitions[i].Name
		if name == "" {
			continue
		}
		field := fmt.Sprintf("agents.definitions[%d].name", i)
		switch {
		case strings.TrimSpace(name) != name:
			return &Error{Code: ErrorCodePersonaInvalid, Detail: field + " must not have surrounding whitespace"}
		case strings.ContainsFunc(name, unicode.IsControl):
			return &Error{Code: ErrorCodePersonaInvalid, Detail: field + " must be a single line without control characters"}
		case utf8.RuneCountInString(name) > MaxPersonaNameRunes:
			return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s must be at most %d characters", field, MaxPersonaNameRunes)}
		case strings.EqualFold(name, "user"):
			return &Error{Code: ErrorCodePersonaInvalid, Detail: field + " must not be \"user\""}
		}
	}
	return nil
}

func loadPersonaSouls(cfg *Config, configPath string) error {
	if cfg.Agents == nil {
		return nil
	}
	var vault *secret.Vault
	for i := range cfg.Agents.Definitions {
		definition := &cfg.Agents.Definitions[i]
		if definition.SoulFile == "" {
			continue
		}
		field := fmt.Sprintf("agents.definitions[%d]", i)
		if strings.TrimSpace(definition.Instructions) != "" {
			return &Error{Code: ErrorCodePersonaInvalid, Detail: field + ": soul_file and instructions are mutually exclusive"}
		}
		soul, err := readSoul(soulPath(definition.SoulFile, configPath))
		if err != nil {
			return &Error{Code: ErrorCodePersonaInvalid, Detail: fmt.Sprintf("%s.soul_file: %v", field, err)}
		}
		if vault == nil {
			vault = configuredSecretVault(cfg)
		}
		if vault.Contains(soul) {
			return &Error{Code: ErrorCodePersonaSecret, Detail: fmt.Sprintf("persona %q soul must not repeat a configured secret value", definition.ID)}
		}
		definition.Soul = soul
	}
	return nil
}

func soulPath(soulFile, configPath string) string {
	if filepath.IsAbs(soulFile) || configPath == "" {
		return soulFile
	}
	return filepath.Join(filepath.Dir(configPath), soulFile)
}

func readSoul(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("soul file is not readable")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, MaxSoulBytes+1))
	if err != nil {
		return "", errors.New("soul file is not readable")
	}
	if len(data) > MaxSoulBytes {
		return "", fmt.Errorf("soul file exceeds %d bytes", MaxSoulBytes)
	}
	if !utf8.Valid(data) {
		return "", errors.New("soul file is not valid UTF-8")
	}
	soul := strings.TrimSpace(string(data))
	if soul == "" {
		return "", errors.New("soul file is empty")
	}
	return soul, nil
}
