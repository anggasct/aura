package toolbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	escapedSecret = `p&ss"w<rd`
	numericSecret = "123456"
	slashSecret   = "1//0gAb"
	goEscapedForm = `p\u0026ss\"w\u003crd`
)

var escapedSecretForms = []string{escapedSecret, `p&ss\"w<rd`, goEscapedForm}

type redactionCase struct {
	name    string
	secrets []string
	input   string
	want    string
}

func redactAsOutput(data []byte, secrets []string) []byte {
	return redactOutput(data, secrets)
}

func requireRedactions(t *testing.T, cases []redactionCase, apply func(data []byte, secrets []string) []byte) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(apply([]byte(tc.input), tc.secrets)); got != tc.want {
				t.Errorf("redaction of %s = %s, want %s", tc.input, got, tc.want)
			}
		})
	}
}

func everyFormCases() []redactionCase {
	return []redactionCase{
		{
			name:    "the value, its minimal form and its Go default form",
			secrets: []string{escapedSecret},
			input:   `x p&ss"w<rd y p&ss\"w<rd z p\u0026ss\"w\u003crd end`,
			want:    `x [REDACTED] y [REDACTED] z [REDACTED] end`,
		},
		{
			name:    "a value with four different forms",
			secrets: []string{`a/"b&c`},
			input:   `1 a/"b&c 2 a/\"b&c 3 a/\"b\u0026c 4 a\/\"b&c 5`,
			want:    `1 [REDACTED] 2 [REDACTED] 3 [REDACTED] 4 [REDACTED] 5`,
		},
		{
			name:    "escaped slashes",
			secrets: []string{slashSecret},
			input:   `t 1\/\/0gAb u`,
			want:    `t [REDACTED] u`,
		},
		{
			name:    "an empty value beside another one",
			secrets: []string{"", escapedSecret},
			input:   `nothing to hide in here`,
			want:    `nothing to hide in here`,
		},
	}
}

func overlappingCases() []redactionCase {
	return []redactionCase{
		{name: "a shorter value registered first", secrets: []string{"abc", "abcdef"}, input: "abcdef abc", want: "[REDACTED] [REDACTED]"},
		{name: "a longer value registered first", secrets: []string{"abcdef", "abc"}, input: "abcdef abc", want: "[REDACTED] [REDACTED]"},
		{name: "values that overlap at the start", secrets: []string{"abcd", "xab"}, input: "xabcd", want: "[REDACTED]"},
		{name: "values that overlap at the end", secrets: []string{"abc", "bcd"}, input: "abcd", want: "[REDACTED]"},
		{name: "occurrences next to each other", secrets: []string{"abc"}, input: "abcabc abc", want: "[REDACTED] [REDACTED]"},
		{name: "occurrences that overlap", secrets: []string{"aa"}, input: "aaa b", want: "[REDACTED] b"},
	}
}

func decodedStringCases() []redactionCase {
	secrets := []string{escapedSecret}
	return []redactionCase{
		{name: "Go default escapes", secrets: secrets, input: `{"echo":"p\u0026ss\"w\u003crd"}`, want: `{"echo":"[REDACTED]"}`},
		{name: "escapes that no form spells", secrets: secrets, input: `{"echo":"\u0070\u0026ss\"w\u003Crd"}`, want: `{"echo":"[REDACTED]"}`},
		{name: "an object key", secrets: secrets, input: `{"p\u0026ss\"w\u003crd":true}`, want: `{"[REDACTED]":true}`},
		{name: "a nested value", secrets: secrets, input: `{"a":[1,{"b":"p&ss\"w<rd"}]}`, want: `{"a":[1,{"b":"[REDACTED]"}]}`},
		{name: "inside a longer string", secrets: secrets, input: `{"a":"x < p\u0026ss\"w\u003crd \u00e9 y"}`, want: `{"a":"x < [REDACTED] é y"}`},
		{name: "two strings", secrets: secrets, input: `{"a":"p&ss\"w<rd","b":"k","c":"p\u0026ss\"w\u003crd"}`, want: `{"a":"[REDACTED]","b":"k","c":"[REDACTED]"}`},
		{name: "a string that ends in a backslash", secrets: secrets, input: `["C:\\dir\\","p&ss\"w<rd"]`, want: `["C:\\dir\\","[REDACTED]"]`},
		{name: "a string that ends in a backslash and a quote", secrets: secrets, input: `{"a":"x\\\"","b":"p&ss\"w<rd"}`, want: `{"a":"x\\\"","b":"[REDACTED]"}`},
	}
}

