package config

import (
	"path/filepath"
	"strings"

	"github.com/anggasct/aura/internal/modules/fetch"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/modules/shellexec"
	"github.com/anggasct/aura/internal/platform/configkit"
)

type Tools struct {
	Workspace            string           `json:"workspace"`
	MaxInlineResultBytes int64            `json:"max_inline_result_bytes"`
	Exec                 shellexec.Config `json:"exec"`
	Fetch                fetch.Config     `json:"fetch"`
	WebSearch            search.Config    `json:"web_search"`
	GitHub               github.Config    `json:"github,omitzero"`
}

func DefaultTools() *Tools {
	return &Tools{
		Workspace:            "/srv/aura/workspace",
		MaxInlineResultBytes: 65536,
		Exec:                 shellexec.Default(),
		Fetch:                fetch.Default(),
		WebSearch:            search.Default(),
	}
}

func (t *Tools) Validate() error {
	if t == nil {
		return nil
	}
	if strings.TrimSpace(t.Workspace) == "" {
		return configkit.Invalid("workspace", "must not be empty")
	}
	if !filepath.IsAbs(t.Workspace) {
		return configkit.Invalid("workspace", "must be an absolute path")
	}
	if t.MaxInlineResultBytes <= 0 {
		return configkit.Invalid("max_inline_result_bytes", "must be positive")
	}
	if err := configkit.Under("exec", t.Exec.Validate()); err != nil {
		return err
	}
	if err := configkit.Under("fetch", t.Fetch.Validate()); err != nil {
		return err
	}
	if err := configkit.Under("github", t.GitHub.Validate()); err != nil {
		return err
	}
	return configkit.Under("web_search", t.WebSearch.Validate())
}
