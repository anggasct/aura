package connector

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func describeWithCapabilities(capabilities any, names ...string) map[string]any {
	reply := describeReply(names...)
	if capabilities != nil {
		reply["capabilities"] = capabilities
	}
	return reply
}

func kinds(payloads [][]byte) []string {
	out := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(payload, &head)
		out = append(out, head.Type)
	}
	return out
}

func TestEverySessionOpensWithDescribeAndTheHostCapabilities(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		starter := newStarter(func(index int) func(map[string]any) (any, error) {
			return func(in map[string]any) (any, error) {
				if index == 0 && kindOf(in) == "invoke" {
					return nil, errSessionGone
				}
				return resultReply(callOf(in), map[string]any{"session": index}), nil
			}
		})
		h := newTestHost(t, nil, starter, &bearerAuth{})
		if _, fail := h.describe(t.Context()); fail != nil {
			t.Fatalf("describe: %v", fail)
		}
		_, fail := invokeTool(t, h, readTool(h))
		mustCode(t, fail, ErrBackendUnavailable)
		time.Sleep(RestartInterval)
		if _, fail := invokeTool(t, h, readTool(h)); fail != nil {
			t.Fatalf("recovery call: %v", fail)
		}
		if _, fail := invokeTool(t, h, readTool(h)); fail != nil {
			t.Fatalf("second call on the new session: %v", fail)
		}
		if starter.started() != 2 {
			t.Fatalf("starts = %d, want 2", starter.started())
		}
		wantKinds := [][]string{{"describe", "invoke"}, {"describe", "invoke", "invoke"}}
		for index, want := range wantKinds {
			payloads := starter.session(index).payloads()
			if got := kinds(payloads); !slices.Equal(got, want) {
				t.Fatalf("session %d exchanged %v, want %v", index, got, want)
			}
			var opening map[string]any
			if err := decodeStrict(payloads[0], &opening); err != nil {
				t.Fatalf("decode: %v", err)
			}
			capabilities, present := opening["host_capabilities"].([]any)
			if opening["v"] != Version || !present || len(capabilities) != 0 {
				t.Fatalf("session %d opened with %v, want the version and an empty host_capabilities list", index, opening)
			}
		}
	})
}

func TestUnknownCapabilitiesAreIgnoredAndGrantNothing(t *testing.T) {
	cases := []struct {
		name         string
		capabilities any
	}{
		{"absent", nil},
		{"empty", []string{}},
		{"unknown names", []string{"future_feature", "x"}},
		{"the largest list", func() []string {
			names := make([]string, 32)
			for i := range names {
				names[i] = "cap_" + strconv.Itoa(i)
			}
			return names
		}()},
		{"the longest name", []string{"a" + strings.Repeat("b", 47)}},
		{"digits and underscores", []string{"v2_batch_0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			starter := newStarter(func(int) func(map[string]any) (any, error) {
				return func(in map[string]any) (any, error) {
					if kindOf(in) == "describe" {
						return describeWithCapabilities(tc.capabilities, "search", "send"), nil
					}
					return resultReply(callOf(in), map[string]any{"ok": true}), nil
				}
			})
			starter.scripted = true
			h := newTestHost(t, nil, starter, &bearerAuth{})
			if _, fail := invokeTool(t, h, readTool(h)); fail != nil {
				t.Fatalf("invoke: %v", fail)
			}
			if len(h.agreed) != 0 {
				t.Fatalf("agreed capabilities = %v, want none while the host declares none", h.agreed)
			}
		})
	}
}

func TestBadCapabilityListsFailTheSessionBeforeAnyCall(t *testing.T) {
	tooMany := make([]string, 33)
	for i := range tooMany {
		tooMany[i] = "cap_" + strconv.Itoa(i)
	}
	cases := []struct {
		name         string
		capabilities any
	}{
		{"too many names", tooMany},
		{"capital letters", []string{"Future"}},
		{"leading digit", []string{"1future"}},
		{"leading underscore", []string{"_future"}},
		{"a dash", []string{"future-feature"}},
		{"a space", []string{"future feature"}},
		{"an empty name", []string{""}},
		{"a name over 48 characters", []string{"a" + strings.Repeat("b", 48)}},
		{"a duplicate", []string{"future", "future"}},
		{"not a list", "future"},
		{"a list of numbers", []int{1}},
		{"null entries", []any{nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			starter := newStarter(func(int) func(map[string]any) (any, error) {
				return func(in map[string]any) (any, error) {
					if kindOf(in) == "describe" {
						return describeWithCapabilities(tc.capabilities, "search", "send"), nil
					}
					return resultReply(callOf(in), map[string]any{"ok": true}), nil
				}
			})
			starter.scripted = true
			h := newTestHost(t, nil, starter, &bearerAuth{})
			_, fail := invokeTool(t, h, readTool(h))
			mustCode(t, fail, ErrRPCInvalid)
			if fail.RequestSent {
				t.Fatal("a failed describe cannot have sent a request")
			}
			if got := kinds(starter.allPayloads()); !slices.Equal(got, []string{"describe"}) {
				t.Fatalf("exchanged %v, want only the describe", got)
			}
			if starter.openSessions() != 0 {
				t.Fatal("a session with a bad describe stayed open")
			}
		})
	}
}

