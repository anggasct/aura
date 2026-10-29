package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/secret"
)

const MaxSoulBytes = 32 << 10

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
		// The detail names the persona only: the soul text and its path may echo the secret.
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
