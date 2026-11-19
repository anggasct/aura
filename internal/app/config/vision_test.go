package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/vision"
)

func TestLoad_VisionDefaults(t *testing.T) {
	res, err := Load(writeTempConfig(t, "version: 1\nvision: {}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Vision == nil {
		t.Fatal("Vision is nil")
	}
	loaded := res.Config.Vision
	if !loaded.Enabled || !loaded.StripMetadata || !loaded.OrientNormalize {
		t.Errorf("vision flags = %+v", loaded)
	}
	if loaded.MaxEncodedBytes != 20971520 || loaded.MaxPixels != 40000000 || loaded.MaxDimension != 8192 {
		t.Errorf("vision bounds = %+v", loaded)
	}
	if loaded.MaxImages != 4 || loaded.MaxFrames != 1 || loaded.MaxDecodeConcurrency != 2 {
		t.Errorf("vision counts = %+v", loaded)
	}
	if loaded.MaxRequestBytes != 33554432 || loaded.MaxTransformMemory != 268435456 {
		t.Errorf("vision byte bounds = %+v", loaded)
	}
	if loaded.Detail != "auto" || loaded.TransformVersion != "v1" {
		t.Errorf("vision detail/version = %+v", loaded)
	}
}

func TestLoad_VisionOptionalForCoreProfile(t *testing.T) {
	res, err := Load(writeTempConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Vision != nil {
		t.Errorf("Vision = %+v, want nil for absent section under core profile", res.Config.Vision)
	}
}

func TestVisionRequiredForNonCoreProfile(t *testing.T) {
	base := "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/srv/aura/skills]\ncontext:\n  recent_complete_turns: 5\nprofile:\n  prompt_version: v1\nchildren:\n  recovery: interrupt\n"
	_, err := LoadWithOptions(writeTempConfig(t, base), execLinuxOptions(t))
	if err == nil || !strings.Contains(err.Error(), `vision: config_invalid: section is required for build profile "exec-linux"`) {
		t.Fatalf("LoadWithOptions error = %v", err)
	}
	wantCode(t, err, invalidCode)

	res, err := LoadWithOptions(writeTempConfig(t, base+"vision: {}\n"), execLinuxOptions(t))
	if err != nil {
		t.Fatalf("LoadWithOptions with vision section: %v", err)
	}
	if res.Config.Vision == nil {
		t.Fatal("Vision is nil")
	}
}

func TestLoad_VisionInvalid(t *testing.T) {
	cases := map[string]string{
		"frames":   "version: 1\nvision:\n  max_frames: 2\n",
		"detail":   "version: 1\nvision:\n  detail: ultra\n",
		"pixels":   "version: 1\nvision:\n  max_pixels: -1\n",
		"timeout":  "version: 1\nvision:\n  decode_timeout: 60s\n",
		"widening": "version: 1\nvision:\n  providers:\n    openai_responses:\n      max_images: 8\n",
		"unknown":  "version: 1\nvision:\n  providers:\n    chatty:\n      max_images: 1\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, content))
			if err == nil {
				t.Fatal("expected error")
			}
			wantCode(t, err, invalidCode)
		})
	}
}

func TestLoad_VisionShapeStrict(t *testing.T) {
	_, err := Load(writeTempConfig(t, "version: 1\nvision:\n  max_images: \"4\"\n"))
	requireProblemText(t, err, invalidText("vision.max_images", "line 3, column 15", "must be an integer"))
	_, err = Load(writeTempConfig(t, "version: 1\nvision:\n  no_such_key: true\n"))
	wantCode(t, err, unknownKeyCode)
	if !strings.Contains(err.Error(), unknownKeyText("vision.no_such_key", "line 3, column 3")) {
		t.Fatalf("error = %v, want unknown key rejection", err)
	}
}

func TestLoad_VisionEnvOverride(t *testing.T) {
	t.Setenv("AURA_VISION_MAX_IMAGES", "2")
	t.Setenv("AURA_VISION_DETAIL", "low")
	res, err := Load(writeTempConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Vision.MaxImages != 2 || res.Config.Vision.Detail != "low" {
		t.Errorf("vision = %+v", res.Config.Vision)
	}
}

func TestVisionProviderProblemsSkipLimitChecksForUnknownProtocols(t *testing.T) {
	outOfRange := vision.ProviderLimits{MaxImages: 99, MaxRequestBytes: -1, Detail: "ultra"}

	t.Run("unknown protocol", func(t *testing.T) {
		section := vision.Default()
		section.Providers = map[string]vision.ProviderLimits{"nosuch": outOfRange}
		got := problemTexts(visionProviderProblems(&section))
		want := []string{"vision.providers.nosuch: config_invalid: is not a known provider protocol"}
		if !slices.Equal(got, want) {
			t.Errorf("problems = %v, want %v", got, want)
		}
	})

	t.Run("known protocols in sorted order", func(t *testing.T) {
		section := vision.Default()
		section.Providers = map[string]vision.ProviderLimits{
			"openai_chat_compat": outOfRange,
			"nosuch":             outOfRange,
			"anthropic_messages": outOfRange,
		}
		got := problemTexts(visionProviderProblems(&section))
		want := []string{
			"vision.providers.anthropic_messages.max_images: config_invalid: must be between 0 and vision.max_images",
			"vision.providers.anthropic_messages.max_request_bytes: config_invalid: must be between 0 and vision.max_request_bytes",
			"vision.providers.anthropic_messages.detail: config_invalid: must be auto, low, or high",
			"vision.providers.nosuch: config_invalid: is not a known provider protocol",
			"vision.providers.openai_chat_compat.max_images: config_invalid: must be between 0 and vision.max_images",
			"vision.providers.openai_chat_compat.max_request_bytes: config_invalid: must be between 0 and vision.max_request_bytes",
			"vision.providers.openai_chat_compat.detail: config_invalid: must be auto, low, or high",
		}
		if !slices.Equal(got, want) {
			t.Errorf("problems = %v, want %v", got, want)
		}
	})
}

func problemTexts(err error) []string {
	var texts []string
	for _, problem := range fieldProblems(err) {
		texts = append(texts, problem.Error())
	}
	return texts
}
