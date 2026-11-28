package operation_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
)

const filesRead = "files.read"

type pathObservation struct {
	Input     map[string]string
	Name      string
	Reference string
	HasFile   bool
	Body      string
	file      operation.File
}

func pathCatalog() operation.Catalog {
	return operation.Catalog{
		Group: operation.Group{Name: "files", Summary: "path fixtures"},
		Descriptors: []operation.Descriptor{{
			ID: filesRead, Summary: "read a file", Description: "reads a file or fetches an address",
			Exposure: operation.ExposureAgent, Risk: operation.RiskRead, Commit: operation.CommitNone,
			Texts: []operation.SurfaceText{{Surface: operation.SurfaceCLI, Text: "reads a file on this host"}},
			Params: []operation.Param{
				{
					Name: "source", Kind: operation.KindPath, Summary: "a file or an address", Required: true, Position: 1, URLParam: "url",
					Surfaces: []operation.Surface{operation.SurfaceCLI, operation.SurfaceAgent},
					Texts:    []operation.SurfaceText{{Surface: operation.SurfaceAgent, Text: "a workspace path"}},
				},
				{Name: "url", Kind: operation.KindString, Summary: "an https address", Required: true, Position: 2, MinLength: 1, MaxLength: 256, Surfaces: []operation.Surface{operation.SurfaceChat}},
			},
			Output: operation.Output{Kind: operation.OutputRecord, Fields: []operation.OutputField{{Name: "ok"}}},
		}},
	}
}

func pathRig(t *testing.T, workspace string, seen *pathObservation) *operationtest.Rig {
	t.Helper()
	return operationtest.NewRig(t, &operationtest.Options{
		Catalogs: []operation.Catalog{pathCatalog()},
		Files:    &operation.FileDeps{Workspace: workspace},
		Bindings: func(*operationtest.Stubs) []operation.Binding {
			return []operation.Binding{{ID: filesRead, Handle: func(ctx context.Context, call *operation.Call) (operation.Result, error) {
				*seen = pathObservation{HasFile: call.HasFile("source")}
				if err := json.Unmarshal(call.Input, &seen.Input); err != nil {
					return operation.Result{}, err
				}
				if seen.HasFile {
					file, err := call.File(ctx, "source")
					if err != nil {
						return operation.Result{}, err
					}
					body, err := io.ReadAll(file)
					if err != nil {
						return operation.Result{}, err
					}
					seen.Name, seen.Reference, seen.Body, seen.file = file.Name(), file.Reference(), string(body), file
				}
				return operation.JSONResult(map[string]any{"ok": true})
			}}}
		},
	})
}

func cliPathCall(surface operation.Surface, input string) *operation.Call {
	return &operation.Call{
		Caller: operation.Caller{Surface: surface, Class: principal.ClassOwner, PrincipalID: operationtest.OwnerID, SessionID: operationtest.SessionID, Direct: true},
		Input:  json.RawMessage(input),
	}
}

type pathResolutionCase struct {
	name      string
	surface   operation.Surface
	source    string
	wantName  string
	wantRef   string
	wantBody  string
	wantInput map[string]string
}

