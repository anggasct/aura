package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/tools"
)

const (
	APIVersion       = "aura.plugin/v1"
	ManifestFile     = "plugin.yaml"
	MaxManifestBytes = 256 << 10

	TierRead   = "read"
	TierEffect = "effect"

	BackendMCP = "mcp"

	AuthNone         = "none"
	AuthStaticSecret = "static_secret"

	maxNameRunes         = 64
	maxDescriptionBytes  = 1024
	maxCapabilities      = 16
	maxTools             = 64
	maxEgress            = 32
	maxSecrets           = 1
	maxArgs              = 64
	maxArgBytes          = 1024
	maxCommandBytes      = 512
	maxProblemsReported  = 8
	validationServerRoot = "/plugin"
)

var (
	capabilityPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)
	toolNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	hostPattern       = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	headerPattern     = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

type Manifest struct {
	APIVersion   string   `yaml:"api_version" json:"api_version"`
	ID           string   `yaml:"id" json:"id"`
	Version      string   `yaml:"version" json:"version"`
	Name         string   `yaml:"name" json:"name"`
	Description  string   `yaml:"description" json:"description"`
	Capabilities []string `yaml:"capabilities" json:"capabilities"`
	Tools        []Tool   `yaml:"tools" json:"tools"`
	Egress       []string `yaml:"egress" json:"egress"`
	Secrets      []Secret `yaml:"secrets" json:"secrets"`
	Auth         Auth     `yaml:"auth" json:"auth"`
	Backend      Backend  `yaml:"backend" json:"backend"`
}

type Tool struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Tier        string `yaml:"tier" json:"tier"`
}

type Secret struct {
	Name string `yaml:"name" json:"name"`
	Ref  string `yaml:"ref" json:"ref"`
}

type Auth struct {
	Kind   string `yaml:"kind" json:"kind"`
	Secret string `yaml:"secret" json:"secret"`
	Header string `yaml:"header" json:"header"`
}

type Backend struct {
	Kind           string   `yaml:"kind" json:"kind"`
	Transport      string   `yaml:"transport" json:"transport"`
	Command        string   `yaml:"command" json:"command"`
	Args           []string `yaml:"args" json:"args"`
	URL            string   `yaml:"url" json:"url"`
	StartupTimeout string   `yaml:"startup_timeout" json:"startup_timeout"`
	RequestTimeout string   `yaml:"request_timeout" json:"request_timeout"`
	ConnectTimeout string   `yaml:"connect_timeout" json:"connect_timeout"`
	MaxMessageSize string   `yaml:"max_message_size" json:"max_message_size"`
}

type backendProbe struct {
	Backend struct {
		Kind any `yaml:"kind"`
	} `yaml:"backend"`
}

func ParseManifest(data []byte) (*Manifest, error) {
	if len(data) > MaxManifestBytes {
		return nil, Errorf(ErrManifestTooLarge, "the manifest exceeds %d bytes", MaxManifestBytes)
	}
	var probe backendProbe
	if err := yamlv3.Unmarshal(data, &probe); err != nil {
		return nil, Errorf(ErrManifestInvalid, "the manifest is not valid yaml")
	}
	if kind, isString := probe.Backend.Kind.(string); isString && kind != "" && kind != BackendMCP {
		return nil, Errorf(ErrBackendUnsupported, "backend kind %q is not supported", boundedText(kind))
	}
	decoder := yamlv3.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, Errorf(ErrManifestInvalid, "the manifest does not match the plugin schema: %s", decodeProblem(err))
	}
	var extra yamlv3.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, Errorf(ErrManifestInvalid, "the manifest must hold exactly one document")
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

var yamlLinePattern = regexp.MustCompile(`^line (\d+):`)

func decodeProblem(err error) string {
	var typeErr *yamlv3.TypeError
	if !errors.As(err, &typeErr) {
		return "invalid structure"
	}
	lines := make([]string, 0, len(typeErr.Errors))
	for _, text := range typeErr.Errors {
		if match := yamlLinePattern.FindStringSubmatch(text); match != nil {
			lines = append(lines, "line "+match[1]+" has an unknown field or a value of the wrong type")
		}
	}
	if len(lines) == 0 {
		return "invalid structure"
	}
	if len(lines) > maxProblemsReported {
		lines = lines[:maxProblemsReported]
	}
	return strings.Join(lines, "; ")
}

