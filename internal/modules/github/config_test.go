package github

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/secret/secrettest"
)

func TestConfigDefaultAndValidate(t *testing.T) {
	def := Default()
	if len(def.Credentials) != 0 {
		t.Fatalf("Default().Credentials = %+v, want empty", def.Credentials)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
	var nilCfg *Config
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil Validate() = %v", err)
	}
	if vals := nilCfg.SecretValues(); len(vals) != 0 {
		t.Fatalf("nil SecretValues() = %v", vals)
	}

	valid := Config{
		Credentials: []Credential{
			{Name: "ci", Ref: secrettest.Ref("env://GITHUB_CI_TOKEN")},
			{Name: "release_1", Ref: secrettest.Ref("file:///etc/aura/gh-release")},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid.Validate() = %v", err)
	}

	invalid := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "bad name",
			cfg:  Config{Credentials: []Credential{{Name: "BadName", Ref: secrettest.Ref("env://A")}}},
			want: "credentials[0].name",
		},
		{
			name: "duplicate name",
			cfg: Config{Credentials: []Credential{
				{Name: "ci", Ref: secrettest.Ref("env://A")},
				{Name: "ci", Ref: secrettest.Ref("env://B")},
			}},
			want: "credentials[1].name",
		},
		{
			name: "empty ref",
			cfg:  Config{Credentials: []Credential{{Name: "ci"}}},
			want: "credentials[0].ref",
		},
		{
			name: "duplicate ref",
			cfg: Config{Credentials: []Credential{
				{Name: "ci", Ref: secrettest.Ref("env://A")},
				{Name: "rel", Ref: secrettest.Ref("env://A")},
			}},
			want: "credentials[1].ref",
		},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestConfigSecretValuesResolvesConfiguredEnv(t *testing.T) {
	t.Setenv("AURA_GH_CFG_SECRET", "ghp_secret_value")
	cfg := Config{
		Credentials: []Credential{
			{Name: "ci", Ref: secrettest.Ref("env://AURA_GH_CFG_SECRET")},
			{Name: "missing", Ref: secrettest.Ref("env://AURA_GH_CFG_UNSET")},
		},
	}
	vals := cfg.SecretValues()
	if len(vals) != 1 || vals[0] != "ghp_secret_value" {
		t.Fatalf("SecretValues() = %v, want [ghp_secret_value]", vals)
	}
}
