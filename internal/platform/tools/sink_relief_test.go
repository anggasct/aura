package tools

import (
	"encoding/json"
	"testing"
)

func reliefDefinition() Definition {
	return Definition{
		Name: "web_fetch", Version: Version, ExternalSink: true,
		Schema:     json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"note":{"type":"string"}},"required":["url"]}`),
		SinkRelief: &SinkRelief{Kind: SinkReliefLinkProvenance, Argument: "url"},
	}
}

func TestSinkReliefRegistrationRules(t *testing.T) {
	cases := map[string]struct {
		change func(*Definition)
		valid  bool
	}{
		"a built-in sink with a required argument": {func(*Definition) {}, true},
		"a bounded query with its bound": {func(d *Definition) {
			d.SinkRelief = &SinkRelief{Kind: SinkReliefBoundedQuery, Argument: "url", MaxBytes: 256}
		}, true},
		"a bounded query without a bound": {func(d *Definition) {
			d.SinkRelief = &SinkRelief{Kind: SinkReliefBoundedQuery, Argument: "url"}
		}, false},
		"link provenance with a bound": {func(d *Definition) {
			d.SinkRelief = &SinkRelief{Kind: SinkReliefLinkProvenance, Argument: "url", MaxBytes: 10}
		}, false},
		"an unknown kind":               {func(d *Definition) { d.SinkRelief.Kind = "everything" }, false},
		"a tool that is no sink":        {func(d *Definition) { d.ExternalSink = false }, false},
		"a server tool":                 {func(d *Definition) { d.Server = "docs" }, false},
		"a plugin tool":                 {func(d *Definition) { d.Plugin = &PluginRef{} }, false},
		"an operation tool":             {func(d *Definition) { d.Operation = "cron.add" }, false},
		"an optional argument":          {func(d *Definition) { d.SinkRelief.Argument = "note" }, false},
		"an argument not in the schema": {func(d *Definition) { d.SinkRelief.Argument = "other" }, false},
		"no argument":                   {func(d *Definition) { d.SinkRelief.Argument = "" }, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			definition := reliefDefinition()
			tc.change(&definition)
			err := definition.validateSinkRelief()
			if tc.valid != (err == nil) {
				t.Errorf("validateSinkRelief = %v, want valid=%t", err, tc.valid)
			}
		})
	}
}