func boundedText(text string) string {
	if len(text) > 32 {
		return text[:32]
	}
	return text
}

type problems []string

func (p *problems) add(format string, args ...any) {
	*p = append(*p, fmt.Sprintf(format, args...))
}

func (m *Manifest) Validate() error {
	var found problems
	if m.APIVersion != APIVersion {
		found.add("api_version must be %s", APIVersion)
	}
	if !config.ValidPluginID(m.ID) {
		found.add("id must match [a-z][a-z0-9-]{1,47}")
	}
	if !config.ValidPluginVersion(m.Version) {
		found.add("version must be a semantic version")
	}
	if err := validateText("name", m.Name, maxNameRunes, false); err != "" {
		found.add("%s", err)
	}
	if err := validateText("description", m.Description, maxDescriptionBytes, true); err != "" {
		found.add("%s", err)
	}
	m.validateCapabilities(&found)
	m.validateTools(&found)
	m.validateEgress(&found)
	m.validateSecretsAndAuth(&found)
	m.validateBackend(&found)
	if len(found) == 0 {
		return nil
	}
	reported := found
	suffix := ""
	if len(reported) > maxProblemsReported {
		reported = reported[:maxProblemsReported]
		suffix = "; " + strconv.Itoa(len(found)-maxProblemsReported) + " more"
	}
	return Errorf(ErrManifestInvalid, "%s%s", strings.Join(reported, "; "), suffix)
}

func validateText(field, text string, limit int, bytesLimit bool) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return field + " must not be empty"
	}
	if !utf8.ValidString(text) {
		return field + " must be valid utf-8"
	}
	size := utf8.RuneCountInString(text)
	if bytesLimit {
		size = len(text)
	}
	if size > limit {
		return field + " is too long"
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return field + " must not contain control characters"
		}
	}
	return ""
}

func (m *Manifest) validateCapabilities(found *problems) {
	if len(m.Capabilities) > maxCapabilities {
		found.add("capabilities lists more than %d entries", maxCapabilities)
	}
	seen := map[string]bool{}
	for _, capability := range m.Capabilities {
		if !capabilityPattern.MatchString(capability) {
			found.add("capabilities entries must match [a-z][a-z0-9_-]{0,47}")
			continue
		}
		if seen[capability] {
			found.add("capabilities lists an entry twice")
		}
		seen[capability] = true
	}
}

func (m *Manifest) validateTools(found *problems) {
	if len(m.Tools) == 0 {
		found.add("tools must declare at least one tool")
	}
	if len(m.Tools) > maxTools {
		found.add("tools lists more than %d entries", maxTools)
	}
	seen := map[string]bool{}
	for i := range m.Tools {
		tool := &m.Tools[i]
		field := "tools[" + strconv.Itoa(i) + "]"
		switch {
		case !toolNamePattern.MatchString(tool.Name):
			found.add("%s.name must match [A-Za-z0-9][A-Za-z0-9_-]{0,63}", field)
		case len(tools.PluginToolWireName(m.ID, tool.Name)) > tools.MaxPluginToolWireLen:
			found.add("%s.name makes the exposed tool name longer than %d characters", field, tools.MaxPluginToolWireLen)
		case seen[tool.Name]:
			found.add("%s.name repeats an earlier tool", field)
		}
		seen[tool.Name] = true
		if err := validateText(field+".description", tool.Description, maxDescriptionBytes, true); err != "" {
			found.add("%s", err)
		}
		if tool.Tier != TierRead && tool.Tier != TierEffect {
			found.add("%s.tier must be %s or %s", field, TierRead, TierEffect)
		}
	}
}

func validEgressEntry(entry string) bool {
	host, port, hasPort := strings.Cut(entry, ":")
	if !hostPattern.MatchString(host) || strings.Contains(host, "..") {
		return false
	}
	if !hasPort {
		return true
	}
	number, err := strconv.Atoi(port)
	return err == nil && number >= 1 && number <= 65535 && strconv.Itoa(number) == port
}