func TestDescribeKeepsStrictDecodingForFieldsNobodyDeclared(t *testing.T) {
	starter := newStarter(func(int) func(map[string]any) (any, error) {
		return func(in map[string]any) (any, error) {
			if kindOf(in) == "describe" {
				reply := describeReply("search", "send")
				reply["future_field"] = true
				return reply, nil
			}
			return resultReply(callOf(in), map[string]any{"ok": true}), nil
		}
	})
	starter.scripted = true
	h := newTestHost(t, nil, starter, &bearerAuth{})
	_, fail := invokeTool(t, h, readTool(h))
	mustCode(t, fail, ErrRPCInvalid)
}

func TestCapabilitiesAreNegotiatedAgainOnARestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		starter := newStarter(func(index int) func(map[string]any) (any, error) {
			return func(in map[string]any) (any, error) {
				if kindOf(in) == "describe" {
					if index == 1 {
						return describeWithCapabilities([]string{"Broken"}, "search", "send"), nil
					}
					return describeWithCapabilities([]string{"future_feature"}, "search", "send"), nil
				}
				if index == 0 {
					return nil, errSessionGone
				}
				return resultReply(callOf(in), map[string]any{"ok": true}), nil
			}
		})
		starter.scripted = true
		h := newTestHost(t, nil, starter, &bearerAuth{})
		_, fail := invokeTool(t, h, readTool(h))
		mustCode(t, fail, ErrBackendUnavailable)
		time.Sleep(RestartInterval)
		_, fail = invokeTool(t, h, readTool(h))
		mustCode(t, fail, ErrRPCInvalid)
		if got := kinds(starter.session(1).payloads()); !slices.Equal(got, []string{"describe"}) {
			t.Fatalf("restarted session exchanged %v, want only a rejected describe", got)
		}
	})
}

func TestARestartedConnectorThatNoLongerDescribesTheToolFailsTheCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		starter := newStarter(func(index int) func(map[string]any) (any, error) {
			return func(in map[string]any) (any, error) {
				if kindOf(in) == "describe" {
					if index == 1 {
						return describeReply("search"), nil
					}
					return describeReply("search", "send"), nil
				}
				if index == 0 {
					return nil, errSessionGone
				}
				return resultReply(callOf(in), map[string]any{"ok": true}), nil
			}
		})
		starter.scripted = true
		h := newTestHost(t, nil, starter, &bearerAuth{})
		_, fail := invokeTool(t, h, effectTool(h))
		mustCode(t, fail, ErrBackendUnavailable)
		time.Sleep(RestartInterval)
		_, fail = invokeTool(t, h, effectTool(h))
		mustCode(t, fail, ErrToolDrift)
		if fail.RequestSent || starter.openSessions() != 0 {
			t.Fatalf("sent = %v, open sessions = %d", fail.RequestSent, starter.openSessions())
		}
		if got := kinds(starter.session(1).payloads()); !slices.Equal(got, []string{"describe"}) {
			t.Fatalf("exchanged %v, want no invoke on a drifted session", got)
		}
	})
}

