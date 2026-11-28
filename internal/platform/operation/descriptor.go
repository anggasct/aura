package operation

import (
	"context"
	"encoding/json"
	"io"

	"github.com/anggasct/aura/internal/platform/principal"
)

type Exposure string

const (
	ExposureAgent Exposure = "agent"
	ExposureOwner Exposure = "owner"
	ExposureHost  Exposure = "host"
)

type Risk string

const (
	RiskRead        Risk = "read"
	RiskChange      Risk = "change"
	RiskTrustGrant  Risk = "trust_grant"
	RiskDestructive Risk = "destructive"
)

type Surface string

const (
	SurfaceCLI   Surface = "cli"
	SurfaceChat  Surface = "chat"
	SurfaceAgent Surface = "agent"
)

type CommitClass string

const (
	CommitTransactional      CommitClass = "transactional"
	CommitExternalIdempotent CommitClass = "external_idempotent"
	CommitExternal           CommitClass = "external"
	CommitNone               CommitClass = "none"
)

type Group struct {
	Name    string
	Summary string
}

type Catalog struct {
	Group       Group
	Descriptors []Descriptor
}

type Descriptor struct {
	ID                   string
	Summary              string
	Description          string
	Exposure             Exposure
	Risk                 Risk
	Commit               CommitClass
	FreshApproval        bool
	Sink                 bool
	UntrustedOutput      bool
	OwnerNotice          bool
	ModuleTool           string
	Surfaces             []Surface
	SurfaceReason        string
	Params               []Param
	Output               Output
	Display              []string
	RequiredCapabilities []string
	Needs                []string
	Interactive          bool
	Preview              bool
	Confirmation         OwnerConfirmation
	ReachesNetwork       bool
	Texts                []SurfaceText
}

type OwnerConfirmation string

const (
	ConfirmPreviewUntilApply OwnerConfirmation = "preview_until_apply"
	ConfirmRunDirectly       OwnerConfirmation = "run_directly"
)

type ParamKind string

const (
	KindString   ParamKind = "string"
	KindInteger  ParamKind = "integer"
	KindBoolean  ParamKind = "boolean"
	KindDuration ParamKind = "duration"
	KindEnum     ParamKind = "enum"
	KindStrings  ParamKind = "strings"
	KindJSON     ParamKind = "json"
	KindPath     ParamKind = "path"
)

type Param struct {
	Name      string
	Kind      ParamKind
	Summary   string
	Required  bool
	Position  int
	Rest      bool
	Enum      []EnumValue
	MinLength int
	MaxLength int
	Minimum   *int64
	Maximum   *int64
	Default   json.RawMessage
	Secret    bool
	Surfaces  []Surface
	Schema    json.RawMessage
	Prompt    string
	URLParam  string
	Texts     []SurfaceText
}

type SurfaceText struct {
	Surface Surface
	Text    string
}

type EnumValue struct {
	Value    string
	Surfaces []Surface
}

type OutputKind string

const (
	OutputRecord OutputKind = "record"
	OutputTable  OutputKind = "table"
	OutputText   OutputKind = "text"
)

type Output struct {
	Kind   OutputKind
	Fields []OutputField
}

type OutputField struct {
	Name     string
	Secret   bool
	Surfaces []Surface
}

type Caller struct {
	Surface     Surface
	PrincipalID string
	Class       principal.Class
	SessionID   string
	TurnID      string
	AgentID     string
	Channel     string
	Origin      string
	Direct      bool
	Tainted     bool
}

type Call struct {
	Caller         Caller
	Input          json.RawMessage
	IdempotencyKey string
	RequestDigest  string
	PreviewDigest  string
	Files          FileAuthority
	Terminal       *Terminal

	run *invocation
}

type Result struct {
	Data json.RawMessage
}

type Preview struct {
	Lines  []string
	Digest string
	Empty  bool
}

type Handler func(ctx context.Context, call *Call) (Result, error)

type Previewer func(ctx context.Context, call *Call) (Preview, error)

type RateLimit func(ctx context.Context, call *Call) error

type Binding struct {
	ID        string
	Handle    Handler
	Preview   Previewer
	RateLimit RateLimit
}

type Provider interface {
	Bindings() []Binding
}

type File interface {
	io.ReadCloser
	Name() string
	MediaType() string
	Size() int64
	Reference() string
}

type FileAuthority interface {
	Open(ctx context.Context, source string) (File, error)
}

type Terminal struct {
	In  io.Reader
	Out io.Writer
}
