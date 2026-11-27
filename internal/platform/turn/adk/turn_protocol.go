package runtimeadk

import "strings"

const turnProtocol = `Turn rules:
- Before calling a tool, write one short status line of at most six words in the user's language that says what you are doing in plain words, without naming tools, arguments, or secrets.
- When you need a tool, call it in this same reply. Never end a reply by promising to do something later, such as "one moment, I will check"; either do it now or say what you could not do.
- If a tool returns the same error twice, do not call it again with the same approach: tell the user what you tried, why it failed, and what they can do next.
- Write every reply and status line in the language of the user's latest message.`

func appendTurnProtocol(instruction string) string {
	trimmed := strings.TrimSpace(instruction)
	if trimmed == "" {
		return turnProtocol
	}
	return trimmed + "\n\n" + turnProtocol
}
