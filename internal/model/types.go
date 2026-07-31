package model

import "encoding/json"

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type Usage struct {
	InputTokens  int64
	OutputTokens int64
}