func (m *Manifest) validateEgress(found *problems) {
	if len(m.Egress) > maxEgress {
		found.add("egress lists more than %d entries", maxEgress)
	}
	seen := map[string]bool{}
	for _, entry := range m.Egress {
		if !validEgressEntry(entry) {
			found.add("egress entries must be a lowercase host or host:port without wildcards")
			continue
		}
		if seen[entry] {
			found.add("egress lists an entry twice")
		}
		seen[entry] = true
	}
}

func validSecretRef(ref string) bool {
	switch {
	case strings.HasPrefix(ref, "env://"):
		return envNamePattern.MatchString(strings.TrimPrefix(ref, "env://"))
	case strings.HasPrefix(ref, "file://"):
		path := strings.TrimPrefix(ref, "file://")
		return filepath.IsAbs(path) && len(path) <= maxCommandBytes && !strings.ContainsFunc(path, unicode.IsControl)
	default:
		return false
	}
}

func (m *Manifest) validateSecretsAndAuth(found *problems) {
	if len(m.Secrets) > maxSecrets {
		found.add("secrets lists more than %d entries", maxSecrets)
	}
	for i := range m.Secrets {
		secret := &m.Secrets[i]
		field := "secrets[" + strconv.Itoa(i) + "]"
		if !secretNamePattern.MatchString(secret.Name) {
			found.add("%s.name must match [A-Za-z_][A-Za-z0-9_]{0,63}", field)
		}
		if !validSecretRef(secret.Ref) {
			found.add("%s.ref must be an env:// name or an absolute file:// path, never a value", field)
		}
	}
	switch m.Auth.Kind {
	case AuthNone:
		if len(m.Secrets) > 0 || m.Auth.Secret != "" || m.Auth.Header != "" {
			found.add("auth.kind none takes no secret or header")
		}
	case AuthStaticSecret:
		if len(m.Secrets) != 1 || m.Auth.Secret != m.Secrets[0].Name {
			found.add("auth.secret must name the one declared secret")
		}
		if m.Auth.Header != "" && !headerPattern.MatchString(m.Auth.Header) {
			found.add("auth.header must be a header name")
		}
		if m.Backend.Transport != config.MCPTransportStreamableHTTP {
			found.add("auth.kind static_secret needs the streamable_http transport")
		}
	default:
		found.add("auth.kind must be %s or %s", AuthNone, AuthStaticSecret)
	}
}

func (m *Manifest) validateBackend(found *problems) {
	backend := &m.Backend
	if backend.Kind != BackendMCP {
		found.add("backend.kind must be %s", BackendMCP)
		return
	}
	var local problems
	if len(backend.Args) > maxArgs {
		local.add("backend.args lists more than %d entries", maxArgs)
	}
	for _, arg := range backend.Args {
		if len(arg) > maxArgBytes || !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			local.add("backend.args entries must be short text without NUL")
			break
		}
	}
	switch backend.Transport {
	case config.MCPTransportStdio:
		if !validBundleCommand(backend.Command) {
			local.add("backend.command must be a relative path inside the bundle")
		}
		if backend.URL != "" {
			local.add("backend.url is not allowed for the stdio transport")
		}
	case config.MCPTransportStreamableHTTP:
		m.validateBackendURL(&local)
	default:
		local.add("backend.transport must be %s or %s", config.MCPTransportStdio, config.MCPTransportStreamableHTTP)
	}
	for _, field := range []struct{ name, value string }{
		{"startup_timeout", backend.StartupTimeout}, {"request_timeout", backend.RequestTimeout}, {"connect_timeout", backend.ConnectTimeout},
	} {
		if !validDuration(field.value) {
			local.add("backend.%s must be a non-negative duration", field.name)
		}
	}
	if !validByteSize(backend.MaxMessageSize) {
		local.add("backend.max_message_size must be a byte size")
	}
	if len(local) == 0 {
		server := m.ServerConfig(validationServerRoot)
		if err := config.ValidateMCPServer(&server, "backend"); err != nil {
			local.add("backend does not describe a valid mcp server")
		}
	}
	*found = append(*found, local...)
}

func validBundleCommand(command string) bool {
	if command == "" || len(command) > maxCommandBytes || strings.ContainsFunc(command, unicode.IsControl) || strings.Contains(command, "\\") {
		return false
	}
	return filepath.IsLocal(command) && filepath.Clean(command) == strings.TrimPrefix(command, "./") && !strings.HasSuffix(command, "/")
}

