package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	ToolCreatePR = "github_create_pr"
	ToolMerge    = "github_merge"
	ToolComment  = "github_comment"
	ToolVersion  = "v1"

	defaultBaseURL         = "https://api.github.com"
	defaultTimeout         = 30 * time.Second
	defaultMaxResponseSize = 64 * 1024
	githubAPIVersion       = "2022-11-28"

	aliasNotConfigured = "credential is not a configured github credential"
	optionsRequired    = "github tool options are required"
	headMoved          = "github refused the merge: the pull request head is not the given sha"
	bodyHint           = "Shown whole on the owner's approval card. A body that does not fit the card (about 800 characters, less with a long title or repository) is refused, not truncated."
	maxBodyHint        = 800
	capabilityName     = "provider-github"
)

type Credential struct {
	Name string
	Ref  string
}

type ToolOptions struct {
	BaseURL          string
	Resolver         egress.Resolver
	Timeout          time.Duration
	MaxResponseBytes int64

	HTTPClient  *http.Client
	Credentials []Credential
}

func baseURLOrDefault(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return defaultBaseURL
	}
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func timeoutOrDefault(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultTimeout
	}
	return timeout
}

func maxResponseOrDefault(limit int64) int64 {
	if limit <= 0 {
		return defaultMaxResponseSize
	}
	return limit
}

type commonArguments struct {
	Repo           string `json:"repo"`
	CredentialName string `json:"credential_name"`
}

type client struct {
	baseURL     string
	http        *http.Client
	timeout     time.Duration
	maxSize     int64
	credentials []Credential
}

func newClient(options *ToolOptions) (*client, error) {
	if options == nil {
		return nil, toolbroker.Errorf(toolbroker.ResultInvalidArgument, optionsRequired)
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = egress.NewClient(options.Resolver)
	}
	return &client{
		baseURL:     baseURLOrDefault(options.BaseURL),
		http:        httpClient,
		timeout:     timeoutOrDefault(options.Timeout),
		maxSize:     maxResponseOrDefault(options.MaxResponseBytes),
		credentials: slices.Clone(options.Credentials),
	}, nil
}

var (
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

const repoDetail = "repo must be owner/name, owner up to 39 letters, digits, '-' or '_', name up to 100 letters, digits, '.', '-' or '_'"

type repoRef struct {
	owner, name string
}

func parseRepo(repo string) (repoRef, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || !ownerPattern.MatchString(parts[0]) || !namePattern.MatchString(parts[1]) || parts[1] == "." || parts[1] == ".." {
		return repoRef{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, repoDetail)
	}
	return repoRef{owner: parts[0], name: parts[1]}, nil
}

func (r repoRef) path(rest ...string) string {
	var path strings.Builder
	path.WriteString("/repos/")
	path.WriteString(url.PathEscape(r.owner))
	path.WriteString("/")
	path.WriteString(url.PathEscape(r.name))
	for _, element := range rest {
		path.WriteString("/")
		path.WriteString(url.PathEscape(element))
	}
	return path.String()
}

func credentialIndex(credentials []Credential, name string) int {
	return slices.IndexFunc(credentials, func(c Credential) bool { return c.Name == name })
}

func resolveToken(name string, credentials []Credential) (string, error) {
	index := credentialIndex(credentials, name)
	if index < 0 {
		return "", toolbroker.Errorf(toolbroker.ResultInvalidArgument, aliasNotConfigured)
	}
	ref := credentials[index].Ref
	var source secret.Reference
	switch {
	case strings.HasPrefix(ref, "env://"):
		source.Env = strings.TrimPrefix(ref, "env://")
	case strings.HasPrefix(ref, "file://"):
		source.File = strings.TrimPrefix(ref, "file://")
	}
	token, err := source.Resolve()
	if err != nil || token == "" {
		return "", toolbroker.Errorf(toolbroker.ResultInvalidArgument, "github credential is unavailable")
	}
	return token, nil
}

func (c *client) call(ctx context.Context, token, method, path string, body any) (int, json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "github request is not encodable: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "github request is not buildable: %v", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github request failed: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, c.maxSize+1))
	if err != nil {
		return 0, nil, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github response is not readable: %v", err)
	}
	if int64(len(raw)) > c.maxSize {
		return 0, nil, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github response exceeds the bound")
	}
	if !json.Valid(raw) {
		return response.StatusCode, nil, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github response is not JSON")
	}
	return response.StatusCode, json.RawMessage(raw), nil
}

func statusError(status int, raw json.RawMessage) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github rejected the credential (status %d)", status)
	}
	if status == http.StatusNotFound {
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, "github found no such resource (status 404)")
	}
	if status == http.StatusUnprocessableEntity {
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, "github rejected the request: %s", truncMessage(raw))
	}
	return toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github request failed with status %d: %s", status, truncMessage(raw))
}

