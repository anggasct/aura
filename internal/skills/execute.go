package skills

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type Snapshot struct {
	Digest string
	Files  []FileEntry
}

type ScriptCall struct {
	ToolName   string
	Capability string
	Script     string
	Args       []string
	Approval   any
}

type ScriptResult struct {
	Output    []byte
	Truncated bool
}

type ScriptBroker interface {
	RunScriptTool(ctx context.Context, call *ScriptCall) (ScriptResult, error)
}

func (e *Engine) ReadResource(ctx context.Context, activation *Activation, relPath string) ([]byte, error) {
	if ctx == nil {
		return nil, errNilArgument("ctx")
	}
	loaded, err := e.applyActivation(activation)
	if err != nil {
		return nil, err
	}
	entry, err := snapshotEntry(activation.Snapshot, relPath)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(entry.Path, "scripts/") {
		return nil, Errorf(ErrorCodeSkillUnavailable, "skill scripts execute only through the broker")
	}
	content, err := e.readVerified(loaded, entry)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > e.maxResource {
		return nil, Errorf(ErrorCodeSkillInvalid, "skill resource exceeds the resource bound")
	}
	e.logger.DebugContext(ctx, "skill resource read",
		"component", "skills",
		"skill_id", activation.SkillID,
		"path", entry.Path,
		"bytes", len(content))
	return content, nil
}

func (e *Engine) RunScript(ctx context.Context, broker ScriptBroker, activation *Activation, relPath string, args []string, stagingDir string, approval any) (ScriptResult, error) {
	if ctx == nil {
		return ScriptResult{}, errNilArgument("ctx")
	}
	if broker == nil {
		return ScriptResult{}, errNilArgument("broker")
	}
	loaded, err := e.applyActivation(activation)
	if err != nil {
		return ScriptResult{}, err
	}
	entry, err := snapshotEntry(activation.Snapshot, relPath)
	if err != nil {
		return ScriptResult{}, err
	}
	if !strings.HasPrefix(entry.Path, "scripts/") {
		return ScriptResult{}, Errorf(ErrorCodeSkillUnavailable, "only bundled scripts execute through the broker")
	}
	if !slicesContains(activation.Grants, e.scriptCapability) {
		e.logger.InfoContext(ctx, "skill script denied",
			"component", "skills",
			"skill_id", activation.SkillID,
			"capability", e.scriptCapability)
		return ScriptResult{}, Errorf(ErrorCodeSkillUnavailable, "skill grant does not cover script execution")
	}
	if entry.Size > e.maxResource {
		return ScriptResult{}, Errorf(ErrorCodeSkillInvalid, "skill script exceeds the resource bound")
	}
	content, err := e.readVerified(loaded, entry)
	if err != nil {
		return ScriptResult{}, err
	}
	staged, err := stageScript(stagingDir, activation.Digest, path.Base(entry.Path), content)
	if err != nil {
		return ScriptResult{}, err
	}
	e.logger.InfoContext(ctx, "skill script started",
		"component", "skills",
		"skill_id", activation.SkillID,
		"capability", e.scriptCapability,
		"args", len(args))
	result, err := broker.RunScriptTool(ctx, &ScriptCall{
		ToolName:   e.scriptTool,
		Capability: e.scriptCapability,
		Script:     staged,
		Args:       append([]string(nil), args...),
		Approval:   approval,
	})
	if err != nil {
		return ScriptResult{}, codedError(ErrorCodeSkillUnavailable, "skill script failed", err)
	}
	return result, nil
}

func (e *Engine) applyActivation(activation *Activation) (*loadedPackage, error) {
	if activation == nil {
		return nil, errNilArgument("activation")
	}
	if strings.TrimSpace(activation.SkillID) == "" || strings.TrimSpace(activation.Digest) == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "activation must pin skill id and digest")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	loaded, ok := e.entries[activation.SkillID]
	if !ok || loaded.record.Digest != activation.Digest {
		return nil, Errorf(ErrorCodeSkillUnavailable, "skill activation is not current")
	}
	return loaded, nil
}

func snapshotEntry(snapshot Snapshot, relPath string) (FileEntry, error) {
	if strings.TrimSpace(relPath) == "" {
		return FileEntry{}, Errorf(ErrorCodeInvalidArgument, "resource path must not be empty")
	}
	cleaned := path.Clean(relPath)
	if path.IsAbs(relPath) || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != relPath {
		return FileEntry{}, Errorf(ErrorCodeSkillInvalid, "resource path escapes the skill root")
	}
	for _, entry := range snapshot.Files {
		if entry.Path == cleaned {
			return entry, nil
		}
	}
	return FileEntry{}, Errorf(ErrorCodeSkillUnavailable, "skill resource is not available")
}

func (e *Engine) readVerified(loaded *loadedPackage, entry FileEntry) ([]byte, error) {
	origin, err := parseLocalOrigin(loaded.record.Origin)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(origin.Root)
	if err != nil {
		return nil, Errorf(ErrorCodeSkillUnavailable, "skill content is not accessible")
	}
	defer func() { _ = root.Close() }()
	handle, err := root.Open(entry.Path)
	if err != nil {
		return nil, Errorf(ErrorCodeSkillUnavailable, "skill content is not accessible")
	}
	content, err := io.ReadAll(io.LimitReader(handle, maxSkillFileBytes+1))
	_ = handle.Close()
	if err != nil {
		return nil, Errorf(ErrorCodeSkillUnavailable, "skill content is not accessible")
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != entry.Digest {
		return nil, Errorf(ErrorCodeSkillDigestChanged, "skill content changed during activation")
	}
	return content, nil
}

func stageScript(stagingDir, digest, base string, content []byte) (string, error) {
	if !filepath.IsAbs(stagingDir) || filepath.Clean(stagingDir) != stagingDir {
		return "", Errorf(ErrorCodeInvalidArgument, "staging directory must be absolute and clean")
	}
	info, err := os.Stat(stagingDir)
	if err != nil || !info.IsDir() {
		return "", Errorf(ErrorCodeInvalidArgument, "staging directory is not available")
	}
	name := digest[:12] + "-" + rand.Text()[:6] + "-" + base
	staged := filepath.Join(stagingDir, name)
	tmp, err := os.CreateTemp(stagingDir, ".stage-*")
	if err != nil {
		return "", codedError(ErrorCodeSkillUnavailable, "stage skill script", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", codedError(ErrorCodeSkillUnavailable, "stage skill script", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", codedError(ErrorCodeSkillUnavailable, "stage skill script", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", codedError(ErrorCodeSkillUnavailable, "stage skill script", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return "", codedError(ErrorCodeSkillUnavailable, "stage skill script", err)
	}
	if err := os.Rename(tmpName, staged); err != nil {
		_ = os.Remove(tmpName)
		return "", codedError(ErrorCodeSkillUnavailable, "stage skill script", err)
	}
	return staged, nil
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