func (m *Manifest) validateBackendURL(found *problems) {
	backend := &m.Backend
	if backend.Command != "" || len(backend.Args) > 0 {
		found.add("backend.command and backend.args are not allowed for the streamable_http transport")
	}
	parsed, err := url.Parse(backend.URL)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		found.add("backend.url must be an http(s) url without credentials")
		return
	}
	host := strings.ToLower(parsed.Host)
	if !slices.Contains(m.Egress, host) && !slices.Contains(m.Egress, strings.ToLower(parsed.Hostname())) {
		found.add("backend.url host must be listed in egress")
	}
}

func (m *Manifest) ServerConfig(bundleRoot string) config.MCPServer {
	backend := &m.Backend
	server := config.MCPServer{
		Name:           m.ID,
		Transport:      backend.Transport,
		Args:           slices.Clone(backend.Args),
		URL:            backend.URL,
		StartupTimeout: config.Duration(parseDuration(backend.StartupTimeout)),
		RequestTimeout: config.Duration(parseDuration(backend.RequestTimeout)),
		ConnectTimeout: config.Duration(parseDuration(backend.ConnectTimeout)),
		MaxMessageSize: parseByteSize(backend.MaxMessageSize),
	}
	if backend.Command != "" {
		server.Command = filepath.Join(bundleRoot, filepath.Clean(backend.Command))
	}
	capabilities := slices.Clone(m.Capabilities)
	slices.Sort(capabilities)
	server.Capabilities = capabilities
	if m.Auth.Kind == AuthStaticSecret && len(m.Secrets) == 1 {
		server.Auth = &config.MCPAuth{Static: &config.MCPStaticAuth{Header: m.Auth.Header, CredentialRef: m.Secrets[0].Ref}}
	}
	config.ApplyMCPServerDefaults(&server)
	return server
}

func parseDuration(text string) time.Duration {
	if text == "" {
		return 0
	}
	value, err := time.ParseDuration(text)
	if err != nil {
		return 0
	}
	return value
}

func parseByteSize(text string) config.ByteSize {
	var size config.ByteSize
	if text == "" || size.UnmarshalText([]byte(text)) != nil {
		return 0
	}
	return size
}

func validDuration(text string) bool {
	if text == "" {
		return true
	}
	value, err := time.ParseDuration(text)
	return err == nil && value >= 0
}

func validByteSize(text string) bool {
	var size config.ByteSize
	return text == "" || size.UnmarshalText([]byte(text)) == nil
}

func (m *Manifest) normalized() Manifest {
	out := *m
	out.Capabilities = sortedCopy(m.Capabilities)
	out.Egress = sortedCopy(m.Egress)
	out.Tools = slices.Clone(m.Tools)
	slices.SortFunc(out.Tools, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
	out.Secrets = slices.Clone(m.Secrets)
	slices.SortFunc(out.Secrets, func(a, b Secret) int { return strings.Compare(a.Name, b.Name) })
	out.Backend.StartupTimeout = canonicalDuration(m.Backend.StartupTimeout)
	out.Backend.RequestTimeout = canonicalDuration(m.Backend.RequestTimeout)
	out.Backend.ConnectTimeout = canonicalDuration(m.Backend.ConnectTimeout)
	out.Backend.MaxMessageSize = canonicalByteSize(m.Backend.MaxMessageSize)
	out.Backend.Args = slices.Clone(m.Backend.Args)
	if out.Backend.Args == nil {
		out.Backend.Args = []string{}
	}
	return out
}

func canonicalDuration(text string) string {
	if text == "" {
		return ""
	}
	return parseDuration(text).String()
}

func canonicalByteSize(text string) string {
	if text == "" {
		return ""
	}
	return strconv.FormatInt(int64(parseByteSize(text)), 10)
}

func sortedCopy(values []string) []string {
	out := slices.Clone(values)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return out
}

func (m *Manifest) Canonical() ([]byte, error) {
	normalized := m.normalized()
	if normalized.Tools == nil {
		normalized.Tools = []Tool{}
	}
	if normalized.Secrets == nil {
		normalized.Secrets = []Secret{}
	}
	data, err := json.Marshal(&normalized)
	if err != nil {
		return nil, Wrap(ErrManifestInvalid, err, "the manifest cannot be encoded")
	}
	return data, nil
}
