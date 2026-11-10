package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	"github.com/anggasct/aura/internal/modules/plugin/auth"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	APIVersion       = "aura.plugin/v1"
	ManifestFile     = "plugin.yaml"
	MaxManifestBytes = 256 << 10

	TierRead   = "read"
	TierEffect = "effect"

	BackendMCP       = "mcp"
	BackendConnector = "connector"

	AuthNone         = auth.KindNone
	AuthStaticSecret = auth.KindStaticSecret
	AuthOAuth2       = auth.KindOAuth2

	maxNameRunes         = 64
	maxDescriptionBytes  = 1024
	maxCapabilities      = 16
	maxTools             = 64
	maxEgress            = 32
	maxSecrets           = 2
	maxScopes            = 32
	maxURLBytes          = 2048
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
	scopePattern      = regexp.MustCompile(`^[\x21\x23-\x5B\x5D-\x7E]{1,128}$`)
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
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Tier        string   `yaml:"tier" json:"tier"`
	Display     []string `yaml:"display" json:"display,omitempty"`
}

type Secret struct {
	Name string `yaml:"name" json:"name"`
	Ref  string `yaml:"ref" json:"ref"`
}

type Auth struct {
	Kind               string            `yaml:"kind" json:"kind"`
	Secret             string            `yaml:"secret" json:"secret"`
	Header             string            `yaml:"header" json:"header"`
	AuthorizeURL       string            `yaml:"authorize_url" json:"authorize_url,omitempty"`
	TokenURL           string            `yaml:"token_url" json:"token_url,omitempty"`
	Scopes             []string          `yaml:"scopes" json:"scopes,omitempty"`
	RedirectURI        string            `yaml:"redirect_uri" json:"redirect_uri,omitempty"`
	ClientIDSecret     string            `yaml:"client_id_secret" json:"client_id_secret,omitempty"`
	ClientSecretSecret string            `yaml:"client_secret_secret" json:"client_secret_secret,omitempty"`
	Hosts              []string          `yaml:"hosts" json:"hosts,omitempty"`
	AuthorizeParams    map[string]string `yaml:"authorize_params" json:"authorize_params,omitempty"`
	AccessOnly         bool              `yaml:"access_only" json:"access_only,omitempty"`
	Issuer             string            `yaml:"issuer" json:"issuer,omitempty"`
}

