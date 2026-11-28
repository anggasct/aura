package workspace

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

type LegacySession struct {
	SessionID       string
	FirstTaintedSeq uint64
}

type LegacyEvent struct {
	Kind    string
	Payload []byte
}

type LegacyEventReader interface {
	ListSessionEventsFrom(ctx context.Context, sessionID string, fromSeq uint64) ([]LegacyEvent, error)
}

func DeriveLegacyWorkspace(ctx context.Context, db *sql.DB, events LegacyEventReader, workspaceRoot string, sessions []LegacySession) (int, error) {
	if db == nil || events == nil || len(sessions) == 0 {
		return 0, nil
	}
	store := NewTaintStore(db)
	recorded := 0
	for _, sess := range sessions {
		n, err := deriveLegacySessionWorkspace(ctx, store, events, workspaceRoot, sess)
		if err != nil {
			return recorded, err
		}
		recorded += n
	}
	return recorded, nil
}

func deriveLegacySessionWorkspace(ctx context.Context, store *TaintStore, events LegacyEventReader, workspaceRoot string, sess LegacySession) (int, error) {
	sessionEvents, err := events.ListSessionEventsFrom(ctx, sess.SessionID, sess.FirstTaintedSeq)
	if err != nil {
		return 0, fmt.Errorf("workspace: scan legacy session %s events: %w", sess.SessionID, err)
	}
	writePaths, sawExec := collectLegacyWorkspaceEffects(sessionEvents)

	recorded := 0
	if sawExec {
		created, err := store.RecordWorkspaceWideWithID(ctx, "wt-legacy-wide", "legacy", sess.SessionID)
		if err != nil {
			return recorded, err
		}
		if created {
			recorded++
		}
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return recorded, nil
	}
	for _, rel := range writePaths {
		digest, dev, ino, err := statAndHashWorkspaceFile(workspaceRoot, rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			continue
		}
		id := legacyFileRecordID(sess.SessionID, rel)
		created, err := store.RecordFileWithID(ctx, id, rel, digest, dev, ino, "legacy", sess.SessionID)
		if err != nil {
			return recorded, err
		}
		if created {
			recorded++
		}
	}
	return recorded, nil
}

func legacyFileRecordID(sessionID, path string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + path))
	return "wt-legacy-" + hex.EncodeToString(sum[:12])
}

type legacyPendingCall struct {
	name string
	path string
}

func collectLegacyWorkspaceEffects(events []LegacyEvent) (writePaths []string, sawExec bool) {
	seenPaths := map[string]bool{}
	addPath := func(rawPath string) {
		clean := normalizeRelativePath(rawPath)
		if ValidateRelative(clean) != nil || seenPaths[clean] {
			return
		}
		seenPaths[clean] = true
		writePaths = append(writePaths, clean)
	}

	pendingByID := map[string]legacyPendingCall{}
	var pendingWritesNoID []string
	pendingExecsNoID := 0

	for i := range events {
		ev := &events[i]
		if ev.Kind == "tool.completed" {
			name, path, ok := parseLegacyToolCompleted(ev.Payload)
			if !ok {
				continue
			}
			switch {
			case name == "write_file" && path != "":
				addPath(path)
			case isLegacyExecTool(name):
				sawExec = true
			}
			continue
		}
		var adkEv struct {
			Content *struct {
				Parts []struct {
					FunctionCall *struct {
						ID   string         `json:"id"`
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
					FunctionResponse *struct {
						ID       string         `json:"id"`
						Name     string         `json:"name"`
						Response map[string]any `json:"response"`
					} `json:"functionResponse"`
				} `json:"parts"`
			} `json:"content"`
		}
		if err := json.Unmarshal(ev.Payload, &adkEv); err != nil || adkEv.Content == nil {
			continue
		}
		for j := range adkEv.Content.Parts {
			part := &adkEv.Content.Parts[j]
			if call := part.FunctionCall; call != nil {
				switch {
				case call.Name == "write_file":
					if path, ok := call.Args["path"].(string); ok && strings.TrimSpace(path) != "" {
						if call.ID != "" {
							pendingByID[call.ID] = legacyPendingCall{name: call.Name, path: path}
						} else {
							pendingWritesNoID = append(pendingWritesNoID, path)
						}
					}
				case isLegacyExecTool(call.Name):
					if call.ID != "" {
						pendingByID[call.ID] = legacyPendingCall{name: call.Name}
					} else {
						pendingExecsNoID++
					}
				}
			}
			if resp := part.FunctionResponse; resp != nil {
				if !isOKLegacyToolResponse(resp.Response) {
					switch {
					case resp.ID != "":
						delete(pendingByID, resp.ID)
					case resp.Name == "write_file" && len(pendingWritesNoID) > 0:
						pendingWritesNoID = pendingWritesNoID[1:]
					case isLegacyExecTool(resp.Name) && pendingExecsNoID > 0:
						pendingExecsNoID--
					}
					continue
				}
				if resp.ID != "" {
					if pending, ok := pendingByID[resp.ID]; ok {
						delete(pendingByID, resp.ID)
						if pending.name == "write_file" {
							addPath(pending.path)
						} else if isLegacyExecTool(pending.name) {
							sawExec = true
						}
						continue
					}
				}
				if resp.Name == "write_file" {
					if path, ok := resp.Response["path"].(string); ok && strings.TrimSpace(path) != "" {
						addPath(path)
						if len(pendingWritesNoID) > 0 {
							pendingWritesNoID = pendingWritesNoID[1:]
						}
					} else if len(pendingWritesNoID) > 0 {
						addPath(pendingWritesNoID[0])
						pendingWritesNoID = pendingWritesNoID[1:]
					}
				} else if isLegacyExecTool(resp.Name) {
					sawExec = true
					if pendingExecsNoID > 0 {
						pendingExecsNoID--
					}
				}
			}
		}
	}
	return writePaths, sawExec
}

func isLegacyExecTool(name string) bool {
	return name == "exec" || strings.HasPrefix(name, "mcp_") || strings.HasPrefix(name, "plugin_")
}

func isOKLegacyToolResponse(response map[string]any) bool {
	if response == nil {
		return false
	}
	if errVal, exists := response["error"]; exists && errVal != nil && errVal != "" {
		return false
	}
	if status, ok := response["status"].(string); ok && status != "" && status != "ok" && status != "succeeded" {
		return false
	}
	return true
}

func parseLegacyToolCompleted(payload []byte) (name, path string, ok bool) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil || !isOKLegacyToolResponse(raw) {
		return "", "", false
	}
	name, _ = raw["tool_name"].(string)
	if name == "" {
		name, _ = raw["name"].(string)
	}
	path, _ = raw["path"].(string)
	return name, path, name != ""
}
