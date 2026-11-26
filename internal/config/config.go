package config

import (
	"time"

	"github.com/anggasct/aura/internal/modules/fetch"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/modules/shellexec"
	"github.com/anggasct/aura/internal/platform/configkit"
)

const (
	HTTPListenAddress            = "127.0.0.1:8284"
	HTTPMaxBodyBytes             = 1048576
	HTTPMaxBodyCeiling           = 10485760
	HTTPRequestsPerMinute        = 60
	HTTPRequestsPerMinuteCeiling = 6000
	HTTPMaxConcurrentPerKey      = 4
	HTTPMaxConcurrentPerKeyLimit = 64
	HTTPMaxHistoryMessages       = 20
	HTTPMaxHistoryMessagesLimit  = 100
	HTTPMaxHistoryBytes          = 32768
	HTTPMaxHistoryBytesLimit     = 262144
	HTTPKeepaliveInterval        = 15 * time.Second
	HTTPKeepaliveIntervalFloor   = time.Second
	HTTPKeepaliveIntervalCeiling = time.Minute
)

type HTTP struct {
	Enabled             bool               `json:"enabled"`
	ListenAddress       string             `json:"listen_address"`
	MaxBodyBytes        int64              `json:"max_body_bytes"`
	RequestsPerMinute   int                `json:"requests_per_minute"`
	MaxConcurrentPerKey int                `json:"max_concurrent_per_key"`
	MaxHistoryMessages  int                `json:"max_history_messages"`
	MaxHistoryBytes     int                `json:"max_history_bytes"`
	KeepaliveInterval   configkit.Duration `json:"keepalive_interval"`
}

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
