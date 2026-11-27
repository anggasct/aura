package config

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/platform/configkit"
)

var positionSuffix = regexp.MustCompile(` \((line \d+, column \d+|env AURA_[A-Z0-9_]+)\)`)

func stripPositions(text string) string {
	return positionSuffix.ReplaceAllString(text, "")
}

func TestAMapOrListInTheFileReplacesItsDefault(t *testing.T) {
	t.Run("a map", func(t *testing.T) {
		res, err := Load(writeTempConfig(t, "version: 1\nmodels:\n  routing:\n    agent: primary\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := map[string]string{"agent": "primary"}; !reflect.DeepEqual(res.Config.Models.Routing, want) {
			t.Errorf("routing = %v, want %v", res.Config.Models.Routing, want)
		}
	})
	t.Run("a list", func(t *testing.T) {
		res, err := Load(writeTempConfig(t, "version: 1\nskills:\n  roots: [/x]\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := []string{"/x"}; !reflect.DeepEqual(res.Config.Skills.Roots, want) {
			t.Errorf("roots = %v, want %v", res.Config.Skills.Roots, want)
		}
	})
}

func TestEnvSetsALeafAndKeepsItsSiblings(t *testing.T) {
	t.Run("over the default routing", func(t *testing.T) {
		res, err := loadWithEnvironment(t, "version: 1\n", map[string]string{"AURA_MODELS_ROUTING_AGENT": "auxiliary"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := Default().Models.Routing
		want["agent"] = "auxiliary"
		if !reflect.DeepEqual(res.Config.Models.Routing, want) {
			t.Errorf("routing = %v, want %v", res.Config.Models.Routing, want)
		}
		if len(res.Warnings) != 0 {
			t.Errorf("warnings = %v, want none", res.Warnings)
		}
	})
	t.Run("over the routing of the file", func(t *testing.T) {
		res, err := loadWithEnvironment(t, "version: 1\nmodels:\n  routing:\n    agent: primary\n", map[string]string{"AURA_MODELS_ROUTING_TITLE": "primary"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := map[string]string{"agent": "primary", "title": "primary"}; !reflect.DeepEqual(res.Config.Models.Routing, want) {
			t.Errorf("routing = %v, want %v", res.Config.Models.Routing, want)
		}
	})
}

const modelDefinitionsFixture = `  definitions:
    primary:
      protocol: anthropic_messages
      model: claude-sonnet
      base_url: https://api.anthropic.com
      api_key_ref: env://ANTHROPIC_API_KEY
      capabilities:
        streaming: true
        tools: true
        structured_output: false
        vision: true
        audio: false
        reasoning: true
        context_tokens: 200000
        tokenizer: anthropic
        usage_reporting: true
`

func TestEnvSetRoutingUndefinedModelRejected(t *testing.T) {
	t.Run("a routed role the environment sets and the definitions lack fails the load", func(t *testing.T) {
		_, err := loadWithEnvironment(t, "version: 1\nmodels:\n"+modelDefinitionsFixture, map[string]string{"AURA_MODELS_ROUTING_SUMMARIZE": "auxiliary"})
		wantCode(t, err, invalidCode)
		if !strings.Contains(err.Error(), "models.routing.summarize (env AURA_MODELS_ROUTING_SUMMARIZE): config_invalid: references an undefined model") {
			t.Errorf("Load error = %v, want the undefined-model reference named at the environment variable", err)
		}
	})
	t.Run("a routed role the environment sets and the definitions define keeps loading", func(t *testing.T) {
		res, err := loadWithEnvironment(t, "version: 1\nmodels:\n"+modelDefinitionsFixture, map[string]string{"AURA_MODELS_ROUTING_SUMMARIZE": "primary"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := res.Config.Models.Routing["summarize"]; got != "primary" {
			t.Errorf("routing.summarize = %q, want primary", got)
		}
	})
}

func TestAnAbsentOptionalSectionStaysAbsent(t *testing.T) {
	t.Run("a file without sections", func(t *testing.T) {
		res, err := Load(writeTempConfig(t, "version: 1\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		cfg := res.Config
		if cfg.Tools != nil || cfg.Skills != nil || cfg.Vision != nil || cfg.Sync != nil {
			t.Errorf("tools, skills, vision and sync = %v, %v, %v, %v; want all nil", cfg.Tools, cfg.Skills, cfg.Vision, cfg.Sync)
		}
		defaults := Default()
		if !reflect.DeepEqual(cfg.Workflows, defaults.Workflows) || !reflect.DeepEqual(cfg.Access, defaults.Access) {
			t.Errorf("workflows and access = %+v, %+v; want the defaults", cfg.Workflows, cfg.Access)
		}
	})
	t.Run("a section the file writes", func(t *testing.T) {
		res, err := Load(writeTempConfig(t, "version: 1\nvision:\n  max_images: 2\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := Default().Vision
		want.MaxImages = 2
		if !reflect.DeepEqual(res.Config.Vision, want) {
			t.Errorf("vision = %+v, want %+v", res.Config.Vision, want)
		}
	})
	t.Run("a section only the environment writes", func(t *testing.T) {
		res, err := loadWithEnvironment(t, "version: 1\n", map[string]string{"AURA_VISION_MAX_IMAGES": "2"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := Default().Vision
		want.MaxImages = 2
		if !reflect.DeepEqual(res.Config.Vision, want) {
			t.Errorf("vision = %+v, want %+v", res.Config.Vision, want)
		}
		if res.Config.Tools != nil {
			t.Errorf("tools = %+v, want nil", res.Config.Tools)
		}
	})
}

func TestAnElementGetsItsDefaultsUnderItsKeys(t *testing.T) {
	t.Run("discord accounts", func(t *testing.T) {
		res, err := Load(writeTempConfig(t, "version: 1\nchannels:\n  discord:\n    accounts:\n      - name: home\n        bot_token_ref: env://DISCORD_HOME_TOKEN\n      - name: kuma\n        bot_token_ref: env://DISCORD_KUMA_TOKEN\n        accept_dms: false\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		accounts := res.Config.Channels.Discord.Accounts
		if len(accounts) != 2 || !accounts[0].AcceptDMs || accounts[1].AcceptDMs {
			t.Errorf("accounts = %+v, want accept_dms true for home and false for kuma", accounts)
		}
	})
	t.Run("email accounts", func(t *testing.T) {
		res, err := Load(writeTempConfig(t, emailConfig(emailOwner, "", emailAccountYAML)))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		imap := res.Config.Channels.Email.Accounts[0].IMAP
		if imap.Folder != email.DefaultFolder || imap.PollInterval != configkit.Duration(time.Minute) {
			t.Errorf("imap = %+v, want folder %s and poll interval 1m", imap, email.DefaultFolder)
		}
	})
}

func TestAnExplicitZeroIsKeptAndValidated(t *testing.T) {
	cases := map[string]struct {
		content string
		env     map[string]string
		want    string
	}{
		"a zero port":        {content: "version: 1\nserver:\n  port: 0\n", want: "server.port (line 3, column 9): config_invalid: 0 is out of range (1-65535)"},
		"an empty log level": {content: "version: 1\nlogging:\n  level: \"\"\n", want: "logging.level (line 3, column 10): config_invalid: must be one of debug, info, warn, error"},
		"a zero turn limit":  {content: "version: 1\nruntime:\n  max_active_turns: 0\n", want: "runtime.max_active_turns (line 3, column 21): config_invalid: must be positive"},
		"a zero from the environment": {
			content: "version: 1\n",
			env:     map[string]string{"AURA_RUNTIME_MAX_ACTIVE_TURNS": "0"},
			want:    "runtime.max_active_turns (env AURA_RUNTIME_MAX_ACTIVE_TURNS): config_invalid: must be positive",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadWithEnvironment(t, tc.content, tc.env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestMCPServerZeroLimitsKeepTheirDefaults(t *testing.T) {
	_, err := loadWithEnvironment(t, "version: 1\nmcp:\n  servers:\n    - name: local-files\n      transport: stdio\n      command: /usr/local/bin/files-mcp\n      startup_timeout: 0s\n      request_timeout: 0s\n      max_message_size: \"0\"\n", nil)
	if err == nil {
		t.Fatal("explicit zero limits accepted, want config_invalid")
	}
	for _, want := range []string{
		"mcp.servers[local-files].startup_timeout: config_invalid: must be positive",
		"mcp.servers[local-files].request_timeout: config_invalid: must be positive",
		"mcp.servers[local-files].max_message_size: config_invalid: must be positive",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load error = %v, want %q", err, want)
		}
	}
}
