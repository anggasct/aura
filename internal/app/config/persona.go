package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret"
)

func validateAgents(section *auraagent.Config) error {
	if section == nil {
		return nil
	}
	return configkit.Under("agents", section.Validate())
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
			return configkit.Field(field, fault.New(auraagent.ErrorCodePersonaInvalid, "soul_file and instructions are mutually exclusive"))
		}
		soul, err := readSoul(soulPath(definition.SoulFile, configPath))
		if err != nil {
			return configkit.Field(field+".soul_file", fault.New(auraagent.ErrorCodePersonaInvalid, err.Error()))
		}
		if vault == nil {
			vault = configuredSecretVault(cfg)
		}
		if vault.Contains(soul) {
			return configkit.Field(field, fault.New(auraagent.ErrorCodePersonaSecret, fmt.Sprintf("persona %q soul must not repeat a configured secret value", definition.ID)))
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
	data, err := io.ReadAll(io.LimitReader(file, auraagent.MaxSoulBytes+1))
	if err != nil {
		return "", errors.New("soul file is not readable")
	}
	if len(data) > auraagent.MaxSoulBytes {
		return "", fmt.Errorf("soul file exceeds %d bytes", auraagent.MaxSoulBytes)
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
