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

	legacyconfig "github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret"
)

func validatePersonaAccess(cfg *Config) error {
	if cfg.Agents == nil {
		return nil
	}
	for i := range cfg.Agents.Definitions {
		definition := &cfg.Agents.Definitions[i]
		field := fmt.Sprintf("agents.definitions[%d]", i)
		switch definition.Audience {
		case "", legacyconfig.AudienceOwner, legacyconfig.AudienceTrusted, legacyconfig.AudiencePublic:
		default:
			return configkit.Field(field+".audience", fault.New(legacyconfig.ErrorCodePersonaInvalid, fmt.Sprintf("must be %q, %q or %q", legacyconfig.AudienceOwner, legacyconfig.AudienceTrusted, legacyconfig.AudiencePublic)))
		}
		if definition.Pairing && definition.Audience != legacyconfig.AudienceTrusted {
			return configkit.Field(field+".pairing", fault.New(legacyconfig.ErrorCodePersonaInvalid, "requires audience \"trusted\""))
		}
		switch definition.GroupContext {
		case "", legacyconfig.GroupContextPerPrincipal:
		case legacyconfig.GroupContextShared:
			if definition.Audience != legacyconfig.AudienceTrusted {
				return configkit.Field(field+".group_context", fault.New(legacyconfig.ErrorCodePersonaInvalid, "\"shared\" requires audience \"trusted\""))
			}
		default:
			return configkit.Field(field+".group_context", fault.New(legacyconfig.ErrorCodePersonaInvalid, fmt.Sprintf("must be %q or %q", legacyconfig.GroupContextPerPrincipal, legacyconfig.GroupContextShared)))
		}
		for _, list := range []struct {
			name  string
			tools []string
		}{{"principal_tools", definition.PrincipalTools}, {"public_tools", definition.PublicTools}} {
			sorted := slices.Clone(list.tools)
			slices.Sort(sorted)
			if len(slices.Compact(sorted)) != len(list.tools) {
				return configkit.Field(field+"."+list.name, fault.New(legacyconfig.ErrorCodePersonaInvalid, "must not list a tool twice"))
			}
		}
		for _, id := range definition.Workflows {
			if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) {
				return configkit.Field(field+".workflows", fault.New(legacyconfig.ErrorCodePersonaInvalid, "entries must be non-empty workflow ids"))
			}
		}
		if sorted := slices.Sorted(slices.Values(definition.Workflows)); len(slices.Compact(sorted)) != len(definition.Workflows) {
			return configkit.Field(field+".workflows", fault.New(legacyconfig.ErrorCodePersonaInvalid, "must not list a workflow twice"))
		}
		if err := legacyconfig.ValidatePluginList(field, definition.Plugins); err != nil {
			return err
		}
		if err := legacyconfig.ValidatePersonaKnowledge(field, definition); err != nil {
			return err
		}
		if timeout := time.Duration(definition.ApprovalTimeout); timeout != 0 && (timeout < legacyconfig.MinApprovalTimeout || timeout > legacyconfig.MaxApprovalTimeout) {
			return configkit.Invalid(field+".approval_timeout", "must be between 1m and 24h")
		}
		for _, key := range slices.Sorted(maps.Keys(definition.Messages)) {
			if _, known := legacyconfig.PersonaMessageDefaults()[key]; !known {
				return configkit.Field(field+".messages", fault.New(legacyconfig.ErrorCodePersonaInvalid, fmt.Sprintf("unknown key %q", key)))
			}
			value := definition.Messages[key]
			switch {
			case strings.TrimSpace(value) == "":
				return configkit.Field(field+".messages."+key, fault.New(legacyconfig.ErrorCodePersonaInvalid, "must not be empty"))
			case strings.ContainsFunc(value, unicode.IsControl):
				return configkit.Field(field+".messages."+key, fault.New(legacyconfig.ErrorCodePersonaInvalid, "must be a single line without control characters"))
			case utf8.RuneCountInString(value) > legacyconfig.MaxPersonaMessageRunes:
				return configkit.Field(field+".messages."+key, fault.New(legacyconfig.ErrorCodePersonaInvalid, fmt.Sprintf("must be at most %d characters", legacyconfig.MaxPersonaMessageRunes)))
			case key == legacyconfig.MessagePairingCode && !strings.Contains(value, legacyconfig.PairingCodePlaceholder):
				return configkit.Field(field+".messages."+key, fault.New(legacyconfig.ErrorCodePersonaInvalid, "must contain "+legacyconfig.PairingCodePlaceholder))
			}
		}
	}
	return nil
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
			return configkit.Field(field, fault.New(legacyconfig.ErrorCodePersonaInvalid, "must not have surrounding whitespace"))
		case strings.ContainsFunc(name, unicode.IsControl):
			return configkit.Field(field, fault.New(legacyconfig.ErrorCodePersonaInvalid, "must be a single line without control characters"))
		case utf8.RuneCountInString(name) > legacyconfig.MaxPersonaNameRunes:
			return configkit.Field(field, fault.New(legacyconfig.ErrorCodePersonaInvalid, fmt.Sprintf("must be at most %d characters", legacyconfig.MaxPersonaNameRunes)))
		case strings.EqualFold(name, "user"):
			return configkit.Field(field, fault.New(legacyconfig.ErrorCodePersonaInvalid, "must not be \"user\""))
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
			return configkit.Field(field, fault.New(legacyconfig.ErrorCodePersonaInvalid, "soul_file and instructions are mutually exclusive"))
		}
		soul, err := readSoul(soulPath(definition.SoulFile, configPath))
		if err != nil {
			return configkit.Field(field+".soul_file", fault.New(legacyconfig.ErrorCodePersonaInvalid, err.Error()))
		}
		if vault == nil {
			vault = configuredSecretVault(cfg)
		}
		if vault.Contains(soul) {
			return configkit.Field(field, fault.New(legacyconfig.ErrorCodePersonaSecret, fmt.Sprintf("persona %q soul must not repeat a configured secret value", definition.ID)))
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
	data, err := io.ReadAll(io.LimitReader(file, legacyconfig.MaxSoulBytes+1))
	if err != nil {
		return "", errors.New("soul file is not readable")
	}
	if len(data) > legacyconfig.MaxSoulBytes {
		return "", fmt.Errorf("soul file exceeds %d bytes", legacyconfig.MaxSoulBytes)
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