func truncMessage(raw json.RawMessage) string {
	var document struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &document); err != nil || document.Message == "" {
		return "no detail"
	}
	if len(document.Message) > 200 {
		return document.Message[:200]
	}
	return document.Message
}

func okResult(name, output string) (toolbroker.ToolResult, error) {
	return toolbroker.ToolResult{
		ToolName: name, ToolVersion: ToolVersion,
		Class: toolbroker.ResultOK, Untrusted: true,
		Output: json.RawMessage(output),
	}, nil
}

type createPRArguments struct {
	commonArguments
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body,omitempty"`
}

func CreatePRAdapter(options *ToolOptions) toolbroker.Adapter {
	cli, setupErr := newClient(options)
	return func(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		if setupErr != nil {
			return toolbroker.ToolResult{}, setupErr
		}
		if request == nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
		}
		var args createPRArguments
		if err := json.Unmarshal(request.Arguments, &args); err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "arguments must be a JSON object")
		}
		ref, err := parseRepo(args.Repo)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Head) == "" || strings.TrimSpace(args.Base) == "" {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "title, head, and base are required")
		}
		token, err := resolveToken(args.CredentialName, cli.credentials)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		status, raw, err := cli.call(ctx, token, http.MethodPost,
			ref.path("pulls"),
			map[string]string{"title": args.Title, "head": args.Head, "base": args.Base, "body": args.Body})
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		if status != http.StatusCreated {
			return toolbroker.ToolResult{}, statusError(status, raw)
		}
		var created struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
		}
		if err := json.Unmarshal(raw, &created); err != nil || created.Number <= 0 {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github pull request response is not usable")
		}
		output, err := json.Marshal(map[string]any{
			"external_id": fmt.Sprintf("%s/%s#%d", ref.owner, ref.name, created.Number),
			"number":      created.Number,
			"url":         created.HTMLURL,
		})
		if err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github result is not encodable: %v", err)
		}
		return okResult(ToolCreatePR, string(output))
	}
}

type mergeArguments struct {
	commonArguments
	Number      int    `json:"number"`
	SHA         string `json:"sha"`
	MergeMethod string `json:"merge_method"`
}

func MergeAdapter(options *ToolOptions) toolbroker.Adapter {
	cli, setupErr := newClient(options)
	return func(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		if setupErr != nil {
			return toolbroker.ToolResult{}, setupErr
		}
		if request == nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
		}
		var args mergeArguments
		if err := json.Unmarshal(request.Arguments, &args); err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "arguments must be a JSON object")
		}
		ref, err := parseRepo(args.Repo)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		if args.Number <= 0 {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "number must be positive")
		}
		if !slices.Contains(mergeMethods, args.MergeMethod) {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "merge_method must be merge, squash, or rebase")
		}
		token, err := resolveToken(args.CredentialName, cli.credentials)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		status, raw, err := cli.call(ctx, token, http.MethodPut,
			ref.path("pulls", strconv.Itoa(args.Number), "merge"),
			map[string]string{"merge_method": args.MergeMethod, "sha": args.SHA})
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		if status == http.StatusConflict {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, headMoved)
		}
		if status != http.StatusOK {
			return toolbroker.ToolResult{}, statusError(status, raw)
		}
		var merged struct {
			Merged bool   `json:"merged"`
			SHA    string `json:"sha"`
		}
		if err := json.Unmarshal(raw, &merged); err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github merge response is not usable")
		}
		output, err := json.Marshal(map[string]any{"merged": merged.Merged, "sha": merged.SHA})
		if err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github result is not encodable: %v", err)
		}
		return okResult(ToolMerge, string(output))
	}
}

