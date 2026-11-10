package child

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	spawnToolName    = "spawn_child"
	spawnToolVersion = "v1"
)

type SpawnArgs struct {
	Task           string      `json:"task"`
	Capabilities   []string    `json:"capabilities"`
	BudgetTokens   int64       `json:"budget_tokens"`
	BudgetCost     int64       `json:"budget_cost"`
	BudgetTools    int         `json:"budget_tools"`
	TimeoutSeconds int64       `json:"timeout_seconds"`
	IdempotencyKey string      `json:"idempotency_key"`
	Background     bool        `json:"background"`
	References     []Reference `json:"references"`
}

func SpawnToolDefinition() (name, version string, schema []byte) {
	return spawnToolName, spawnToolVersion, []byte(`{
		"type": "object",
		"required": ["task", "idempotency_key"],
		"properties": {
			"task": {"type": "string", "maxLength": 8192},
			"idempotency_key": {"type": "string", "maxLength": 128},
			"capabilities": {"type": "array", "items": {"type": "string"}, "maxItems": 64},
			"references": {"type": "array", "items": {"type": "object"}, "maxItems": 32},
			"budget_tokens": {"type": "integer", "minimum": 0},
			"budget_cost": {"type": "integer", "minimum": 0},
			"budget_tools": {"type": "integer", "minimum": 0},
			"timeout_seconds": {"type": "integer", "minimum": 1, "maximum": 86400},
			"background": {"type": "boolean"}
		}
	}`)
}

func ParseSpawnArgs(raw []byte) (SpawnArgs, error) {
	if len(raw) == 0 {
		return SpawnArgs{}, Errorf(ErrorCodeChildInvalid, "spawn_child arguments are empty")
	}
	if len(raw) > 1<<20 {
		return SpawnArgs{}, Errorf(ErrorCodeChildInvalid, "spawn_child arguments exceed the bound")
	}
	var args SpawnArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return SpawnArgs{}, Errorf(ErrorCodeChildInvalid, "spawn_child arguments are not valid: %v", err)
	}
	if strings.TrimSpace(args.Task) == "" {
		return SpawnArgs{}, Errorf(ErrorCodeChildInvalid, "spawn_child task must not be empty")
	}
	if utf8.RuneCountInString(args.Task) > maxTaskChars {
		return SpawnArgs{}, Errorf(ErrorCodeChildInvalid, "spawn_child task length is out of range")
	}
	if strings.TrimSpace(args.IdempotencyKey) == "" {
		return SpawnArgs{}, Errorf(ErrorCodeChildInvalid, "spawn_child idempotency key must not be empty")
	}
	if len(args.Capabilities) > maxGrants {
		return SpawnArgs{}, Errorf(ErrorCodeChildInvalid, "spawn_child capabilities exceed the maximum")
	}
	for _, capability := range args.Capabilities {
		if strings.TrimSpace(capability) == "" {
			return SpawnArgs{}, Errorf(ErrorCodeChildInvalid, "spawn_child capability must not be empty")
		}
		if strings.EqualFold(strings.TrimSpace(capability), spawnToolName) {
			return SpawnArgs{}, Errorf(ErrorCodeChildSpawnDenied, "child catalogs cannot carry spawn_child")
		}
	}
	if len(args.References) > maxReferences {
		return SpawnArgs{}, Errorf(ErrorCodeChildInvalid, "spawn_child references exceed the maximum")
	}
	if args.BudgetTokens < 0 || args.BudgetCost < 0 || args.BudgetTools < 0 {
		return SpawnArgs{}, Errorf(ErrorCodeChildInvalid, "spawn_child budget must not be negative")
	}
	if args.TimeoutSeconds < 0 || args.TimeoutSeconds > 86400 {
		return SpawnArgs{}, Errorf(ErrorCodeChildInvalid, "spawn_child timeout is out of range")
	}
	return args, nil
}

func SpawnArgsErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	if code, ok := CodeOf(err); ok {
		return fmt.Sprintf("%s: %s", code, err.Error())
	}
	return err.Error()
}