func jsonInsideStringCases() []redactionCase {
	return []redactionCase{
		{
			name:    "Go default escapes one level down",
			secrets: []string{escapedSecret},
			input:   `{"body":"{\"token\":\"p\\u0026ss\\\"w\\u003crd\"}"}`,
			want:    `{"body":"{\"token\":\"[REDACTED]\"}"}`,
		},
		{
			name:    "minimal escapes one level down",
			secrets: []string{escapedSecret},
			input:   `{"body":"{\"token\":\"p&ss\\\"w<rd\"}"}`,
			want:    `{"body":"{\"token\":\"[REDACTED]\"}"}`,
		},
		{
			name:    "escaped slashes one level down",
			secrets: []string{slashSecret},
			input:   `{"body":"{\"t\":\"1\\/\\/0gAb\"}"}`,
			want:    `{"body":"{\"t\":\"[REDACTED]\"}"}`,
		},
		{
			name:    "escaped slashes in the output's own strings",
			secrets: []string{slashSecret},
			input:   `{"k":"1\/\/0gAb","u":"https:\/\/x"}`,
			want:    `{"k":"[REDACTED]","u":"https:\/\/x"}`,
		},
	}
}

const untouchedOutput = `{ "a" : "caf\u00e9" ,  "n": 1.0e3, "n": 2, "u":"https:\/\/x" }`

func untouchedOutputCases() []redactionCase {
	return []redactionCase{
		{name: "nothing to redact", secrets: []string{escapedSecret}, input: untouchedOutput, want: untouchedOutput},
		{
			name:    "one string to redact",
			secrets: []string{escapedSecret},
			input:   `{ "a" : "caf\u00e9" ,  "n": 1.0e3, "n": 2, "u":"https:\/\/x", "s":"p&ss\"w<rd" }`,
			want:    `{ "a" : "caf\u00e9" ,  "n": 1.0e3, "n": 2, "u":"https:\/\/x", "s":"[REDACTED]" }`,
		},
		{name: "an empty value beside another one", secrets: []string{"", escapedSecret}, input: untouchedOutput, want: untouchedOutput},
	}
}

func failClosedCases() []redactionCase {
	return []redactionCase{
		{name: "a value inside a number", secrets: []string{numericSecret}, input: `{"pin":123456,"s":"k"}`, want: `"[REDACTED]"`},
		{name: "a value that spans tokens", secrets: []string{`a","b`}, input: `["a","b"]`, want: `"[REDACTED]"`},
	}
}

func invalidJSONCases() []redactionCase {
	return []redactionCase{
		{
			name:    "every form in a text",
			secrets: []string{escapedSecret},
			input:   `not json p&ss"w<rd and p\u0026ss\"w\u003crd`,
			want:    `not json [REDACTED] and [REDACTED]`,
		},
	}
}

func TestRedactFindsEveryFormOfASecret(t *testing.T) {
	requireRedactions(t, everyFormCases(), redact)
}

func TestRedactCoversOverlappingSecrets(t *testing.T) {
	requireRedactions(t, overlappingCases(), redact)
}

func TestRedactOutputDecodesEveryString(t *testing.T) {
	requireRedactions(t, decodedStringCases(), redactAsOutput)
}