type commentArguments struct {
	commonArguments
	Number int    `json:"number"`
	Body   string `json:"body"`
}

func CommentAdapter(options *ToolOptions) toolbroker.Adapter {
	cli, setupErr := newClient(options)
	return func(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		if setupErr != nil {
			return toolbroker.ToolResult{}, setupErr
		}
		if request == nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
		}
		var args commentArguments
		if err := json.Unmarshal(request.Arguments, &args); err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "arguments must be a JSON object")
		}
		ref, err := parseRepo(args.Repo)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		if args.Number <= 0 {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "number must be positive")
		}
		if strings.TrimSpace(args.Body) == "" || len(args.Body) > 65536 {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "body must be non-empty within 64 KiB")
		}
		token, err := resolveToken(args.CredentialName, cli.credentials)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		status, raw, err := cli.call(ctx, token, http.MethodPost,
			ref.path("issues", strconv.Itoa(args.Number), "comments"),
			map[string]string{"body": args.Body})
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		if status != http.StatusCreated {
			return toolbroker.ToolResult{}, statusError(status, raw)
		}
		var created struct {
			ID      int64  `json:"id"`
			HTMLURL string `json:"html_url"`
		}
		if err := json.Unmarshal(raw, &created); err != nil || created.ID <= 0 {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github comment response is not usable")
		}
		output, err := json.Marshal(map[string]any{"id": created.ID, "url": created.HTMLURL})
		if err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "github result is not encodable: %v", err)
		}
		return okResult(ToolComment, string(output))
	}
}

type objectSchema struct {
	Type                 string               `json:"type"`
	Properties           map[string]fieldType `json:"properties"`
	Required             []string             `json:"required"`
	AdditionalProperties bool                 `json:"additionalProperties"`
}