func TestTheKernelResolvesAPathParameterBeforeTheHandlerSeesIt(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "policy.md"), []byte("workspace body"), 0o600); err != nil {
		t.Fatal(err)
	}
	host := t.TempDir()
	hostFile := filepath.Join(host, "notes.md")
	if err := os.WriteFile(hostFile, []byte("host body"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []pathResolutionCase{
		{name: "a host path on the CLI", surface: operation.SurfaceCLI, source: hostFile, wantName: "notes.md", wantRef: hostFile, wantBody: "host body", wantInput: map[string]string{}},
		{name: "an address on the CLI", surface: operation.SurfaceCLI, source: "https://docs.example.test/a", wantInput: map[string]string{"url": "https://docs.example.test/a"}},
		{name: "an address in chat", surface: operation.SurfaceChat, source: "", wantInput: map[string]string{"url": "https://docs.example.test/b"}},
	}
	cases = append(cases, workspacePathResolutionCases()...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen pathObservation
			rig := pathRig(t, workspace, &seen)
			input := `{"source":"` + tc.source + `"}`
			if tc.surface == operation.SurfaceChat {
				input = `{"url":"https://docs.example.test/b"}`
			}

			if _, err := rig.Registry.Invoke(t.Context(), filesRead, cliPathCall(tc.surface, input)); err != nil {
				t.Fatalf("Invoke: %v", err)
			}

			if _, leaked := seen.Input["source"]; leaked {
				t.Errorf("the handler input %v carries the raw path", seen.Input)
			}
			if len(seen.Input) != len(tc.wantInput) || seen.Input["url"] != tc.wantInput["url"] {
				t.Errorf("handler input = %v, want %v", seen.Input, tc.wantInput)
			}
			if seen.HasFile != (tc.wantName != "") || seen.Name != tc.wantName || seen.Reference != tc.wantRef || seen.Body != tc.wantBody {
				t.Errorf("file = has %t name %q reference %q body %q, want %q %q %q", seen.HasFile, seen.Name, seen.Reference, seen.Body, tc.wantName, tc.wantRef, tc.wantBody)
			}
			if seen.file != nil {
				if _, err := seen.file.Read(make([]byte, 1)); err == nil {
					t.Error("the kernel left the opened file open after the call")
				}
			}
		})
	}
}

func TestTheKernelRefusesAHostPathItCannotOpenSafely(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.md")
	if err := os.WriteFile(target, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		source string
		want   operation.ErrorCode
	}{
		{name: "a symbolic link", source: link, want: operation.ErrorCodePolicyDenied},
		{name: "a directory", source: dir, want: operation.ErrorCodeSourceUnavailable},
		{name: "a missing file", source: filepath.Join(dir, "missing.md"), want: operation.ErrorCodeSourceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen pathObservation
			rig := pathRig(t, t.TempDir(), &seen)

			_, err := rig.Registry.Invoke(t.Context(), filesRead, cliPathCall(operation.SurfaceCLI, `{"source":"`+tc.source+`"}`))

			if !fault.HasCode(err, tc.want) {
				t.Fatalf("Invoke = %v, want %s", err, tc.want)
			}
			if strings.Contains(err.Error(), dir) {
				t.Errorf("error %q carries the host path", err)
			}
		})
	}
}

func TestAPathParameterIsNotAChatInput(t *testing.T) {
	var seen pathObservation
	rig := pathRig(t, t.TempDir(), &seen)

	_, err := rig.Registry.Invoke(t.Context(), filesRead, cliPathCall(operation.SurfaceChat, `{"source":"notes.md","url":"https://docs.example.test/a"}`))

	if !fault.HasCode(err, operation.ErrorCodeInputInvalid) {
		t.Fatalf("Invoke = %v, want %s", err, operation.ErrorCodeInputInvalid)
	}
}

func TestEachSurfaceReadsItsOwnDescriptionOfAPathOperation(t *testing.T) {
	var seen pathObservation
	rig := pathRig(t, t.TempDir(), &seen)
	descriptor, _ := rig.Registry.Descriptor(filesRead)
	source := &descriptor.Params[0]

	cases := []struct {
		name        string
		surface     operation.Surface
		description string
		summary     string
	}{
		{name: "the CLI", surface: operation.SurfaceCLI, description: "reads a file on this host", summary: "a file or an address"},
		{name: "chat", surface: operation.SurfaceChat, description: "reads a file or fetches an address", summary: "a file or an address"},
		{name: "the agent", surface: operation.SurfaceAgent, description: "reads a file or fetches an address", summary: "a workspace path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := descriptor.DescriptionOn(tc.surface); got != tc.description {
				t.Errorf("description = %q, want %q", got, tc.description)
			}
			if got := source.SummaryOn(tc.surface); got != tc.summary {
				t.Errorf("summary = %q, want %q", got, tc.summary)
			}
		})
	}
	schema, err := descriptor.SchemaFor(operation.SurfaceAgent)
	if err != nil || !strings.Contains(string(schema), "a workspace path") || strings.Contains(string(schema), "a file or an address") {
		t.Errorf("agent schema = %s, %v; want the agent summary of the path", schema, err)
	}
}
