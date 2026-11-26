package config

import (
	"github.com/anggasct/aura/internal/modules/fetch"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/modules/shellexec"
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
