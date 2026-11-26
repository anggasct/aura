package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func ValidateHTTP(http *HTTP) error {
	var problems []error
	if err := configkit.ValidateListenAddress("channels.http.listen_address", http.ListenAddress); err != nil {
		problems = append(problems, err)
	}
	if http.MaxBodyBytes < 1 || http.MaxBodyBytes > HTTPMaxBodyCeiling {
		problems = append(problems, configkit.Invalid("channels.http.max_body_bytes", fmt.Sprintf("must be between 1 and %d", HTTPMaxBodyCeiling)))
	}
	if http.RequestsPerMinute < 1 || http.RequestsPerMinute > HTTPRequestsPerMinuteCeiling {
		problems = append(problems, configkit.Invalid("channels.http.requests_per_minute", fmt.Sprintf("must be between 1 and %d", HTTPRequestsPerMinuteCeiling)))
	}
	if http.MaxConcurrentPerKey < 1 || http.MaxConcurrentPerKey > HTTPMaxConcurrentPerKeyLimit {
		problems = append(problems, configkit.Invalid("channels.http.max_concurrent_per_key", fmt.Sprintf("must be between 1 and %d", HTTPMaxConcurrentPerKeyLimit)))
	}
	if http.MaxHistoryMessages < 0 || http.MaxHistoryMessages > HTTPMaxHistoryMessagesLimit {
		problems = append(problems, configkit.Invalid("channels.http.max_history_messages", fmt.Sprintf("must be between 0 and %d", HTTPMaxHistoryMessagesLimit)))
	}
	if http.MaxHistoryBytes < 0 || http.MaxHistoryBytes > HTTPMaxHistoryBytesLimit {
		problems = append(problems, configkit.Invalid("channels.http.max_history_bytes", fmt.Sprintf("must be between 0 and %d", HTTPMaxHistoryBytesLimit)))
	}
	if keepalive := time.Duration(http.KeepaliveInterval); keepalive < HTTPKeepaliveIntervalFloor || keepalive > HTTPKeepaliveIntervalCeiling {
		problems = append(problems, configkit.Invalid("channels.http.keepalive_interval", fmt.Sprintf("must be between %s and %s", HTTPKeepaliveIntervalFloor, HTTPKeepaliveIntervalCeiling)))
	}
	return errors.Join(problems...)
}

func ValidateTools(toolsConfig *Tools) error {
	if toolsConfig == nil {
		return nil
	}
	if strings.TrimSpace(toolsConfig.Workspace) == "" {
		return configkit.Invalid("tools.workspace", "must not be empty")
	}
	if !filepath.IsAbs(toolsConfig.Workspace) {
		return configkit.Invalid("tools.workspace", "must be an absolute path")
	}
	if toolsConfig.MaxInlineResultBytes <= 0 {
		return configkit.Invalid("tools.max_inline_result_bytes", "must be positive")
	}
	if err := configkit.Under("tools.exec", toolsConfig.Exec.Validate()); err != nil {
		return err
	}
	if err := configkit.Under("tools.fetch", toolsConfig.Fetch.Validate()); err != nil {
		return err
	}
	if err := configkit.Under("tools.github", toolsConfig.GitHub.Validate()); err != nil {
		return err
	}
	return configkit.Under("tools.web_search", toolsConfig.WebSearch.Validate())
}
