package model

import (
	"encoding/json"
	"testing"
)

func TestToolCall_ArgumentsRoundTrip(t *testing.T) {
	tc := ToolCall{ID: "call_1", Name: "search", Arguments: json.RawMessage(`{"q":"go"}`)}
	data, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ToolCall
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "call_1" || got.Name != "search" || string(got.Arguments) != `{"q":"go"}` {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestUsage_ZeroValueDefaultsToZero(t *testing.T) {
	var u Usage
	if u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("zero-value Usage should be zero, got %+v", u)
	}
}