func TestRedactOutputFindsASecretInJSONInsideAString(t *testing.T) {
	requireRedactions(t, jsonInsideStringCases(), redactAsOutput)
}

func TestRedactOutputLeavesTheRestOfTheOutputAsItIs(t *testing.T) {
	requireRedactions(t, untouchedOutputCases(), redactAsOutput)
	secrets := []string{escapedSecret}
	if got := redactOutput(nil, secrets); got != nil {
		t.Errorf("redaction of a nil output = %#v, want nil", got)
	}
	if got := redactOutput(json.RawMessage{}, secrets); got == nil || len(got) != 0 {
		t.Errorf("redaction of an empty output = %#v, want an empty one", got)
	}
}

func TestRedactOutputStaysValidJSON(t *testing.T) {
	requireRedactions(t, failClosedCases(), redactAsOutput)
	for _, tc := range slices.Concat(decodedStringCases(), jsonInsideStringCases(), untouchedOutputCases(), failClosedCases()) {
		if got := redactOutput(json.RawMessage(tc.input), tc.secrets); !json.Valid(got) {
			t.Errorf("%s: redaction of %s = %s, want valid JSON", tc.name, tc.input, got)
		}
	}
}

func TestRedactOutputOfInvalidJSONIsRedactedAsText(t *testing.T) {
	requireRedactions(t, invalidJSONCases(), redactAsOutput)
}

func TestRedactingTwiceChangesNothing(t *testing.T) {
	texts := slices.Concat(everyFormCases(), overlappingCases())
	outputs := slices.Concat(decodedStringCases(), jsonInsideStringCases(), failClosedCases(), invalidJSONCases())
	for _, set := range []struct {
		cases []redactionCase
		apply func(data []byte, secrets []string) []byte
	}{{cases: texts, apply: redact}, {cases: outputs, apply: redactAsOutput}} {
		for _, tc := range set.cases {
			once := set.apply([]byte(tc.input), tc.secrets)
			if twice := set.apply(once, tc.secrets); !bytes.Equal(twice, once) {
				t.Errorf("%s: a second redaction of %s = %s, want %s", tc.name, tc.input, twice, once)
			}
		}
	}
}

func newRedactingBroker(t *testing.T, adapter Adapter) *Broker {
	t.Helper()
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t), Effects: newBrokerEffectExecutor(t),
		Adapters: map[string]Adapter{"read_file@v1": adapter},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return broker
}

func respondWith(output json.RawMessage) Adapter {
	return func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{Output: output}, nil
	}
}

func goEncodedEcho(t *testing.T) json.RawMessage {
	t.Helper()
	output, err := json.Marshal(map[string]string{"echo": escapedSecret})
	if err != nil {
		t.Fatalf("encode the echo: %v", err)
	}
	if !strings.Contains(string(output), goEscapedForm) {
		t.Fatalf("encoded echo = %s, want the Go default escapes", output)
	}
	return output
}

func requireNoForm(t *testing.T, text string) {
	t.Helper()
	for _, form := range escapedSecretForms {
		if strings.Contains(text, form) {
			t.Errorf("%q holds the form %q", text, form)
		}
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Errorf("%q holds no [REDACTED]", text)
	}
}

func TestBrokerRedactsAnEscapedSecretFromAToolOutput(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		output json.RawMessage
		want   json.RawMessage
	}{
		{name: "an escaped value in a string", secret: escapedSecret, output: goEncodedEcho(t), want: json.RawMessage(`{"echo":"[REDACTED]"}`)},
		{name: "a value inside a number", secret: numericSecret, output: json.RawMessage(`{"pin":123456}`), want: json.RawMessage(`"[REDACTED]"`)},
		{name: "no output", secret: escapedSecret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := newRedactingBroker(t, respondWith(tc.output))
			broker.RegisterSecret(tc.secret)
			result, err := broker.Execute(context.Background(), brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read"))
			if err != nil || result.Class != ResultOK {
				t.Fatalf("result = %+v, err = %v, want class ok", result, err)
			}
			if string(result.Output) != string(tc.want) || (tc.want == nil) != (result.Output == nil) {
				t.Errorf("output = %#v, want %#v", result.Output, tc.want)
			}
		})
	}
}

