package config

import (
	"strings"
	"testing"
)

func TestLoad_VisionDefaults(t *testing.T) {
	res, err := Load(writeTempConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Vision == nil {
		t.Fatal("Vision is nil")
	}
	vision := res.Config.Vision
	if !vision.Enabled || !vision.StripMetadata || !vision.OrientNormalize {
		t.Errorf("vision flags = %+v", vision)
	}
	if vision.MaxEncodedBytes != 20971520 || vision.MaxPixels != 40000000 || vision.MaxDimension != 8192 {
		t.Errorf("vision bounds = %+v", vision)
	}
	if vision.MaxImages != 4 || vision.MaxFrames != 1 || vision.MaxDecodeConcurrency != 2 {
		t.Errorf("vision counts = %+v", vision)
	}
	if vision.MaxRequestBytes != 33554432 || vision.MaxTransformMemory != 268435456 {
		t.Errorf("vision byte bounds = %+v", vision)
	}
	if vision.Detail != "auto" || vision.TransformVersion != "v1" {
		t.Errorf("vision detail/version = %+v", vision)
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
			wantCode(t, err, ErrorCodeConfigInvalid)
		})
	}
}

func TestLoad_VisionShapeStrict(t *testing.T) {
	_, err := Load(writeTempConfig(t, "version: 1\nvision:\n  max_images: \"4\"\n"))
	if err == nil || !strings.Contains(err.Error(), "vision.max_images must be an integer") {
		t.Fatalf("error = %v, want integer shape rejection", err)
	}
	_, err = Load(writeTempConfig(t, "version: 1\nvision:\n  no_such_key: true\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown key "vision.no_such_key"`) {
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