type fieldType struct {
	Type        string   `json:"type"`
	Minimum     *int     `json:"minimum,omitempty"`
	MaxLength   *int     `json:"maxLength,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

func intRef(limit int) *int {
	return &limit
}

var mergeMethods = []string{"merge", "squash", "rebase"}

func textProperty(name string) fieldType {
	if name == "merge_method" {
		return fieldType{Type: "string", Enum: slices.Clone(mergeMethods)}
	}
	if name == "body" {
		return fieldType{Type: "string", MaxLength: intRef(maxBodyHint), Description: bodyHint}
	}
	return fieldType{Type: "string"}
}

func fieldSchema(requiredStrings, optionalStrings, requiredNumbers []string) json.RawMessage {
	properties := map[string]fieldType{
		"repo":            {Type: "string"},
		"credential_name": {Type: "string"},
	}
	required := make([]string, 0, 2+len(requiredStrings)+len(requiredNumbers))
	required = append(required, "repo", "credential_name")
	for _, field := range requiredStrings {
		if _, ok := properties[field]; !ok {
			properties[field] = textProperty(field)
		}
		required = append(required, field)
	}
	for _, field := range optionalStrings {
		if _, ok := properties[field]; !ok {
			properties[field] = textProperty(field)
		}
	}
	for _, field := range requiredNumbers {
		properties[field] = fieldType{Type: "integer", Minimum: intRef(1)}
		required = append(required, field)
	}
	encoded, err := json.Marshal(objectSchema{
		Type: "object", Properties: properties,
		Required: required,
	})
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return encoded
}

func fieldValidator(requiredStrings, optionalStrings, requiredNumbers []string) func(json.RawMessage) (json.RawMessage, error) {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		var document map[string]json.RawMessage
		if err := json.Unmarshal(raw, &document); err != nil {
			return nil, errors.New("arguments must be a JSON object")
		}
		allowed := map[string]bool{"repo": true, "credential_name": true}
		for _, field := range requiredStrings {
			allowed[field] = true
		}
		for _, field := range optionalStrings {
			allowed[field] = true
		}
		for _, field := range requiredNumbers {
			allowed[field] = true
		}
		for field := range document {
			if !allowed[field] {
				return nil, fmt.Errorf("unknown argument %q", field)
			}
		}
		for _, field := range append([]string{"repo", "credential_name"}, requiredStrings...) {
			var value string
			if err := json.Unmarshal(document[field], &value); err != nil || strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("argument %q must be a non-empty string", field)
			}
		}
		var repo string
		if err := json.Unmarshal(document["repo"], &repo); err != nil {
			return nil, errors.New(`argument "repo" must be a non-empty string`)
		}
		if _, err := parseRepo(repo); err != nil {
			return nil, err
		}
		for _, field := range optionalStrings {
			rawValue, ok := document[field]
			if !ok {
				continue
			}
			var value string
			if err := json.Unmarshal(rawValue, &value); err != nil {
				return nil, fmt.Errorf("argument %q must be a string", field)
			}
		}
		for _, field := range requiredNumbers {
			var value float64
			if err := json.Unmarshal(document[field], &value); err != nil || value <= 0 || value != float64(int(value)) {
				return nil, fmt.Errorf("argument %q must be a positive integer", field)
			}
		}
		return raw, nil
	}
}

var headPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func mergeValidator(inner func(json.RawMessage) (json.RawMessage, error)) func(json.RawMessage) (json.RawMessage, error) {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		var probe struct {
			MergeMethod string `json:"merge_method"`
		}
		if json.Unmarshal(raw, &probe) == nil && !slices.Contains(mergeMethods, probe.MergeMethod) {
			return nil, toolbroker.Errorf(toolbroker.ResultInvalidArgument, `argument "merge_method" must be merge, squash, or rebase`)
		}
		validated, err := inner(raw)
		if err != nil {
			return nil, err
		}
		var args mergeArguments
		if err := json.Unmarshal(validated, &args); err != nil || !headPattern.MatchString(args.SHA) {
			return nil, errors.New(`argument "sha" must be a 40-character lowercase hex commit id`)
		}
		return validated, nil
	}
}

func describe(action string) string {
	return action + "; credential_name is one of the configured credential names, and the owner approves each call on an exact card that shows every argument, so a body over about 800 characters is refused."
}

func withCredentialEnum(schema json.RawMessage, names []string) (json.RawMessage, error) {
	var document objectSchema
	if err := json.Unmarshal(schema, &document); err != nil {
		return nil, fmt.Errorf("decode github schema: %w", err)
	}
	if _, ok := document.Properties["credential_name"]; !ok {
		return nil, errors.New("github schema has no credential_name property")
	}
	document.Properties["credential_name"] = fieldType{Type: "string", Enum: slices.Clone(names)}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode github schema: %w", err)
	}
	return encoded, nil
}

func ToolNames() []string {
	return []string{ToolCreatePR, ToolMerge, ToolComment}
}

func Definitions() []tools.Definition {
	return []tools.Definition{
		{
			Name: ToolCreatePR, Version: ToolVersion,
			Schema:               fieldSchema([]string{"title", "head", "base"}, []string{"body"}, nil),
			Validator:            fieldValidator([]string{"title", "head", "base"}, []string{"body"}, nil),
			RequiresApproval:     true,
			Effectful:            true,
			OptIn:                true,
			ExternalSink:         true,
			NoStandingGrant:      true,
			ExactDisplay:         true,
			RequiredCapabilities: []string{capabilityName},
			Description:          describe("Open a pull request on a GitHub repository"),
			Display:              []string{"repo", "head", "base", "title", "credential_name", "body"},
		},
		{
			Name: ToolMerge, Version: ToolVersion,
			Schema:               fieldSchema([]string{"sha", "merge_method"}, nil, []string{"number"}),
			Validator:            mergeValidator(fieldValidator([]string{"sha", "merge_method"}, nil, []string{"number"})),
			RequiresApproval:     true,
			Effectful:            true,
			OptIn:                true,
			NoStandingGrant:      true,
			ExactDisplay:         true,
			RequiredCapabilities: []string{capabilityName},
			Description:          describe("Merge a GitHub pull request at the given head sha"),
			Display:              []string{"repo", "number", "sha", "merge_method", "credential_name"},
		},
		{
			Name: ToolComment, Version: ToolVersion,
			Schema:               fieldSchema([]string{"body"}, nil, []string{"number"}),
			Validator:            fieldValidator([]string{"body"}, nil, []string{"number"}),
			RequiresApproval:     true,
			Effectful:            true,
			OptIn:                true,
			ExternalSink:         true,
			NoStandingGrant:      true,
			ExactDisplay:         true,
			RequiredCapabilities: []string{capabilityName},
			Description:          describe("Comment on a GitHub issue or pull request"),
			Display:              []string{"repo", "number", "credential_name", "body"},
		},
	}
}

func Rules() []approval.Rule {
	rules := make([]approval.Rule, 0, len(Definitions()))
	listed := Definitions()
	for i := range listed {
		definition := &listed[i]
		rules = append(rules, approval.Rule{
			ToolName:             definition.Name,
			ToolVersion:          definition.Version,
			RequiresApproval:     true,
			RequiredCapabilities: append([]string(nil), definition.RequiredCapabilities...),
			AllowedTrust: []approval.TrustLabel{
				approval.TrustOwnerInput,
				approval.TrustTrustedConfiguration,
				approval.TrustDerivedUntrusted,
			},
			Constraints: approval.Constraints{
				MaxOutputBytes: defaultMaxResponseSize,
				Timeout:        defaultTimeout,
			},
		})
	}
	return rules
}

type ToolRegistrar interface {
	RegisterTool(definition *tools.Definition, adapter toolbroker.Adapter, rule *approval.Rule) error
}

func credentialValidator(inner func(json.RawMessage) (json.RawMessage, error), credentials []Credential) func(json.RawMessage) (json.RawMessage, error) {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		validated, err := inner(raw)
		if err != nil {
			return nil, err
		}
		var args commonArguments
		if err := json.Unmarshal(validated, &args); err != nil || credentialIndex(credentials, args.CredentialName) < 0 {
			return nil, errors.New(aliasNotConfigured)
		}
		return validated, nil
	}
}

func CheckedDefinitions(credentials []Credential) ([]tools.Definition, error) {
	credentials = slices.Clone(credentials)
	if len(credentials) == 0 {
		return nil, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "github tools require at least one configured credential")
	}
	for i := range credentials {
		if credentialIndex(credentials[:i], credentials[i].Name) >= 0 {
			return nil, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "github credential %q is listed twice", credentials[i].Name)
		}
	}
	names := make([]string, len(credentials))
	for i := range credentials {
		names[i] = credentials[i].Name
	}
	definitions := Definitions()
	for i := range definitions {
		definitions[i].Validator = credentialValidator(definitions[i].Validator, credentials)
		schema, err := withCredentialEnum(definitions[i].Schema, names)
		if err != nil {
			return nil, fmt.Errorf("register %s: %w", definitions[i].Name, err)
		}
		definitions[i].Schema = schema
	}
	return definitions, nil
}

func Register(registrar ToolRegistrar, options *ToolOptions) error {
	if registrar == nil {
		return errors.New("github tools require a registrar")
	}
	if options == nil {
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, optionsRequired)
	}
	definitions, err := CheckedDefinitions(options.Credentials)
	if err != nil {
		return err
	}
	adapters := map[string]toolbroker.Adapter{
		ToolCreatePR: CreatePRAdapter(options),
		ToolMerge:    MergeAdapter(options),
		ToolComment:  CommentAdapter(options),
	}
	rules := Rules()
	for i := range definitions {
		if err := registrar.RegisterTool(&definitions[i], adapters[definitions[i].Name], &rules[i]); err != nil {
			return fmt.Errorf("register %s: %w", definitions[i].Name, err)
		}
	}
	return nil
}