func strictSchema(properties ...string) map[string]any {
	declared := map[string]any{}
	for _, name := range properties {
		declared[name] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": declared, "additionalProperties": false}
}

func describeWithSchemas(schemas map[string]any) map[string]any {
	described := make([]map[string]any, 0, len(schemas))
	for _, name := range []string{"search", "send"} {
		if schema, ok := schemas[name]; ok {
			described = append(described, map[string]any{"name": name, "description": "connector text for " + name, "input_schema": schema})
		}
	}
	return map[string]any{"v": Version, "type": "tools", "tools": described}
}

func TestToolsWithADisplayNeedAStrictSchemaOrStayHidden(t *testing.T) {
	loose := map[string]any{"type": "object", "properties": map[string]any{"to": map[string]any{"type": "string"}}}
	cases := []struct {
		name      string
		display   []string
		schema    any
		exposed   bool
		wantDrift []string
	}{
		{"strict schema naming every field", []string{"to", "subject", "body"}, strictSchema("to", "subject", "body", "cc"), true, []string{}},
		{"no display needs no strictness", nil, loose, true, []string{}},
		{"additional properties unset", []string{"to"}, loose, false, []string{"send"}},
		{"additional properties allowed", []string{"to"}, map[string]any{"type": "object", "properties": map[string]any{"to": map[string]any{}}, "additionalProperties": true}, false, []string{"send"}},
		{"display names an undeclared field", []string{"to", "bcc"}, strictSchema("to", "subject"), false, []string{"send"}},
		{"no schema at all", []string{"to"}, nil, false, []string{"send"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagerFixture(t, func(*fakeAPI) func(int) func(map[string]any) (any, error) {
				return func(int) func(map[string]any) (any, error) {
					return func(in map[string]any) (any, error) {
						if kindOf(in) == "describe" {
							schemas := map[string]any{"search": strictSchema("q")}
							if tc.schema != nil {
								schemas["send"] = tc.schema
							} else {
								schemas["send"] = map[string]any{}
							}
							return describeWithSchemas(schemas), nil
						}
						return resultReply(callOf(in), true), nil
					}
				}
			}, func(cfg *Config) { cfg.Tools[1].Display = tc.display })
			f.starter.scripted = true
			f.start()
			names := f.toolNames()
			if exposed := slices.Contains(names, "plugin_mailer_send"); exposed != tc.exposed {
				t.Fatalf("registered = %v, want send exposed = %v", names, tc.exposed)
			}
			statuses := f.manager.Statuses()
			if len(statuses) != 1 || statuses[0].Code != "" || !slices.Equal(append([]string{}, statuses[0].Drift...), tc.wantDrift) {
				t.Fatalf("statuses = %+v, want drift %v", statuses, tc.wantDrift)
			}
			if tc.exposed {
				for _, definition := range f.broker.Definitions() {
					if definition.Name == "plugin_mailer_send" && !slices.Equal(definition.Display, tc.display) {
						t.Fatalf("definition display = %v, want %v", definition.Display, tc.display)
					}
				}
			}
		})
	}
}

func TestAStrictSchemaThatLoosensOnRestartFailsTheCallBeforeAnyRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		starter := newStarter(func(index int) func(map[string]any) (any, error) {
			return func(in map[string]any) (any, error) {
				if kindOf(in) == "describe" {
					schema := strictSchema("to")
					if index == 1 {
						schema = map[string]any{"type": "object", "properties": map[string]any{"to": map[string]any{}}}
					}
					return describeWithSchemas(map[string]any{"search": strictSchema("q"), "send": schema}), nil
				}
				if index == 0 {
					return nil, errSessionGone
				}
				return resultReply(callOf(in), true), nil
			}
		})
		starter.scripted = true
		h := newTestHost(t, nil, starter, &bearerAuth{}, Tool{Name: "search", Description: "Search mail."}, Tool{Name: "send", Description: "Send mail.", Effect: true, Display: []string{"to"}})
		_, fail := invokeTool(t, h, effectTool(h))
		mustCode(t, fail, ErrBackendUnavailable)
		time.Sleep(RestartInterval)
		_, fail = invokeTool(t, h, effectTool(h))
		mustCode(t, fail, ErrToolDrift)
		if got := kinds(starter.session(1).payloads()); !slices.Equal(got, []string{"describe"}) {
			t.Fatalf("exchanged %v, want no invoke", got)
		}
	})
}

func TestNewManagerValidatesTheDisplayNames(t *testing.T) {
	cases := []struct {
		name    string
		display []string
		valid   bool
	}{
		{"none", nil, true},
		{"named fields", []string{"to", "body"}, true},
		{"a repeated name", []string{"to", "to"}, false},
		{"a path", []string{"message.to"}, false},
		{"an empty name", []string{""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			cfg := testConfig(api, &bearerAuth{})
			cfg.Tools[1].Display = tc.display
			f := newManagerFixture(t, getProgram("/x"), nil)
			_, err := NewManager(&ManagerOptions{Plugins: []Config{cfg}, Broker: f.broker, Starter: f.starter, HTTPClient: api.client(), Validator: func(json.RawMessage, int64) func(json.RawMessage) (json.RawMessage, error) { return nil }})
			if (err == nil) != tc.valid {
				t.Fatalf("NewManager error = %v, want valid = %v", err, tc.valid)
			}
		})
	}
}
