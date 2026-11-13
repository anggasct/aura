package eval

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/turn"
)

const SessionID = "eval-session"

type AbuseCase struct {
	Name    string
	Request approval.ToolRequest
}

func CheckDenied(ctx context.Context, broker turn.ToolBroker, req *approval.ToolRequest) error {
	decision, err := broker.Evaluate(ctx, req)
	if err == nil && decision.Outcome == approval.OutcomeAllow {
		return fmt.Errorf("tool %q with trust %q was allowed, want denied", req.ToolName, req.Trust)
	}
	return nil
}

type abuseFile struct {
	Name         string   `json:"name"`
	Tool         string   `json:"tool"`
	Trust        string   `json:"trust"`
	Capabilities []string `json:"capabilities"`
}

func LoadAbuseCases(dir string) ([]AbuseCase, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("eval: open abuse cases: %w", err)
	}
	defer func() { _ = root.Close() }()
	dirFile, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("eval: read abuse dir: %w", err)
	}
	entries, err := dirFile.ReadDir(-1)
	_ = dirFile.Close()
	if err != nil {
		return nil, fmt.Errorf("eval: read abuse dir: %w", err)
	}
	var out []AbuseCase
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := root.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("eval: read %s: %w", entry.Name(), err)
		}
		abuse, err := parseAbuseCase(entry.Name(), data)
		if err != nil {
			return nil, err
		}
		out = append(out, abuse)
	}
	return out, nil
}

//go:embed testdata/abuse/*.json
var abuseCorpus embed.FS

func AbuseCorpus() ([]AbuseCase, error) {
	entries, err := fs.ReadDir(abuseCorpus, "testdata/abuse")
	if err != nil {
		return nil, fmt.Errorf("eval: read abuse corpus: %w", err)
	}
	out := make([]AbuseCase, 0, len(entries))
	for _, entry := range entries {
		data, err := abuseCorpus.ReadFile("testdata/abuse/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("eval: read %s: %w", entry.Name(), err)
		}
		abuse, err := parseAbuseCase(entry.Name(), data)
		if err != nil {
			return nil, err
		}
		out = append(out, abuse)
	}
	return out, nil
}

func parseAbuseCase(fileName string, data []byte) (AbuseCase, error) {
	var af abuseFile
	if err := json.Unmarshal(data, &af); err != nil {
		return AbuseCase{}, fmt.Errorf("eval: parse %s: %w", fileName, err)
	}
	return AbuseCase{
		Name: af.Name,
		Request: approval.ToolRequest{
			RequestID:    "eval-" + strings.ReplaceAll(af.Name, " ", "-"),
			TurnID:       "eval-turn",
			SessionID:    SessionID,
			PrincipalID:  "eval",
			ToolName:     af.Tool,
			Trust:        approval.TrustLabel(af.Trust),
			Capabilities: af.Capabilities,
		},
	}, nil
}
