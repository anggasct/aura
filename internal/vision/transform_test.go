package vision

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func gradientImage(width, height int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.NRGBA{R: uint8((x * 255) / max(width-1, 1)), G: uint8((y * 255) / max(height-1, 1)), B: 128, A: 255})
		}
	}
	return img
}

func TestPlanGeometryFitsWithoutResize(t *testing.T) {
	w, h, resized, err := PlanGeometry(1920, 1080, 40000000, 8192)
	if err != nil {
		t.Fatalf("PlanGeometry: %v", err)
	}
	if resized || w != 1920 || h != 1080 {
		t.Fatalf("PlanGeometry = %dx%d resized=%v, want 1920x1080 false", w, h, resized)
	}
}

func TestPlanGeometryDownscalesOnDimension(t *testing.T) {
	w, h, resized, err := PlanGeometry(9000, 100, 40000000, 8192)
	if err != nil {
		t.Fatalf("PlanGeometry: %v", err)
	}
	if !resized {
		t.Fatal("expected resize for over-dimension source")
	}
	if w != 8192 {
		t.Fatalf("width = %d, want 8192", w)
	}
	if h != 91 {
		t.Fatalf("height = %d, want 91", h)
	}
}

func TestPlanGeometryDownscalesOnPixels(t *testing.T) {
	w, h, resized, err := PlanGeometry(8000, 8000, 40000000, 8192)
	if err != nil {
		t.Fatalf("PlanGeometry: %v", err)
	}
	if !resized {
		t.Fatal("expected resize for over-pixel source")
	}
	if int64(w)*int64(h) > 40000000 {
		t.Fatalf("pixels %d exceed the bound", int64(w)*int64(h))
	}
	if w != h {
		t.Fatalf("square source must stay square, got %dx%d", w, h)
	}
	if w > 8000 || h > 8000 {
		t.Fatalf("plan must not upscale, got %dx%d", w, h)
	}
}

func TestPlanGeometryNeverUpscales(t *testing.T) {
	w, h, resized, err := PlanGeometry(16, 16, 40000000, 8192)
	if err != nil {
		t.Fatalf("PlanGeometry: %v", err)
	}
	if resized || w != 16 || h != 16 {
		t.Fatalf("PlanGeometry = %dx%d resized=%v, want identity", w, h, resized)
	}
}

func TestPlanGeometryRejectsInvalidInput(t *testing.T) {
	for _, tc := range [][4]int{{0, 10, 100, 100}, {10, 0, 100, 100}, {10, 10, 0, 100}, {10, 10, 100, 0}} {
		if _, _, _, err := PlanGeometry(tc[0], tc[1], int64(tc[2]), tc[3]); err == nil {
			t.Fatalf("PlanGeometry(%v) expected error", tc)
		}
	}
}

func TestTransformSatisfies(t *testing.T) {
	if !TransformSatisfies(1920, 1080, 40000000, 8192, "v1", "v1") {
		t.Fatal("in-bounds transform must satisfy")
	}
	if TransformSatisfies(9000, 100, 40000000, 8192, "v1", "v1") {
		t.Fatal("over-dimension transform must not satisfy")
	}
	if TransformSatisfies(1920, 1080, 1000, 8192, "v1", "v1") {
		t.Fatal("over-pixel transform must not satisfy")
	}
	if TransformSatisfies(1920, 1080, 40000000, 8192, "v2", "v1") {
		t.Fatal("version mismatch must not satisfy")
	}
}

func TestResizeLanczos3Deterministic(t *testing.T) {
	ctx := context.Background()
	src := gradientImage(64, 32)
	first, err := ResizeLanczos3(ctx, src, 32, 16, 1<<26)
	if err != nil {
		t.Fatalf("ResizeLanczos3: %v", err)
	}
	if first.Bounds().Dx() != 32 || first.Bounds().Dy() != 16 {
		t.Fatalf("dims = %v, want 32x16", first.Bounds())
	}
	second, err := ResizeLanczos3(ctx, src, 32, 16, 1<<26)
	if err != nil {
		t.Fatalf("ResizeLanczos3: %v", err)
	}
	var firstPNG, secondPNG bytes.Buffer
	if err := png.Encode(&firstPNG, first); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&secondPNG, second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstPNG.Bytes(), secondPNG.Bytes()) {
		t.Fatal("resize is not deterministic across runs")
	}
}

func TestResizeLanczos3PreservesSolidColor(t *testing.T) {
	ctx := context.Background()
	src := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			src.Set(x, y, color.NRGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	out, err := ResizeLanczos3(ctx, src, 8, 8, 1<<26)
	if err != nil {
		t.Fatalf("ResizeLanczos3: %v", err)
	}
	for y := range 8 {
		for x := range 8 {
			got := out.At(x, y)
			r, g, b, a := got.RGBA()
			if r>>8 != 200 || g>>8 != 100 || b>>8 != 50 || a>>8 != 255 {
				t.Fatalf("pixel (%d,%d) drifted: %v", x, y, got)
			}
		}
	}
}

func TestResizeLanczos3RejectsUpscale(t *testing.T) {
	if _, err := ResizeLanczos3(context.Background(), gradientImage(8, 8), 16, 16, 1<<26); err == nil {
		t.Fatal("expected upscale rejection")
	}
}

func TestResizeLanczos3EnforcesMemoryBound(t *testing.T) {
	if _, err := ResizeLanczos3(context.Background(), gradientImage(64, 64), 32, 32, 100); err == nil {
		t.Fatal("expected memory bound rejection")
	}
}

func TestResizeLanczos3HonorsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ResizeLanczos3(ctx, gradientImage(512, 512), 256, 256, 1<<28); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestResizeLanczos3RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	var missing context.Context
	if _, err := ResizeLanczos3(missing, gradientImage(8, 8), 4, 4, 1<<20); err == nil {
		t.Fatal("expected nil context rejection")
	}
	if _, err := ResizeLanczos3(ctx, nil, 4, 4, 1<<20); err == nil {
		t.Fatal("expected nil source rejection")
	}
	if _, err := ResizeLanczos3(ctx, gradientImage(8, 8), 0, 4, 1<<20); err == nil {
		t.Fatal("expected invalid target rejection")
	}
}
