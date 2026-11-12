package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	ToolCreatePR = "github.create_pr"
	ToolMerge    = "github.merge"
	ToolComment  = "github.comment"
	ToolVersion  = "v1"

	defaultBaseURL         = "https://api.github.com"
	defaultTimeout         = 30 * time.Second
	defaultMaxResponseSize = 64 * 1024
	githubAPIVersion       = "2022-11-28"

	aliasNotConfigured = "credential is not a configured github credential"
	optionsRequired    = "github tool options are required"
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
	Repo       string `json:"repo"`
	Credential string `json:"credential"`
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

func parseRepo(repo string) (owner, name string, err error) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || len(repo) > 128 {
		return "", "", toolbroker.Errorf(toolbroker.ResultInvalidArgument, "repo must be owner/name")
	}
	return parts[0], parts[1], nil
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
		owner, repo, err := parseRepo(args.Repo)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Head) == "" || strings.TrimSpace(args.Base) == "" {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "title, head, and base are required")
		}
		token, err := resolveToken(args.Credential, cli.credentials)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		status, raw, err := cli.call(ctx, token, http.MethodPost,
			fmt.Sprintf("/repos/%s/%s/pulls", owner, repo),
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
			"external_id": fmt.Sprintf("%s/%s#%d", owner, repo, created.Number),
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
	MergeMethod string `json:"merge_method,omitempty"`
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
		owner, repo, err := parseRepo(args.Repo)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		if args.Number <= 0 {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "number must be positive")
		}
		method := args.MergeMethod
		if method == "" {
			method = "merge"
		}
		if method != "merge" && method != "squash" && method != "rebase" {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "merge_method must be merge, squash, or rebase")
		}
		token, err := resolveToken(args.Credential, cli.credentials)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		status, raw, err := cli.call(ctx, token, http.MethodPut,
			fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, args.Number),
			map[string]string{"merge_method": method})
		if err != nil {
			return toolbroker.ToolResult{}, err
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
		owner, repo, err := parseRepo(args.Repo)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		if args.Number <= 0 {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "number must be positive")
		}
		if strings.TrimSpace(args.Body) == "" || len(args.Body) > 65536 {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "body must be non-empty within 64 KiB")
		}
		token, err := resolveToken(args.Credential, cli.credentials)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		status, raw, err := cli.call(ctx, token, http.MethodPost,
			fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, args.Number),
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
	Type       string               `json:"type"`
	Properties map[string]fieldType `json:"properties"`
	Required   []string             `json:"required"`
}

type fieldType struct {
	Type    string `json:"type"`
	Minimum *int   `json:"minimum,omitempty"`
}

func intFloor(limit int) *int {
	return &limit
}

func fieldSchema(requiredStrings, optionalStrings, requiredNumbers []string) json.RawMessage {
	properties := map[string]fieldType{
		"repo":       {Type: "string"},
		"credential": {Type: "string"},
	}
	required := make([]string, 0, 2+len(requiredStrings)+len(requiredNumbers))
	required = append(required, "repo", "credential")
	for _, field := range requiredStrings {
		if _, ok := properties[field]; !ok {
			properties[field] = fieldType{Type: "string"}
		}
		required = append(required, field)
	}
	for _, field := range optionalStrings {
		if _, ok := properties[field]; !ok {
			properties[field] = fieldType{Type: "string"}
		}
	}
	for _, field := range requiredNumbers {
		properties[field] = fieldType{Type: "integer", Minimum: intFloor(1)}
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
		allowed := map[string]bool{"repo": true, "credential": true}
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
		for _, field := range append([]string{"repo", "credential"}, requiredStrings...) {
			var value string
			if err := json.Unmarshal(document[field], &value); err != nil || strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("argument %q must be a non-empty string", field)
			}
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

func ToolNames() []string {
	return []string{ToolCreatePR, ToolMerge, ToolComment}
}

func Definitions() []tools.Definition {
	return []tools.Definition{
		{
			Name: ToolCreatePR, Version: ToolVersion,
			Schema:           fieldSchema([]string{"title", "head", "base"}, []string{"body"}, nil),
			Validator:        fieldValidator([]string{"title", "head", "base"}, []string{"body"}, nil),
			RequiresApproval: true,
			Effectful:        true,
		},
		{
			Name: ToolMerge, Version: ToolVersion,
			Schema:           fieldSchema(nil, []string{"merge_method"}, []string{"number"}),
			Validator:        fieldValidator(nil, []string{"merge_method"}, []string{"number"}),
			RequiresApproval: true,
			Effectful:        true,
		},
		{
			Name: ToolComment, Version: ToolVersion,
			Schema:           fieldSchema([]string{"body"}, nil, []string{"number"}),
			Validator:        fieldValidator([]string{"body"}, nil, []string{"number"}),
			RequiresApproval: true,
			Effectful:        true,
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
		if err := json.Unmarshal(validated, &args); err != nil || credentialIndex(credentials, args.Credential) < 0 {
			return nil, errors.New(aliasNotConfigured)
		}
		return validated, nil
	}
}

func Register(registrar ToolRegistrar, options *ToolOptions) error {
	if registrar == nil {
		return errors.New("github tools require a registrar")
	}
	if options == nil {
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, optionsRequired)
	}
	credentials := slices.Clone(options.Credentials)
	if len(credentials) == 0 {
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, "github tools require at least one configured credential")
	}
	for i := range credentials {
		if credentialIndex(credentials[:i], credentials[i].Name) >= 0 {
			return toolbroker.Errorf(toolbroker.ResultInvalidArgument, "github credential %q is listed twice", credentials[i].Name)
		}
	}
	adapters := map[string]toolbroker.Adapter{
		ToolCreatePR: CreatePRAdapter(options),
		ToolMerge:    MergeAdapter(options),
		ToolComment:  CommentAdapter(options),
	}
	definitions := Definitions()
	rules := Rules()
	for i := range definitions {
		definitions[i].Validator = credentialValidator(definitions[i].Validator, credentials)
		if err := registrar.RegisterTool(&definitions[i], adapters[definitions[i].Name], &rules[i]); err != nil {
			return fmt.Errorf("register %s: %w", definitions[i].Name, err)
		}
	}
	return nil
}