func TestBrokerRedactsAnEscapedSecretFromAnErrorText(t *testing.T) {
	broker := newRedactingBroker(t, func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{}, errors.New(`upstream said {"error":"bad token p\u0026ss\"w\u003crd"}`)
	})
	broker.RegisterSecret(escapedSecret)
	_, err := broker.Execute(context.Background(), brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read"))
	if class := classOf(err); class != ResultExecutionFailed {
		t.Fatalf("class = %q, err = %v, want execution_failed", class, err)
	}
	requireNoForm(t, err.Error())
}

func TestApprovalDisplayRedactsAnEscapedSecret(t *testing.T) {
	h := newDisplayHarness(t, []string{"to"}, escapedSecret)
	if err := h.run(`{"meta":{"t":"p\u0026ss\"w\u003crd"}}`, principal.ClassTrusted); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	prompts := h.promptList()
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(prompts))
	}
	requireNoForm(t, prompts[0].Arguments)
}

func journaledOutput(t *testing.T, rows []actionRow) string {
	t.Helper()
	execution := rows[len(rows)-1]
	if execution.outcome != "result" {
		t.Fatalf("tool action outcome = %q (detail %q), want result", execution.outcome, execution.detail)
	}
	recorded, err := decodeOutcome(execution.result)
	if err != nil {
		t.Fatalf("tool action records the failure %v, want a result", err)
	}
	var result struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(recorded, &result); err != nil {
		t.Fatalf("decode the tool action: %v", err)
	}
	return string(result.Output)
}

func TestDurableToolResultWithAnEscapedSecretIsJournaledRedacted(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		output json.RawMessage
		want   string
	}{
		{name: "Go default escapes", secret: escapedSecret, output: goEncodedEcho(t), want: `{"echo":"[REDACTED]"}`},
		{name: "escapes that no form spells", secret: escapedSecret, output: json.RawMessage(`{"echo":"\u0070\u0026ss\"w\u003Crd"}`), want: `{"echo":"[REDACTED]"}`},
		{name: "a value inside a number", secret: numericSecret, output: json.RawMessage(`{"pin":123456}`), want: `"[REDACTED]"`},
	}
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := newProbeBroker(t)
			broker.RegisterSecret(tc.secret)
			registerProbe(t, broker, &probeTool{rule: probeRule("probe"), respond: respondWith(tc.output)})
			want := callOutcome{class: ResultOK, output: tc.want}
			var first, replay callOutcome
			key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				first = outcomeOf(broker.Execute(inTurn(ctx, inv), probeRequest("probe")))
				replay = outcomeOf(broker.Execute(inTurn(ctx, inv), probeRequest("probe")))
				return nil
			})
			if first != want {
				t.Errorf("first execution = %+v, want %+v", first, want)
			}
			if replay != want {
				t.Errorf("replay = %+v, want %+v", replay, want)
			}
			if inMemory := outcomeOf(broker.Execute(t.Context(), probeRequest("probe"))); inMemory != want {
				t.Errorf("in memory = %+v, want %+v", inMemory, want)
			}
			rows := hosted.actionRows(t, key)
			if got := journaledOutput(t, rows); got != tc.want {
				t.Errorf("journaled output = %s, want %s", got, tc.want)
			}
			forms := escapedSecretForms
			if tc.secret == numericSecret {
				forms = []string{numericSecret}
			}
			for _, form := range forms {
				requireNoRowHolds(t, rows, form)
			}
			requireEveryRowIsAResult(t, rows)
		})
	}
}
