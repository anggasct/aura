package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

type Definition struct {
	ID              string
	Name            string
	Description     string
	Instructions    string
	SoulFile        string
	Tools           []string
	Capabilities    []string
	ModelRoute      string
	MemoryScope     string
	Limits          Limits
	Audience        string
	Pairing         bool
	PrincipalTools  []string
	PublicTools     []string
	ApprovalTimeout time.Duration
	Messages        map[string]string
	GroupContext    string
	EvalDir         string
	Workflows       []string
	Plugins         []string
	EvalBehavior    *EvalBehavior
}

const (
	DefaultEvalMinScore   = 0.8
	DefaultEvalJudgeRoute = "auxiliary"
)

type EvalBehavior struct {
	Required            bool
	MinScore            float64
	JudgeRoute          string
	Schedule            string
	EnforceOnRegression bool
}

func audienceRank(audience string) int {
	switch audience {
	case config.AudiencePublic:
		return 2
	case config.AudienceTrusted:
		return 1
	default:
		return 0
	}
}

func classRank(class principal.Class) int {
	switch class {
	case principal.ClassOwner:
		return -1
	case principal.ClassPublic:
		return 2
	default:
		return 1
	}
}

func (d *Definition) Admits(class principal.Class) bool {
	return classRank(class) <= audienceRank(d.Audience)
}

func (d *Definition) ToolCeiling(class principal.Class) (tools []string, restricted bool) {
	switch class {
	case principal.ClassOwner:
		return slices.Clone(d.Tools), false
	case principal.ClassPublic:
		return slices.Clone(d.PublicTools), true
	default:
		return slices.Clone(d.PrincipalTools), true
	}
}

func (d *Definition) ListsPlugin(plugin string) bool {
	return slices.Contains(d.Plugins, plugin)
}

func (d *Definition) Message(key string) string {
	if text, ok := d.Messages[key]; ok {
		return text
	}
	return config.PersonaMessageDefaults()[key]
}

const (
	MemoryScopePersona = "persona"
	MemoryScopeShared  = "shared"
)

type Limits struct {
	TurnTimeout time.Duration
}

type Fingerprint struct {
	AgentID     string
	SoulSHA256  string
	ToolsSHA256 string
	ModelRoute  string
}

func (d *Definition) Fingerprint() Fingerprint {
	tools := slices.Clone(d.Tools)
	slices.Sort(tools)
	toolsText := strings.Join(tools, "\n")
	if len(d.Plugins) > 0 {
		plugins := slices.Clone(d.Plugins)
		slices.Sort(plugins)
		toolsText += "\nplugins=" + strings.Join(plugins, "\n")
	}
	return Fingerprint{
		AgentID:     d.ID,
		SoulSHA256:  sha256Hex(d.Instructions),
		ToolsSHA256: sha256Hex(toolsText),
		ModelRoute:  d.ModelRoute,
	}
}

func (d *Definition) EvalDigest() string {
	sorted := func(values []string) string {
		out := slices.Clone(values)
		slices.Sort(out)
		return fmt.Sprintf("%q", out)
	}
	keys := slices.Sorted(maps.Keys(d.Messages))
	messages := make([]string, 0, len(keys))
	for _, key := range keys {
		messages = append(messages, fmt.Sprintf("%q=%q", key, d.Messages[key]))
	}
	canonical := strings.Join([]string{
		fmt.Sprintf("soul=%q", d.Instructions),
		"tools=" + sorted(d.Tools),
		"principal_tools=" + sorted(d.PrincipalTools),
		"public_tools=" + sorted(d.PublicTools),
		fmt.Sprintf("audience=%q", d.Audience),
		fmt.Sprintf("pairing=%t", d.Pairing),
		fmt.Sprintf("group_context=%q", d.GroupContext),
		fmt.Sprintf("model_route=%q", d.ModelRoute),
		"messages=" + strings.Join(messages, ","),
	}, "\n")
	if len(d.Workflows) > 0 {
		canonical += "\nworkflows=" + sorted(d.Workflows)
	}
	if len(d.Plugins) > 0 {
		canonical += "\nplugins=" + sorted(d.Plugins)
	}
	return sha256Hex(canonical)
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (d *Definition) key() string {
	return d.ID
}

func (d *Definition) clone() Definition {
	return Definition{
		ID:              d.ID,
		Name:            d.Name,
		Description:     d.Description,
		Instructions:    d.Instructions,
		SoulFile:        d.SoulFile,
		Tools:           append([]string(nil), d.Tools...),
		Capabilities:    append([]string(nil), d.Capabilities...),
		ModelRoute:      d.ModelRoute,
		MemoryScope:     d.MemoryScope,
		Limits:          d.Limits,
		Audience:        d.Audience,
		Pairing:         d.Pairing,
		PrincipalTools:  slices.Clone(d.PrincipalTools),
		PublicTools:     slices.Clone(d.PublicTools),
		ApprovalTimeout: d.ApprovalTimeout,
		Messages:        maps.Clone(d.Messages),
		GroupContext:    d.GroupContext,
		EvalDir:         d.EvalDir,
		Workflows:       slices.Clone(d.Workflows),
		Plugins:         slices.Clone(d.Plugins),
		EvalBehavior:    d.EvalBehavior.clone(),
	}
}

func (b *EvalBehavior) clone() *EvalBehavior {
	if b == nil {
		return nil
	}
	cloned := *b
	return &cloned
}

func (d *Definition) supports(required []string) bool {
	for _, need := range required {
		if !slices.Contains(d.Capabilities, need) {
			return false
		}
	}
	return true
}

func (d *Definition) unusedCapabilityCount(required []string) int {
	unused := 0
	for _, declared := range d.Capabilities {
		if !slices.Contains(required, declared) {
			unused++
		}
	}
	return unused
}