func (a *Auth) oauth2FieldsSet() bool {
	return a.AuthorizeURL != "" || a.TokenURL != "" || len(a.Scopes) > 0 || a.RedirectURI != "" || a.ClientIDSecret != "" || a.ClientSecretSecret != "" || len(a.AuthorizeParams) > 0 || a.AccessOnly || a.Issuer != ""
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
	if kind, isString := probe.Backend.Kind.(string); isString && kind != "" && kind != BackendMCP && kind != BackendConnector {
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
		if err := tools.ValidateDisplayNames(tool.Display); err != nil {
			found.add("%s.display must list at most %d unique argument names matching [A-Za-z_][A-Za-z0-9_-]{0,63}", field, tools.MaxDisplayFields)
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
		if len(m.Secrets) > 0 || m.Auth.Secret != "" || m.Auth.Header != "" || m.Auth.oauth2FieldsSet() {
			found.add("auth.kind none takes no secret, header or oauth2 field")
		}
	case AuthStaticSecret:
		if len(m.Secrets) != 1 || m.Auth.Secret != m.Secrets[0].Name {
			found.add("auth.secret must name the one declared secret")
		}
		if m.Auth.Header != "" && !headerPattern.MatchString(m.Auth.Header) {
			found.add("auth.header must be a header name")
		}
		if m.Auth.oauth2FieldsSet() {
			found.add("auth.kind static_secret takes no oauth2 field")
		}
		if !m.backendCarriesAuth() {
			found.add("auth.kind static_secret needs the streamable_http transport or the connector backend")
		}
	case AuthOAuth2:
		m.validateOAuth2(found)
	default:
		found.add("auth.kind must be %s, %s or %s", AuthNone, AuthStaticSecret, AuthOAuth2)
	}
	m.validateAuthHosts(found)
}

func normalizedEgressEntry(entry string) string {
	entry = strings.ToLower(entry)
	if !strings.Contains(entry, ":") {
		entry += ":443"
	}
	return entry
}

func (m *Manifest) validateAuthHosts(found *problems) {
	hosts := m.Auth.Hosts
	carriesCredentials := m.Backend.Kind == BackendConnector && (m.Auth.Kind == AuthStaticSecret || m.Auth.Kind == AuthOAuth2)
	switch {
	case !carriesCredentials:
		if len(hosts) > 0 {
			found.add("auth.hosts applies only to a connector backend with static_secret or oauth2 auth")
		}
		return
	case len(hosts) == 0:
		found.add("auth.hosts must list the egress hosts that receive the credential")
		return
	case len(hosts) > maxEgress:
		found.add("auth.hosts lists more than %d entries", maxEgress)
		return
	}
	listed := make(map[string]bool, len(m.Egress))
	for _, entry := range m.Egress {
		listed[normalizedEgressEntry(entry)] = true
	}
	seen := map[string]bool{}
	for _, host := range hosts {
		if !validEgressEntry(host) {
			found.add("auth.hosts entries must be a lowercase host or host:port without wildcards")
			continue
		}
		normalized := normalizedEgressEntry(host)
		if !listed[normalized] {
			found.add("auth.hosts entries must be listed in egress")
		}
		if seen[normalized] {
			found.add("auth.hosts lists an entry twice")
		}
		seen[normalized] = true
	}
}

func (m *Manifest) validateOAuth2(found *problems) {
	oauth := &m.Auth
	if oauth.Secret != "" || oauth.Header != "" {
		found.add("auth.kind oauth2 takes no secret or header")
	}
	if !m.backendCarriesAuth() {
		found.add("auth.kind oauth2 needs the streamable_http transport or the connector backend")
	}
	validateAuthURL(found, "auth.authorize_url", oauth.AuthorizeURL)
	if token := validateAuthURL(found, "auth.token_url", oauth.TokenURL); token != nil && !egressListed(m.Egress, token) {
		found.add("auth.token_url host must be listed in egress")
	}
	m.validateScopes(found)
	validateRedirectURI(found, oauth.RedirectURI)
	validateAuthorizeParams(found, oauth.AuthorizeParams)
	if oauth.Issuer != "" {
		if issuer := validateAuthURL(found, "auth.issuer", oauth.Issuer); issuer != nil && issuer.RawQuery != "" {
			found.add("auth.issuer must not carry a query")
		}
	}
	declared := map[string]bool{}
	for i := range m.Secrets {
		declared[m.Secrets[i].Name] = true
	}
	if oauth.ClientIDSecret == "" || !declared[oauth.ClientIDSecret] {
		found.add("auth.client_id_secret must name a declared secret")
	}
	if oauth.ClientSecretSecret != "" && (!declared[oauth.ClientSecretSecret] || oauth.ClientSecretSecret == oauth.ClientIDSecret) {
		found.add("auth.client_secret_secret must name another declared secret")
	}
	wanted := 1
	if oauth.ClientSecretSecret != "" {
		wanted = 2
	}
	if len(m.Secrets) != wanted {
		found.add("secrets must declare exactly the oauth2 client secrets")
	}
}

func validateAuthURL(found *problems, field, raw string) *url.URL {
	parsed, err := url.Parse(raw)
	switch {
	case raw == "" || len(raw) > maxURLBytes || err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "":
		found.add("%s must be an https url without credentials or fragment", field)
		return nil
	case parsed.Scheme == "http" && !config.IsLoopbackBaseURL(raw), parsed.Scheme != "https" && parsed.Scheme != "http":
		found.add("%s must use https, or http on a loopback address", field)
		return nil
	}
	return parsed
}

func egressListed(egress []string, target *url.URL) bool {
	return slices.Contains(egress, strings.ToLower(target.Host)) || slices.Contains(egress, strings.ToLower(target.Hostname()))
}

func (m *Manifest) validateScopes(found *problems) {
	scopes := m.Auth.Scopes
	if len(scopes) == 0 || len(scopes) > maxScopes {
		found.add("auth.scopes must list between 1 and %d scopes", maxScopes)
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		if !scopePattern.MatchString(scope) {
			found.add("auth.scopes entries must be printable ascii scope tokens without spaces, quotes or backslashes")
			continue
		}
		if seen[scope] {
			found.add("auth.scopes lists an entry twice")
		}
		seen[scope] = true
	}
}

func validateRedirectURI(found *problems, raw string) {
	parsed, err := url.Parse(raw)
	if raw == "" || len(raw) > maxURLBytes || err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.ForceQuery || !strings.HasPrefix(parsed.Path, "/") {
		found.add("auth.redirect_uri must be an exact url with a path, without credentials, query or fragment")
		return
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if parsed.Hostname() != "127.0.0.1" || strings.HasSuffix(parsed.Host, ":") || !validOptionalPort(parsed.Port()) {
			found.add("auth.redirect_uri over http must be http://127.0.0.1[:<port>]/<path>")
		}
	default:
		found.add("auth.redirect_uri must use https or http://127.0.0.1[:<port>]/<path>")
	}
}

func validOptionalPort(port string) bool {
	if port == "" {
		return true
	}
	number, err := strconv.Atoi(port)
	return err == nil && number >= 1 && number <= 65535 && strconv.Itoa(number) == port
}

func validateAuthorizeParams(found *problems, params map[string]string) {
	for key, value := range params {
		if !auth.ValidAuthorizeParam(key, value) {
			found.add("auth.authorize_params allows only access_type (offline, online) and prompt (consent, select_account, none)")
			return
		}
	}
}

func (m *Manifest) backendCarriesAuth() bool {
	return m.Backend.Kind == BackendConnector || m.Backend.Transport == config.MCPTransportStreamableHTTP
}

func (m *Manifest) validateBackend(found *problems) {
	backend := &m.Backend
	if backend.Kind != BackendMCP && backend.Kind != BackendConnector {
		found.add("backend.kind must be %s or %s", BackendMCP, BackendConnector)
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
	if backend.Kind == BackendConnector {
		m.validateConnectorTransport(&local)
	} else {
		m.validateMCPTransport(&local)
	}
	timeouts := []struct{ name, value string }{{"startup_timeout", backend.StartupTimeout}, {"request_timeout", backend.RequestTimeout}, {"connect_timeout", backend.ConnectTimeout}}
	for _, field := range timeouts {
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

func (m *Manifest) validateConnectorTransport(local *problems) {
	backend := &m.Backend
	if !validBundleCommand(backend.Command) {
		local.add("backend.command must be a relative path inside the bundle")
	}
	if backend.Transport != "" || backend.URL != "" || backend.ConnectTimeout != "" {
		local.add("backend.transport, backend.url and backend.connect_timeout are not allowed for the connector kind")
	}
}

func (m *Manifest) validateMCPTransport(local *problems) {
	backend := &m.Backend
	switch backend.Transport {
	case config.MCPTransportStdio:
		if !validBundleCommand(backend.Command) {
			local.add("backend.command must be a relative path inside the bundle")
		}
		if backend.URL != "" {
			local.add("backend.url is not allowed for the stdio transport")
		}
	case config.MCPTransportStreamableHTTP:
		m.validateBackendURL(local)
	default:
		local.add("backend.transport must be %s or %s", config.MCPTransportStdio, config.MCPTransportStreamableHTTP)
	}
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
	if !egressListed(m.Egress, parsed) {
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
	if backend.Kind == BackendConnector {
		server.Transport = config.MCPTransportStdio
	}
	capabilities := slices.Clone(m.Capabilities)
	slices.Sort(capabilities)
	server.Capabilities = capabilities
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
	out.Auth.Scopes = sortedScopes(m.Auth.Scopes)
	out.Auth.Hosts = sortedScopes(m.Auth.Hosts)
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

func sortedScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}
	return sortedCopy(scopes)
}

func (m *Manifest) secretRef(name string) string {
	for i := range m.Secrets {
		if m.Secrets[i].Name == name {
			return m.Secrets[i].Ref
		}
	}
	return ""
}

func (m *Manifest) AuthSpec(fingerprint string) auth.Spec {
	spec := auth.Spec{PluginID: m.ID, Kind: m.Auth.Kind, Fingerprint: fingerprint}
	switch m.Auth.Kind {
	case AuthStaticSecret:
		spec.Header = m.Auth.Header
		spec.SecretRef = m.secretRef(m.Auth.Secret)
	case AuthOAuth2:
		spec.AuthorizeURL = m.Auth.AuthorizeURL
		spec.TokenURL = m.Auth.TokenURL
		spec.RedirectURI = m.Auth.RedirectURI
		spec.Scopes = sortedScopes(m.Auth.Scopes)
		spec.ClientIDRef = m.secretRef(m.Auth.ClientIDSecret)
		spec.ClientSecretRef = m.secretRef(m.Auth.ClientSecretSecret)
		spec.AuthorizeParams = maps.Clone(m.Auth.AuthorizeParams)
		spec.AccessOnly = m.Auth.AccessOnly
		spec.Issuer = m.Auth.Issuer
	}
	return spec
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
