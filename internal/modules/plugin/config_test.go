package plugin

import (
	"fmt"
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
)

func TestPluginIdentifiersRejectPathShapes(t *testing.T) {
	for _, id := range []string{"", "a", "Weather", "we\tather", "../x", "a/b", "a\\b", "weather\n", strings.Repeat("a", 49), "-weather", "1weather", "we_x"} {
		if auraagent.ValidPluginID(id) {
			t.Errorf("agent.ValidPluginID(%q) = true", id)
		}
	}
	for _, id := range []string{"ab", "weather", "mail-reader", strings.Repeat("a", 48)} {
		if !auraagent.ValidPluginID(id) {
			t.Errorf("agent.ValidPluginID(%q) = false", id)
		}
	}
	for _, version := range []string{"", "1", "1.0", "01.0.0", "1.0.0.0", "../1.0.0", "1.0.0/x", "v1.0.0", "1.0.0-", "1.0.0 ", strings.Repeat("1", 65)} {
		if ValidVersion(version) {
			t.Errorf("ValidVersion(%q) = true", version)
		}
	}
	for _, version := range []string{"0.0.0", "1.2.3", "10.20.30", "1.0.0-alpha.1", "1.0.0-rc.1+build.5", "1.0.0+20260101"} {
		if !ValidVersion(version) {
			t.Errorf("ValidVersion(%q) = false", version)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	var nilCfg *Config
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil Validate() = %v", err)
	}
	valid := &Config{Desired: []Desired{
		{ID: "weather", Version: "1.2.3", Enabled: true},
		{ID: "mail-reader", Version: "0.1.0-rc.1", Enabled: false},
	}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid Validate() = %v", err)
	}
	invalid := &Config{Desired: []Desired{
		{ID: "Weather", Version: "1.0"},
		{ID: "weather", Version: "1.0.0"},
		{ID: "weather", Version: "1.1.0"},
	}}
	err := invalid.Validate()
	if err == nil {
		t.Fatal("Validate() accepted invalid config")
	}
	for _, want := range []string{"desired[0].id", "desired[0].version", "twice"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %v, want mention of %q", err, want)
		}
	}
	tooMany := &Config{Desired: make([]Desired, MaxDesired+1)}
	for i := range tooMany.Desired {
		tooMany.Desired[i] = Desired{ID: fmt.Sprintf("p%d", i), Version: "1.0.0"}
	}
	if err := tooMany.Validate(); err == nil || !strings.Contains(err.Error(), "desired") {
		t.Fatalf("tooMany Validate() = %v", err)
	}
}
