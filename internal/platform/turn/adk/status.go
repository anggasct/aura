package runtimeadk

import "strings"

const statusProtocolSuffix = "Before calling a tool, first write one short status line of at most six words in the user's language describing the activity in human words, without naming tools, arguments, or secrets."

func statusProtocolLine() string {
	return statusProtocolSuffix
}

func appendStatusProtocol(instruction string) string {
	trimmed := strings.TrimSpace(instruction)
	if trimmed == "" {
		return statusProtocolSuffix
	}
	return trimmed + "\n\n" + statusProtocolSuffix
}
